-- start_shift.lua — atomic, idempotent transition from offline → ready.
--
-- KEYS[1] = op:<tenant>:user:<operator>
-- ARGV[1] = JSON object of fields to set (state=ready, tenant_id, session_id,
--           project_id, state_entered_at, heartbeat_at)
-- ARGV[2] = TTL in seconds
--
-- Returns:
--    1 on success — created the hash with state=ready, version=1.
--    0 if the hash exists and is already ready (idempotent replay; no write).
--   -1 if the hash exists in any non-offline / non-ready state — the caller
--      should refetch and surface ErrInvalidTransition (operator already
--      mid-shift in another state).
--
-- The script accepts both "missing key" and "key with state=offline" as
-- valid starting points so a Force-offline operator can be re-started
-- cleanly.
local key = KEYS[1]
local payload = cjson.decode(ARGV[1])
local ttl = tonumber(ARGV[2])

local cur_state = redis.call("HGET", key, "state")

-- Already ready: idempotent replay — keep the existing hash untouched.
-- (The session_id stays bound to the original PG session row.)
if cur_state == "ready" then
    return 0
end

-- Any other non-offline state means the operator is mid-shift in
-- pause/dialing/call/status/verify. The caller cannot StartShift from
-- there — they must EndShift first.
if cur_state ~= nil and cur_state ~= false and cur_state ~= "offline" then
    return -1
end

-- Either the key is missing OR it is in offline state. Either way, write
-- the fresh ready snapshot at version=1.
for k, v in pairs(payload) do
    if v ~= nil and v ~= "" then
        redis.call("HSET", key, k, v)
    end
end
redis.call("HSET", key, "version", 1)
redis.call("EXPIRE", key, ttl)
return 1
