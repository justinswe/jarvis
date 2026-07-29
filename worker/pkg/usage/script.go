package usage

import "github.com/valkey-io/valkey-go"

// Decoded positions in the admission script reply.
const (
	allowIndexAdmitted = 0
	allowIndexRetryMS  = 1
	allowIndexDenyKind = 2
	allowIndexUtilized = 3
	allowReplyLength   = 4
)

// Denial kinds reported by the admission script.
const (
	denyKindNone   = 0
	denyKindRate   = 1
	denyKindTokens = 2
)

// allowScript admits one request, records it, and reports limit utilization.
var allowScript = valkey.NewLuaScript(allowScriptBody)

// allowScriptBody builds its key and field names by concatenation rather than taking them
// as arguments, because they must come from the server's own TIME rather than a client
// clock. That duplicates the schema keys.go states in Go, so
// TestScriptsBuildTheSameNamesAsKeysGo asserts the two still agree.
//
// KEYS[1] is the GCRA key, which pins the cluster slot. Every other key is derived in
// Lua from ARGV[1]; this is safe only because all of them carry the same hash tag.
//
// ARGV: base, tier, requestsPerSecond, burst, tokensPerHour, requestTTL
// Returns: {admitted, retryAfterMillis, denyKind, utilizationPercent}
const allowScriptBody = `
local base   = ARGV[1]
local tier   = ARGV[2]
local rps    = tonumber(ARGV[3])
local burst  = tonumber(ARGV[4])
local tokens = tonumber(ARGV[5])
local ttl    = tonumber(ARGV[6])

local now    = redis.call('TIME')
local now_ms = now[1] * 1000 + math.floor(now[2] / 1000)
local now_s  = tonumber(now[1])
local minute = math.floor(now_s / 60)
local sec    = now_s % 60
local hour   = math.floor(now_s / 3600)

local reqKey = base .. ':req:' .. minute
local denKey = base .. ':den:' .. minute
local util   = 0

local function deny(retry, kind)
  redis.call('HINCRBY', denKey, sec, 1)
  redis.call('HSET', denKey, '_tier', tier)
  redis.call('EXPIRE', denKey, ttl)
  return {0, retry, kind, 100}
end

-- Token budget is checked first so an over-budget request never consumes rate budget.
if tokens > 0 then
  local used = tonumber(redis.call('HGET', base .. ':tok:' .. hour, '*|total')) or 0
  util = math.max(util, math.floor(used * 100 / tokens))
  if used >= tokens then
    return deny((3600 - (now_s % 3600)) * 1000, 2)
  end
end

if rps > 0 then
  local interval  = 1000.0 / rps
  local tolerance = (math.max(burst, 1) - 1) * interval
  local tat       = math.max(tonumber(redis.call('GET', KEYS[1])) or now_ms, now_ms)
  if now_ms < tat - tolerance then
    return deny(math.ceil(tat - tolerance - now_ms), 1)
  end
  local nextTat = tat + interval
  redis.call('SET', KEYS[1], nextTat, 'PX', math.ceil(interval + tolerance + 60000))
  util = math.max(util, math.floor((nextTat - now_ms) * 100 / (tolerance + interval)))
end

redis.call('HINCRBY', reqKey, sec, 1)
redis.call('HSET', reqKey, '_tier', tier)
redis.call('EXPIRE', reqKey, ttl)
return {1, 0, 0, math.min(util, 100)}
`

// flushScript applies one guild's accumulated token deltas to its current hour bucket.
var flushScript = valkey.NewLuaScript(flushScriptBody)

// flushScriptBody derives its bucket name from server TIME for the same reason
// allowScriptBody does.
//
// KEYS[1] is the GCRA key, used only to pin the slot. ARGV: base, tier, tokenTTL, then
// repeating (model, metric, delta) triples.
const flushScriptBody = `
local base = ARGV[1]
local tier = ARGV[2]
local ttl  = tonumber(ARGV[3])

local now    = redis.call('TIME')
local hour   = math.floor(tonumber(now[1]) / 3600)
local tokKey = base .. ':tok:' .. hour

for index = 4, #ARGV, 3 do
  local model  = ARGV[index]
  local metric = ARGV[index + 1]
  local delta  = tonumber(ARGV[index + 2])
  redis.call('HINCRBY', tokKey, model .. '|' .. metric, delta)
  if model ~= '*' then
    redis.call('HINCRBY', tokKey, '*|' .. metric, delta)
  end
end

redis.call('HSET', tokKey, '_tier', tier)
redis.call('EXPIRE', tokKey, ttl)
return 1
`
