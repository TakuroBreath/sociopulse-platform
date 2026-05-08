-- leak_bucket.lua — atomic token-bucket rate limiter.
--
-- Per-tenant throttle for the RDD generator (Plan 10 Task 4). Every
-- Generate iteration calls Allow(tenant); the script consumes one token
-- on success and 0 on throttle. Refill is continuous and deterministic
-- — the script computes the wall-clock-driven refill at every call.
--
-- KEYS[1] = bucket hash key, e.g. "rdd:leakbucket:<tenant_id>"
-- ARGV[1] = capacity  (positive integer; max tokens the bucket holds)
-- ARGV[2] = rate      (positive integer; tokens added per second)
-- ARGV[3] = now_ms    (unix milliseconds, supplied by the caller to
--                      keep the script clock-agnostic — tests freeze
--                      the clock and assert on observable state)
-- ARGV[4] = ttl_secs  (positive integer; key TTL refreshed on every
--                      successful update so idle tenants drop out of
--                      Redis without the keyspace growing forever)
--
-- Returns:
--   1 — token consumed; caller proceeds.
--   0 — bucket empty; caller surfaces ErrThrottled / buckets as throttled.
--
-- Hash fields: tokens (current count), updated_ms (last refill ts).

local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])
local ttl      = tonumber(ARGV[4])

local data = redis.call("HMGET", KEYS[1], "tokens", "updated_ms")
local tokens     = tonumber(data[1])
local updated_ms = tonumber(data[2])

if tokens == nil or updated_ms == nil then
    -- First touch: fully filled bucket.
    tokens = capacity
    updated_ms = now_ms
end

-- Refill — fractional tokens accumulate over the elapsed window.
-- Convert ms→seconds by dividing once; rate is whole-number tokens/sec.
local elapsed = now_ms - updated_ms
if elapsed < 0 then
    -- Defensive: clock-skew across redis vs caller. Treat as zero
    -- elapsed so we never give a negative token bonus.
    elapsed = 0
end
local refill = (elapsed * rate) / 1000.0
tokens = math.min(capacity, tokens + refill)

local allowed = 0
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
end

redis.call("HMSET", KEYS[1], "tokens", tostring(tokens), "updated_ms", tostring(now_ms))
redis.call("EXPIRE", KEYS[1], ttl)

return allowed
