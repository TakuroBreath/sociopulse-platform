-- transition.lua — atomic CAS write of the operator FSM hash.
--
-- KEYS[1] = op:<tenant>:user:<operator>
-- ARGV[1] = expected_version (string-encoded int — must match HGET key,version)
-- ARGV[2] = JSON object of fields to write (empty-string values trigger HDEL)
-- ARGV[3] = TTL in seconds (refreshed on every successful write)
--
-- Returns:
--   1  on success — fields were written, version incremented by 1, TTL refreshed.
--   -1 on optimistic-concurrency mismatch (caller should refetch and retry).
--
-- Atomicity: Redis serialises script execution per instance, so the
-- HGET / compare / HSET sequence cannot race with another transition on
-- the same key. The single-key shape keeps the script Cluster-safe — every
-- operation hashes to the same slot.
local key = KEYS[1]
local expected = tonumber(ARGV[1])
local payload = cjson.decode(ARGV[2])
local ttl = tonumber(ARGV[3])

local cur = tonumber(redis.call("HGET", key, "version") or "0")
if cur ~= expected then
    return -1
end

for k, v in pairs(payload) do
    if v == nil or v == "" then
        redis.call("HDEL", key, k)
    else
        redis.call("HSET", key, k, v)
    end
end
redis.call("HINCRBY", key, "version", 1)
redis.call("EXPIRE", key, ttl)
return 1
