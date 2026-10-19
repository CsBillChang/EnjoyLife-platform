// Package ratelimit provides the Redis-backed admission rate limit. It is a
// fail-closed boundary: callers must reject an admission when Allow returns an
// error instead of treating a Redis outage as permission to continue.
package ratelimit

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

const (
	maxCapacity = 1_000_000
	maxRate     = 1_000_000
)

var (
	ErrInvalidRequest = errors.New("invalid rate limit request")
	ErrInvalidPrefix  = errors.New("rate limit prefix must not contain Redis hash-tag delimiters")
	ErrScriptResult   = errors.New("unexpected rate limit script result")
)

// Bucket describes one token bucket. Values are deliberately bounded so Lua's
// number arithmetic stays exact for the script's refill calculations.
type Bucket struct {
	Capacity        uint64
	RefillPerSecond uint64
}

// Request contains the server-selected limits for one activity and user. The
// HTTP client must not be allowed to select these values.
type Request struct {
	ActivityID uint64
	UserID     uint64
	Total      Bucket
	User       Bucket
	Cost       uint64
}

// Decision is the atomic result. RetryAfter is zero when Allowed is true.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// Store accepts redis.Client and cluster clients as well as test fakes.
type Store struct {
	client redis.Scripter
	prefix string
}

func New(client redis.Scripter, prefix string) (*Store, error) {
	if client == nil {
		return nil, errors.New("rate limit redis client is required")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), ":")
	if prefix == "" {
		prefix = "local-life:ratelimit"
	}
	if strings.ContainsAny(prefix, "{}") {
		return nil, ErrInvalidPrefix
	}
	return &Store{client: client, prefix: prefix}, nil
}

// Allow atomically tests and charges both the activity-wide and per-user
// buckets. Redis TIME is used inside Lua, so a caller cannot advance a local
// clock to refill a bucket. Any Redis error is returned for the caller to
// fail closed.
func (s *Store) Allow(ctx context.Context, request Request) (Decision, error) {
	if err := validate(request); err != nil {
		return Decision{}, err
	}
	ttl, err := keyTTL(request)
	if err != nil {
		return Decision{}, err
	}
	value, err := redis.NewScript(allowLua).Run(ctx, s.client, s.keys(request.ActivityID, request.UserID),
		request.Total.Capacity, request.Total.RefillPerSecond,
		request.User.Capacity, request.User.RefillPerSecond,
		request.Cost, ttl.Milliseconds()).Result()
	if err != nil {
		return Decision{}, fmt.Errorf("run rate limit script: %w", err)
	}
	parts, ok := value.([]interface{})
	if !ok || len(parts) != 2 {
		return Decision{}, fmt.Errorf("%w: %#v", ErrScriptResult, value)
	}
	allowed, ok := integer(parts[0])
	if !ok || (allowed != 0 && allowed != 1) {
		return Decision{}, fmt.Errorf("%w: %#v", ErrScriptResult, value)
	}
	retryMS, ok := integer(parts[1])
	if !ok || retryMS < 0 || retryMS > int64(math.MaxInt64/time.Millisecond) {
		return Decision{}, fmt.Errorf("%w: %#v", ErrScriptResult, value)
	}
	if allowed == 1 {
		return Decision{Allowed: true}, nil
	}
	return Decision{RetryAfter: time.Duration(retryMS) * time.Millisecond}, nil
}

func validate(r Request) error {
	if r.ActivityID == 0 || r.UserID == 0 || r.Cost == 0 || r.Cost > maxCapacity || !validBucket(r.Total) || !validBucket(r.User) || r.Cost > r.Total.Capacity || r.Cost > r.User.Capacity {
		return ErrInvalidRequest
	}
	return nil
}

func validBucket(b Bucket) bool {
	return b.Capacity > 0 && b.Capacity <= maxCapacity && b.RefillPerSecond > 0 && b.RefillPerSecond <= maxRate
}

// keyTTL is longer than a full refill so an inactive bucket naturally ages
// out, without retaining a key forever. Bounds above keep this conversion safe.
func keyTTL(r Request) (time.Duration, error) {
	fullMS := func(b Bucket) uint64 {
		return (b.Capacity*1000 + b.RefillPerSecond - 1) / b.RefillPerSecond
	}
	ms := fullMS(r.Total)
	if user := fullMS(r.User); user > ms {
		ms = user
	}
	ms += 60_000 // preserve a quiet bucket briefly past a full refill.
	if ms > uint64(math.MaxInt64/time.Millisecond) {
		return 0, ErrInvalidRequest
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (s *Store) keys(activityID, userID uint64) []string {
	base := s.prefix + ":{" + strconv.FormatUint(activityID, 10) + "}"
	return []string{base + ":total", base + ":user:" + strconv.FormatUint(userID, 10)}
}

func integer(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		i, err := strconv.ParseInt(v, 10, 64)
		return i, err == nil
	case []byte:
		i, err := strconv.ParseInt(string(v), 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

const allowLua = `
local now = redis.call('TIME')
local now_ms = now[1] * 1000 + math.floor(now[2] / 1000)

local function refill(key, capacity, rate)
  local tokens = tonumber(redis.call('HGET', key, 'tokens'))
  local last = tonumber(redis.call('HGET', key, 'last_ms'))
  if not tokens or not last then return capacity, now_ms end
  if tokens >= capacity then return capacity, now_ms end
  local elapsed = now_ms - last
  if elapsed <= 0 then return tokens, last end
  local to_full = math.floor(((capacity - tokens) * 1000 + rate - 1) / rate)
  if elapsed >= to_full then return capacity, now_ms end
  local added = math.floor(elapsed * rate / 1000)
  if added <= 0 then return tokens, last end
  return tokens + added, last + math.floor(added * 1000 / rate)
end

local total_tokens, total_last = refill(KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2]))
local user_tokens, user_last = refill(KEYS[2], tonumber(ARGV[3]), tonumber(ARGV[4]))
local cost = tonumber(ARGV[5])
local ttl = tonumber(ARGV[6])

local function retry_after(tokens, rate)
  if tokens >= cost then return 0 end
  return math.floor(((cost - tokens) * 1000 + rate - 1) / rate)
end

local total_retry = retry_after(total_tokens, tonumber(ARGV[2]))
local user_retry = retry_after(user_tokens, tonumber(ARGV[4]))
local retry = total_retry
if user_retry > retry then retry = user_retry end
if retry == 0 then
  total_tokens = total_tokens - cost
  user_tokens = user_tokens - cost
end

redis.call('HSET', KEYS[1], 'tokens', total_tokens, 'last_ms', total_last)
redis.call('HSET', KEYS[2], 'tokens', user_tokens, 'last_ms', user_last)
redis.call('PEXPIRE', KEYS[1], ttl)
redis.call('PEXPIRE', KEYS[2], ttl)
if retry == 0 then return {1, 0} end
return {0, retry}`
