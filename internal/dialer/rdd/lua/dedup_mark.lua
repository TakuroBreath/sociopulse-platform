-- dedup_mark.lua — atomic SADD + EXPIRE for the dedup tier.
--
-- Plan 10 Task 4 dedup uses a per-tenant Redis SET ("rdd:seen:<tenant>")
-- with a 30-day TTL refreshed on every Mark. This script collapses the
-- (SADD member, EXPIRE ttl) pair into a single round-trip — atomic from
-- the Redis side and one-RTT cheaper than the equivalent TxPipeline.
--
-- KEYS[1] = qd-set key, e.g. "rdd:seen:<tenant_id>"
-- ARGV[1..N-1] = phone members to add (one or more E.164 strings)
-- ARGV[N]      = ttl in seconds (positive integer; refreshed on every call)
--
-- Returns the SADD count (members newly added — duplicates SADD-skipped).

local n = #ARGV
if n < 2 then
    return redis.error_reply("dedup_mark: need at least one member and a ttl")
end

local ttl = tonumber(ARGV[n])
if not ttl or ttl <= 0 then
    return redis.error_reply("dedup_mark: ttl must be a positive integer")
end

local members = {}
for i = 1, n - 1 do
    members[i] = ARGV[i]
end

local added = redis.call("SADD", KEYS[1], unpack(members))
redis.call("EXPIRE", KEYS[1], ttl)
return added
