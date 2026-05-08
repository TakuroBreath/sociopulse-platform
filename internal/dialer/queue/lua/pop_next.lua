-- pop_next.lua — atomic ZPOPMIN-then-SREM for the call queue.
--
-- KEYS[1] = q:<tenant>:project:<project>      (sorted set)
-- KEYS[2] = qd:<tenant>:project:<project>     (dedup set)
--
-- Returns:
--   ""        — the queue is empty; caller surfaces api.ErrQueueEmpty.
--   <blob>    — the JSON-encoded QueueItem of the popped item. Caller decodes
--               in Go to recover all fields (respondent_id, priority, etc.).
--
-- The script decodes the popped JSON inside Lua only to recover the
-- respondent_id field for the SREM call against the dedup set. The decoded
-- value is NOT returned to the caller — the original JSON bytes are
-- returned verbatim so the Go decoder remains the single source of truth
-- for QueueItem field semantics.
--
-- ZPOPMIN with COUNT=1 is the atomic "remove and return lowest score"
-- operation. Combined with the SREM in the same Lua call, the whole
-- pop-with-dedup-cleanup is one indivisible step — N concurrent workers
-- racing on the same queue all see distinct items.
local popped = redis.call("ZPOPMIN", KEYS[1], 1)
if #popped == 0 then
    return ""
end
local member = popped[1]
local item = cjson.decode(member)
if item ~= nil and item.respondent_id ~= nil then
    redis.call("SREM", KEYS[2], item.respondent_id)
end
return member
