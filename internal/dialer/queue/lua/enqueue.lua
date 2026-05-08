-- enqueue.lua — atomic SADD-then-ZADD for the call queue.
--
-- KEYS[1] = q:<tenant>:project:<project>      (sorted set; members = JSON QueueItem)
-- KEYS[2] = qd:<tenant>:project:<project>     (set; dedup by respondent_uuid)
-- ARGV[1] = respondent_uuid (string; cheap dedup key)
-- ARGV[2] = score (string-encoded float64 = priority*1e9 + epoch_ms)
-- ARGV[3] = item_blob (JSON-encoded QueueItem written verbatim into the ZSET)
-- ARGV[4] = TTL in seconds (refreshed on every successful enqueue)
--
-- Returns:
--   1 — enqueued (SADD was a fresh add); ZADD wrote the new member; TTLs refreshed.
--   0 — duplicate; the respondent_uuid is already in the dedup SET, no write.
--
-- Atomicity: Redis serialises script execution per instance, so the SADD /
-- compare / ZADD sequence cannot race with another enqueue on the same key
-- pair. EXPIRE on both keys keeps the pair coherent — they age out together.
local r = redis.call("SADD", KEYS[2], ARGV[1])
if r == 0 then
    return 0
end
redis.call("ZADD", KEYS[1], ARGV[2], ARGV[3])
redis.call("EXPIRE", KEYS[1], ARGV[4])
redis.call("EXPIRE", KEYS[2], ARGV[4])
return 1
