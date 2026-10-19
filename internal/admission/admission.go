// Package admission owns the short-lived Redis admission reservation used by
// the asynchronous purchase path. It is deliberately not an inventory store:
// MySQL remains responsible for the final conditional stock deduction.
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
	ErrInvalidRequest         = errors.New("invalid admission request")
	ErrAcceptedOutsideWindow  = errors.New("accepted_at is outside the activity window")
	ErrProcessingDeadline     = errors.New("processing deadline has passed")
	ErrGenerationMismatch     = errors.New("admission generation does not match metadata")
	ErrAdmissionDisabled      = errors.New("admission is disabled")
	ErrSoldOut                = errors.New("admission stock hint is exhausted")
	ErrUserReserved           = errors.New("user already holds an admission reservation")
	ErrReservationNotOwned    = errors.New("admission reservation is not owned by request")
	ErrOrderAlreadyCommitted  = errors.New("admission reservation belongs to a committed order")
	ErrRequestCancelled       = errors.New("admission request was cancelled")
	ErrStateAlreadyExists     = errors.New("admission generation already exists")
	ErrStateMissing           = errors.New("admission generation is not initialized")
	ErrStatePartial           = errors.New("admission generation state is incomplete")
	ErrUnexpectedScriptResult = errors.New("unexpected admission script result")
)

// Request contains only server-owned values from the persisted purchase
// request. AcceptedAt and ProcessingDeadline must never be supplied by HTTP.
type Request struct {
	ActivityID         uint64
	Generation         uint64
	UserID             uint64
	RequestID          string
	AcceptedAt         time.Time
	ProcessingDeadline time.Time
	StartsAt           time.Time
	EndsAt             time.Time
}

// ActivityState is supplied by the rebuild/administration path before this
// generation is opened. Setting a state is intentionally separate from Admit:
// normal request traffic cannot reset a stock hint.
type ActivityState struct {
	ActivityID uint64
	Generation uint64
	StockHint  uint64
	Open       bool
	StartsAt   time.Time
	EndsAt     time.Time
}

type Result string

const (
	Admitted  Result = "ADMITTED"
	Replay    Result = "REPLAY"
	Released  Result = "RELEASED"
	Cancelled Result = "CANCELLED"
	Noop      Result = "NOOP"
)

// Store exposes a narrow Redis Lua boundary. A *redis.Client, cluster client,
// or test fake can be used without coupling callers to the concrete client.
type Store struct {
	client redis.Scripter
	prefix string
	now    func() time.Time
}

func New(client redis.Scripter, prefix string) (*Store, error) {
	if client == nil {
		return nil, errors.New("admission redis client is required")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), ":")
	if prefix == "" {
		prefix = "local-life:admission"
	}
	return &Store{client: client, prefix: prefix, now: time.Now}, nil
}

// Initialize installs a new generation's stock hint and switch. Callers must
// fence MySQL admission/writes before rebuilding a generation; this method is
// not safe as an in-flight traffic reconfiguration primitive.
func (s *Store) Initialize(ctx context.Context, state ActivityState) error {
	if state.ActivityID == 0 || state.Generation == 0 || state.StockHint > math.MaxInt64 || !validWindow(state.StartsAt, state.EndsAt) {
		return ErrInvalidRequest
	}
	open := "0"
	if state.Open {
		open = "1"
	}
	args := []any{strconv.FormatUint(state.StockHint, 10), open, strconv.FormatUint(state.Generation, 10)}
	args = append(args, timeArgs(state.StartsAt)...)
	args = append(args, timeArgs(state.EndsAt)...)
	value, err := redis.NewScript(initializeLua).Run(ctx, s.client, s.activityKeys(state.ActivityID, state.Generation), args...).Text()
	if err != nil {
		return fmt.Errorf("initialize admission state: %w", err)
	}
	switch value {
	case "INITIALIZED":
		return nil
	case "EXISTS":
		return ErrStateAlreadyExists
	case "PARTIAL":
		return ErrStatePartial
	default:
		return fmt.Errorf("%w: %q", ErrUnexpectedScriptResult, value)
	}
}

// SetOpen changes only the admission switch for one existing generation. It
// never writes the stock hint or user markers, so pausing traffic cannot erase
// reservations needed by recovery.
func (s *Store) SetOpen(ctx context.Context, activityID, generation uint64, open bool) error {
	if activityID == 0 || generation == 0 {
		return ErrInvalidRequest
	}
	value := "0"
	if open {
		value = "1"
	}
	result, err := redis.NewScript(setOpenLua).Run(ctx, s.client, s.activityKeys(activityID, generation), value, strconv.FormatUint(generation, 10)).Text()
	if err != nil {
		return fmt.Errorf("set admission switch: %w", err)
	}
	if result == "MISSING" {
		return ErrStateMissing
	}
	if result == "PARTIAL" {
		return ErrStatePartial
	}
	if result == "GENERATION_MISMATCH" {
		return ErrGenerationMismatch
	}
	if result != "UPDATED" {
		return fmt.Errorf("%w: %q", ErrUnexpectedScriptResult, result)
	}
	return nil
}

// Admit reserves one Redis eligibility slot. Repeating the same request ID is
// harmless; another request ID for the same user is rejected, even if stock is
// still available. Lua rechecks persisted times against generation metadata
// and Redis TIME atomically with the reservation writes.
func (s *Store) Admit(ctx context.Context, request Request) (Result, error) {
	if err := s.validate(request); err != nil {
		return "", err
	}
	args := []any{request.UserKey(), request.RequestID, strconv.FormatUint(request.Generation, 10)}
	args = append(args, timeArgs(request.AcceptedAt)...)
	args = append(args, timeArgs(request.ProcessingDeadline)...)
	value, err := redis.NewScript(admitLua).Run(ctx, s.client, s.activityKeys(request.ActivityID, request.Generation), args...).Text()
	if err != nil {
		return "", fmt.Errorf("run admission script: %w", err)
	}
	switch value {
	case string(Admitted):
		return Admitted, nil
	case string(Replay):
		return Replay, nil
	case "DISABLED":
		return "", ErrAdmissionDisabled
	case "ACCEPTED_OUTSIDE_WINDOW":
		return "", ErrAcceptedOutsideWindow
	case "PROCESSING_EXPIRED":
		return "", ErrProcessingDeadline
	case "GENERATION_MISMATCH":
		return "", ErrGenerationMismatch
	case "INVALID_REQUEST":
		return "", ErrInvalidRequest
	case "MISSING":
		return "", ErrStateMissing
	case "PARTIAL":
		return "", ErrStatePartial
	case "SOLD_OUT":
		return "", ErrSoldOut
	case "USER_RESERVED":
		return "", ErrUserReserved
	case "CANCELLED":
		return "", ErrRequestCancelled
	case "INCONSISTENT":
		return "", ErrStatePartial
	default:
		return "", fmt.Errorf("%w: %q", ErrUnexpectedScriptResult, value)
	}
}

// MarkOrdered turns the pending reservation into a permanent user purchase
// marker. It is idempotent and prevents later release attempts from restoring
// this user's eligibility.
func (s *Store) MarkOrdered(ctx context.Context, activityID, generation, userID uint64, requestID string) error {
	if activityID == 0 || generation == 0 || userID == 0 || strings.TrimSpace(requestID) == "" {
		return ErrInvalidRequest
	}
	value, err := redis.NewScript(markOrderedLua).Run(ctx, s.client, s.activityKeys(activityID, generation), userKey(userID), requestID, strconv.FormatUint(generation, 10)).Text()
	if err != nil {
		return fmt.Errorf("mark admission ordered: %w", err)
	}
	switch value {
	case "ORDERED", "REPLAY":
		return nil
	case "NOT_OWNER":
		return ErrReservationNotOwned
	case "CANCELLED":
		return ErrRequestCancelled
	case "MISSING":
		return ErrStateMissing
	case "PARTIAL":
		return ErrStatePartial
	case "GENERATION_MISMATCH":
		return ErrGenerationMismatch
	default:
		return fmt.Errorf("%w: %q", ErrUnexpectedScriptResult, value)
	}
}

// Release restores a slot only once and only when the pending marker still
// belongs to this request and generation. A committed marker can never release.
func (s *Store) Release(ctx context.Context, activityID, generation, userID uint64, requestID string) (Result, error) {
	if activityID == 0 || generation == 0 || userID == 0 || strings.TrimSpace(requestID) == "" {
		return "", ErrInvalidRequest
	}
	value, err := redis.NewScript(releaseLua).Run(ctx, s.client, s.activityKeys(activityID, generation), userKey(userID), requestID, strconv.FormatUint(generation, 10)).Text()
	if err != nil {
		return "", fmt.Errorf("release admission reservation: %w", err)
	}
	switch value {
	case string(Released):
		return Released, nil
	case string(Cancelled):
		return Cancelled, nil
	case "ORDER_COMMITTED":
		return Noop, ErrOrderAlreadyCommitted
	case "MISSING":
		return Noop, ErrStateMissing
	case "PARTIAL", "INCONSISTENT":
		return Noop, ErrStatePartial
	case "GENERATION_MISMATCH":
		return Noop, ErrGenerationMismatch
	default:
		return "", fmt.Errorf("%w: %q", ErrUnexpectedScriptResult, value)
	}
}

// RestoreCommittedStock projects a durable MySQL inventory RELEASE into the
// generation's stock hint. The caller must use the release's fenced generation;
// a rebuild already includes old releases in its new authoritative snapshot.
// A permanent per-order receipt prevents retries from adding stock twice. User
// purchase markers are deliberately untouched: refund/close does not permit a
// second successful purchase by the same user.
func (s *Store) RestoreCommittedStock(ctx context.Context, activityID, generation, orderID uint64) (Result, error) {
	if activityID == 0 || generation == 0 || orderID == 0 {
		return "", ErrInvalidRequest
	}
	keys := s.activityKeys(activityID, generation)
	value, err := redis.NewScript(restoreCommittedStockLua).Run(ctx, s.client, keys, strconv.FormatUint(orderID, 10), strconv.FormatUint(generation, 10)).Text()
	if err != nil {
		return "", fmt.Errorf("restore committed admission stock: %w", err)
	}
	switch value {
	case string(Released):
		return Released, nil
	case string(Replay):
		return Replay, nil
	case "MISSING":
		return Noop, ErrStateMissing
	case "PARTIAL":
		return Noop, ErrStatePartial
	case "GENERATION_MISMATCH":
		return Noop, ErrGenerationMismatch
	default:
		return "", fmt.Errorf("%w: %q", ErrUnexpectedScriptResult, value)
	}
}

func (s *Store) validate(r Request) error {
	if r.ActivityID == 0 || r.Generation == 0 || r.UserID == 0 || strings.TrimSpace(r.RequestID) == "" || r.RequestID == "__initialized" || !validTime(r.AcceptedAt) || !validTime(r.ProcessingDeadline) || !validWindow(r.StartsAt, r.EndsAt) || !r.AcceptedAt.Before(r.ProcessingDeadline) {
		return ErrInvalidRequest
	}
	accepted := r.AcceptedAt.UTC()
	if accepted.Before(r.StartsAt.UTC()) || !accepted.Before(r.EndsAt.UTC()) {
		return ErrAcceptedOutsideWindow
	}
	if !s.now().UTC().Before(r.ProcessingDeadline.UTC()) {
		return ErrProcessingDeadline
	}
	return nil
}

func (r Request) UserKey() string  { return userKey(r.UserID) }
func userKey(userID uint64) string { return strconv.FormatUint(userID, 10) }

func (s *Store) activityKeys(activityID, generation uint64) []string {
	base := s.prefix + ":{" + strconv.FormatUint(activityID, 10) + ":" + strconv.FormatUint(generation, 10) + "}"
	return []string{base + ":stock", base + ":open", base + ":users", base + ":requests", base + ":stock:releases", base + ":meta"}
}

func validTime(t time.Time) bool { return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999 }
func validWindow(start, end time.Time) bool {
	return validTime(start) && validTime(end) && start.Truncate(time.Microsecond).Before(end.Truncate(time.Microsecond))
}
func timeArgs(t time.Time) []any {
	return []any{strconv.FormatInt(t.Unix(), 10), strconv.Itoa(t.Nanosecond() / 1000)}
}

// Seconds and microseconds are compared separately: even year 9999 seconds
// remain exact Lua numbers, unlike a combined UnixMicro value.
const stateLua = `
local function timestamp(sec, usec)
  if not sec or not usec or not string.match(sec, '^%-?%d+$') or not string.match(usec, '^%d+$') then return nil end
  local s, u = tonumber(sec), tonumber(usec)
  if not s or not u or s < -62135596800 or s > 253402300799 or u < 0 or u > 999999 then return nil end
  return {s, u}
end
local function before(a, b)
  return a[1] < b[1] or (a[1] == b[1] and a[2] < b[2])
end
local function state_status(generation)
  local count = 0
  for i = 1, 6 do count = count + redis.call('EXISTS', KEYS[i]) end
  if count == 0 then return 'MISSING' end
  if count ~= 6 then return 'PARTIAL' end
  for i = 1, 6 do
    local expected = 'hash'
    if i <= 2 then expected = 'string' end
    if redis.call('TYPE', KEYS[i]).ok ~= expected then return 'PARTIAL' end
  end
  for i = 3, 5 do
    if redis.call('HGET', KEYS[i], '__initialized') ~= '1' then return 'PARTIAL' end
  end
  local stock, open = redis.call('GET', KEYS[1]), redis.call('GET', KEYS[2])
  if not string.match(stock, '^%d+$') or #stock > 19 or (#stock == 19 and stock > '9223372036854775807') or (open ~= '0' and open ~= '1') then return 'PARTIAL' end
  if redis.call('HGET', KEYS[6], 'schema') ~= '2' then return 'PARTIAL' end
  local actual_generation = redis.call('HGET', KEYS[6], 'generation')
  if not actual_generation or not string.match(actual_generation, '^%d+$') then return 'PARTIAL' end
  if actual_generation ~= generation then return 'GENERATION_MISMATCH' end
  local starts = timestamp(redis.call('HGET', KEYS[6], 'starts_sec'), redis.call('HGET', KEYS[6], 'starts_usec'))
  local ends = timestamp(redis.call('HGET', KEYS[6], 'ends_sec'), redis.call('HGET', KEYS[6], 'ends_usec'))
  if not starts or not ends or not before(starts, ends) then return 'PARTIAL' end
  return nil
end
local function install_metadata(generation, ss, su, es, eu)
  redis.call('HSET', KEYS[6], 'schema', '2', 'generation', generation, 'starts_sec', ss, 'starts_usec', su, 'ends_sec', es, 'ends_usec', eu)
  for i = 3, 5 do redis.call('HSET', KEYS[i], '__initialized', '1') end
end
`

const initializeLua = stateLua + `
local status = state_status(ARGV[3])
if not status then return 'EXISTS' end
if status ~= 'MISSING' then return 'PARTIAL' end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[2])
install_metadata(ARGV[3], ARGV[4], ARGV[5], ARGV[6], ARGV[7])
return 'INITIALIZED'`

const setOpenLua = stateLua + `
if ARGV[1] == '0' then
  -- Closing retired/legacy state is allowed; reopening requires new metadata.
  if redis.call('EXISTS', KEYS[1]) == 0 or redis.call('EXISTS', KEYS[2]) == 0 then return 'MISSING' end
else
  local status = state_status(ARGV[2])
  if status then return status end
end
redis.call('SET', KEYS[2], ARGV[1])
return 'UPDATED'`

const admitLua = stateLua + `
local status = state_status(ARGV[3])
if status then return status end
local accepted = timestamp(ARGV[4], ARGV[5])
local deadline = timestamp(ARGV[6], ARGV[7])
if not accepted or not deadline or not before(accepted, deadline) or not ARGV[1] or not ARGV[2] or ARGV[2] == '__initialized' then return 'INVALID_REQUEST' end
local starts = timestamp(redis.call('HGET', KEYS[6], 'starts_sec'), redis.call('HGET', KEYS[6], 'starts_usec'))
local ends = timestamp(redis.call('HGET', KEYS[6], 'ends_sec'), redis.call('HGET', KEYS[6], 'ends_usec'))
if before(accepted, starts) or not before(accepted, ends) then return 'ACCEPTED_OUTSIDE_WINDOW' end
local now = redis.call('TIME')
if not before({tonumber(now[1]), tonumber(now[2])}, deadline) then return 'PROCESSING_EXPIRED' end
if redis.call('GET', KEYS[2]) ~= '1' then return 'DISABLED' end
local request_state = redis.call('HGET', KEYS[4], ARGV[2])
if request_state == 'C' then return 'CANCELLED' end
local current = redis.call('HGET', KEYS[3], ARGV[1])
if request_state == 'P' then
  if current == 'P:' .. ARGV[2] then return 'REPLAY' end
  return 'INCONSISTENT'
end
if request_state == 'S' then
  if current == 'S:' .. ARGV[2] then return 'REPLAY' end
  return 'INCONSISTENT'
end
if request_state then return 'INCONSISTENT' end
if current then return 'USER_RESERVED' end
local stock = tonumber(redis.call('GET', KEYS[1]))
if not stock or stock <= 0 then return 'SOLD_OUT' end
redis.call('DECR', KEYS[1])
redis.call('HSET', KEYS[3], ARGV[1], 'P:' .. ARGV[2])
redis.call('HSET', KEYS[4], ARGV[2], 'P')
return 'ADMITTED'`

const markOrderedLua = stateLua + `
local status = state_status(ARGV[3])
if status then return status end
local current = redis.call('HGET', KEYS[3], ARGV[1])
local request_state = redis.call('HGET', KEYS[4], ARGV[2])
if current == 'P:' .. ARGV[2] and request_state == 'P' then
  redis.call('HSET', KEYS[3], ARGV[1], 'S:' .. ARGV[2])
  redis.call('HSET', KEYS[4], ARGV[2], 'S')
  return 'ORDERED'
end
if current == 'S:' .. ARGV[2] and request_state == 'S' then return 'REPLAY' end
if request_state == 'C' then return 'CANCELLED' end
return 'NOT_OWNER'`

const releaseLua = stateLua + `
local status = state_status(ARGV[3])
if status then return status end
local current = redis.call('HGET', KEYS[3], ARGV[1])
local request_state = redis.call('HGET', KEYS[4], ARGV[2])
if current == 'P:' .. ARGV[2] and request_state == 'P' then
  -- Redis scripts do not roll back earlier writes on command errors. Increment
  -- before deleting ownership, and preserve all state if the value overflows.
  local incremented = redis.pcall('INCR', KEYS[1])
  if type(incremented) == 'table' and incremented.err then return 'PARTIAL' end
  redis.call('HDEL', KEYS[3], ARGV[1])
  redis.call('HSET', KEYS[4], ARGV[2], 'C')
  return 'RELEASED'
end
if current == 'S:' .. ARGV[2] or request_state == 'S' then return 'ORDER_COMMITTED' end
if current == 'P:' .. ARGV[2] then return 'INCONSISTENT' end
if request_state == 'C' then return 'CANCELLED' end
if request_state then return 'INCONSISTENT' end
redis.call('HSET', KEYS[4], ARGV[2], 'C')
return 'CANCELLED'`

const restoreCommittedStockLua = stateLua + `
local status = state_status(ARGV[2])
if status then return status end
if redis.call('HEXISTS', KEYS[5], ARGV[1]) == 1 then return 'REPLAY' end
local incremented = redis.pcall('INCR', KEYS[1])
if type(incremented) == 'table' and incremented.err then return 'PARTIAL' end
redis.call('HSET', KEYS[5], ARGV[1], '1')
return 'RELEASED'`
