-- requeue.lua — atomic re-insert of an item already popped (retry / no-answer).
--
-- KEYS[1] = q:<tenant>:project:<project>      (sorted set)
-- KEYS[2] = qd:<tenant>:project:<project>     (dedup set)
-- ARGV[1] = respondent_uuid (string)
-- ARGV[2] = score (string-encoded float64 = priority*1e9 + epoch_ms)
-- ARGV[3] = item_blob (JSON-encoded QueueItem; new score lives in the score
--           ARGV, the JSON carries Priority+EnqueuedAt+AttemptN)
-- ARGV[4] = TTL in seconds
--
-- Returns: always 1.
--
-- Invariant: the dedup SET should already contain ARGV[1] from the
-- corresponding pop_next call (which SREMs on success). If a Remove call
-- raced between pop and requeue, the SET no longer contains the entry —
-- the SADD restores it idempotently. The queue self-heals.
--
-- Caller responsibilities:
--   - Cap Priority at 9 BEFORE building the score and the item_blob — the
--     Lua side does not enforce the cap; Go is the only writer, the cap
--     lives there to keep the float arithmetic well within ±2^53.
--   - Compute the new score from the post-delay enqueue time so the item
--     surfaces only after the delay window passes.
redis.call("SADD", KEYS[2], ARGV[1])
redis.call("ZADD", KEYS[1], ARGV[2], ARGV[3])
redis.call("EXPIRE", KEYS[1], ARGV[4])
redis.call("EXPIRE", KEYS[2], ARGV[4])
return 1
