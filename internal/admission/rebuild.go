package admission

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrRebuildConflict  = errors.New("Redis generation belongs to another rebuild or has incompatible data")
	ErrSnapshotMismatch = errors.New("Redis admission snapshot does not match MySQL")
	ErrSnapshotTooLarge = errors.New("admission snapshot exceeds atomic rebuild limit")
)

// AtomicRebuildOwnerLimit bounds one administrative Lua invocation. Larger
// activities need a chunked, sealed publisher; they fail closed, never truncate.
const AtomicRebuildOwnerLimit = 10000

type CommittedOwner struct {
	UserID    uint64
	RequestID string
}

type RebuildSnapshot struct {
	ActivityID uint64
	Generation uint64
	StockHint  uint64
	StartsAt   time.Time
	EndsAt     time.Time
	Owners     []CommittedOwner
}

// InstallRebuild is administrative only. Its token is the durable MySQL task
// identity. It may replace an incomplete, CLOSED installation of that same
// task, but can never overwrite an OPEN generation, even on a delayed retry.
func (s *Store) InstallRebuild(ctx context.Context, snapshot RebuildSnapshot, token string) error {
	return s.runRebuild(ctx, snapshot, token, installRebuildLua)
}

func (s *Store) VerifyRebuild(ctx context.Context, snapshot RebuildSnapshot, token string) error {
	return s.runRebuild(ctx, snapshot, token, verifyRebuildLua)
}

// PublishRebuild verifies the complete snapshot and opens Redis atomically.
// MySQL must still be fenced; the recovery service opens MySQL afterwards.
func (s *Store) PublishRebuild(ctx context.Context, snapshot RebuildSnapshot, token string) error {
	return s.runRebuild(ctx, snapshot, token, publishRebuildLua)
}

func (s *Store) runRebuild(ctx context.Context, snapshot RebuildSnapshot, token, script string) error {
	args, err := rebuildArgs(snapshot, token)
	if err != nil {
		return err
	}
	keys := append(s.activityKeys(snapshot.ActivityID, snapshot.Generation), s.activityKeys(snapshot.ActivityID, snapshot.Generation)[0]+":rebuild")
	value, err := redis.NewScript(script).Run(ctx, s.client, keys, args...).Text()
	if err != nil {
		return fmt.Errorf("run admission rebuild: %w", err)
	}
	switch value {
	case "INSTALLED", "VERIFIED", "PUBLISHED":
		return nil
	case "CONFLICT":
		return ErrRebuildConflict
	case "MISMATCH":
		return ErrSnapshotMismatch
	default:
		return fmt.Errorf("%w: rebuild %q", ErrUnexpectedScriptResult, value)
	}
}

func rebuildArgs(snapshot RebuildSnapshot, token string) ([]any, error) {
	if snapshot.ActivityID == 0 || snapshot.Generation == 0 || snapshot.StockHint > math.MaxInt64 || !validWindow(snapshot.StartsAt, snapshot.EndsAt) || strings.TrimSpace(token) == "" || len(token) > 128 {
		return nil, ErrInvalidRequest
	}
	if len(snapshot.Owners) > AtomicRebuildOwnerLimit {
		return nil, ErrSnapshotTooLarge
	}
	args := []any{token, strconv.FormatUint(snapshot.StockHint, 10), strconv.Itoa(len(snapshot.Owners)), strconv.FormatUint(snapshot.Generation, 10)}
	args = append(args, timeArgs(snapshot.StartsAt)...)
	args = append(args, timeArgs(snapshot.EndsAt)...)
	users := make(map[uint64]bool, len(snapshot.Owners))
	requests := make(map[string]bool, len(snapshot.Owners))
	for _, owner := range snapshot.Owners {
		if owner.UserID == 0 || strings.TrimSpace(owner.RequestID) == "" || owner.RequestID == "__initialized" || len(owner.RequestID) > 128 || users[owner.UserID] || requests[owner.RequestID] {
			return nil, ErrInvalidRequest
		}
		users[owner.UserID] = true
		requests[owner.RequestID] = true
		args = append(args, strconv.FormatUint(owner.UserID, 10), owner.RequestID)
	}
	return args, nil
}

const verifyRebuildBody = stateLua + `
local owner = redis.call('GET', KEYS[7])
if owner ~= ARGV[1] and owner ~= ARGV[1] .. ':published' then return 'CONFLICT' end
if state_status(ARGV[4]) then return 'MISMATCH' end
local fields = {'starts_sec', 'starts_usec', 'ends_sec', 'ends_usec'}
for i = 1, 4 do
  if redis.call('HGET', KEYS[6], fields[i]) ~= ARGV[i + 4] then return 'MISMATCH' end
end
if redis.call('GET', KEYS[1]) ~= ARGV[2] then return 'MISMATCH' end
local open = redis.call('GET', KEYS[2])
if open ~= '0' and open ~= '1' then return 'MISMATCH' end
if redis.call('HGET', KEYS[5], '__initialized') ~= '1' or redis.call('HLEN', KEYS[5]) ~= 1 then return 'MISMATCH' end
local count = tonumber(ARGV[3])
if redis.call('HLEN', KEYS[3]) ~= count + 1 or redis.call('HLEN', KEYS[4]) ~= count + 1 then return 'MISMATCH' end
for i = 1, count do
  local user = ARGV[2 * i + 7]
  local request = ARGV[2 * i + 8]
  if redis.call('HGET', KEYS[3], user) ~= 'S:' .. request then return 'MISMATCH' end
  if redis.call('HGET', KEYS[4], request) ~= 'S' then return 'MISMATCH' end
end
`

const verifyRebuildLua = verifyRebuildBody + `return 'VERIFIED'`
const publishRebuildLua = verifyRebuildBody + `
redis.call('SET', KEYS[2], '1')
redis.call('SET', KEYS[7], ARGV[1] .. ':published')
return 'PUBLISHED'`

const installRebuildLua = stateLua + `
local owner = redis.call('GET', KEYS[7])
local open = redis.call('GET', KEYS[2])
if owner == ARGV[1] .. ':published' or (owner == ARGV[1] and open == '1') then
  -- Already published stock/owners may have changed through normal traffic.
  -- Verify immutable metadata without resetting live reservations.
  if state_status(ARGV[4]) then return 'MISMATCH' end
  local fields = {'starts_sec', 'starts_usec', 'ends_sec', 'ends_usec'}
  for i = 1, 4 do
    if redis.call('HGET', KEYS[6], fields[i]) ~= ARGV[i + 4] then return 'MISMATCH' end
  end
  return 'PUBLISHED'
end
if owner and owner ~= ARGV[1] then return 'CONFLICT' end
if open == '1' then
  return 'CONFLICT'
end
if not owner then
  for i = 1, 6 do
    if redis.call('EXISTS', KEYS[i]) == 1 then return 'CONFLICT' end
  end
end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6])
redis.call('SET', KEYS[7], ARGV[1])
redis.call('SET', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], '0')
install_metadata(ARGV[4], ARGV[5], ARGV[6], ARGV[7], ARGV[8])
local count = tonumber(ARGV[3])
for i = 1, count do
  local user = ARGV[2 * i + 7]
  local request = ARGV[2 * i + 8]
  redis.call('HSET', KEYS[3], user, 'S:' .. request)
  redis.call('HSET', KEYS[4], request, 'S')
end
return 'INSTALLED'`
