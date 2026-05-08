-- remove.lua — atomic eviction of a respondent from both ZSET and dedup SET.
--
-- KEYS[1] = q:<tenant>:project:<project>      (sorted set)
-- KEYS[2] = qd:<tenant>:project:<project>     (dedup set)
-- ARGV[1] = respondent_uuid (string)
--
-- Returns: always 1 (the operation is idempotent — removing a missing
-- entry is fine; SREM/ZREM are no-ops when the value is absent).
--
-- Implementation strategy: ZRANGE 0 -1 returns all members; we cjson.decode
-- each and ZREM the one whose respondent_id matches. This is O(N) where N
-- is the queue size; for production-realistic queues (<= ~1000 items per
-- project) the linear scan is acceptable. The package doc.go notes a
-- future O(1) optimisation: store a respondent_id → score hash so Remove
-- can ZREM by score directly. v1 keeps the simpler shape.
local items = redis.call("ZRANGE", KEYS[1], 0, -1)
for _, m in ipairs(items) do
    local it = cjson.decode(m)
    if it ~= nil and it.respondent_id == ARGV[1] then
        redis.call("ZREM", KEYS[1], m)
        break
    end
end
redis.call("SREM", KEYS[2], ARGV[1])
return 1
