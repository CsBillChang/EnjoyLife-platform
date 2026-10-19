package coupon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

type CacheStore interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string, time.Duration) error
	Del(context.Context, string) error
	Advance(context.Context, string, uint64, time.Duration) error
}

// redisCommands is intentionally narrower than redis.Cmdable so the adapter's
// redis.Nil -> coupon.ErrCacheMiss contract can be unit-tested without Redis.
type redisCommands interface {
	Get(context.Context, string) *redis.StringCmd
	Set(context.Context, string, any, time.Duration) *redis.StatusCmd
	Del(context.Context, ...string) *redis.IntCmd
	Eval(context.Context, string, []string, ...any) *redis.Cmd
}

type RedisStore struct{ client redisCommands }

func NewRedisStore(client redisCommands) RedisStore { return RedisStore{client: client} }
func (s RedisStore) Get(ctx context.Context, key string) (string, error) {
	value, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrCacheMiss
	}
	return value, err
}
func (s RedisStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}
func (s RedisStore) Del(ctx context.Context, key string) error { return s.client.Del(ctx, key).Err() }
func (s RedisStore) Advance(ctx context.Context, key string, generation uint64, ttl time.Duration) error {
	// Cache invalidation tasks for different activities of one shop may be
	// delivered out of order. Never let an older task move the list namespace
	// backwards and expose a previously cached page.
	const advanceGenerationLua = `
local current = redis.call('GET', KEYS[1])
local incoming = ARGV[1]
if not current or not string.match(current, '^%d+$') or #current < #incoming or (#current == #incoming and current < incoming) then
  redis.call('PSETEX', KEYS[1], ARGV[2], incoming)
else
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 1`
	return s.client.Eval(ctx, advanceGenerationLua, []string{key}, strconv.FormatUint(generation, 10), ttl.Milliseconds()).Err()
}

type CacheConfig struct {
	TTL, NegativeTTL, BackendTimeout    time.Duration
	RistrettoCounters, RistrettoMaxCost int64
}

func (c CacheConfig) defaults() CacheConfig {
	if c.TTL <= 0 {
		c.TTL = 30 * time.Second
	}
	if c.NegativeTTL <= 0 {
		c.NegativeTTL = 10 * time.Second
	}
	if c.BackendTimeout <= 0 {
		c.BackendTimeout = time.Second
	}
	if c.RistrettoCounters <= 0 {
		c.RistrettoCounters = 10_000
	}
	if c.RistrettoMaxCost <= 0 {
		// Every entry below is inserted with cost=1, so this is an entry-weight
		// budget rather than a byte-accurate heap limit.
		c.RistrettoMaxCost = 4_096
	}
	return c
}

type CacheMetrics struct{ L1Hit, L2Hit, BackendLoad, NegativeHit uint64 }
type cacheCounters struct{ l1, l2, backend, negative atomic.Uint64 }

type cacheEntry struct {
	Activity   *Activity  `json:"activity,omitempty"`
	Activities []Activity `json:"activities,omitempty"`
	Total      int        `json:"total,omitempty"`
	NotFound   bool       `json:"not_found,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

// CachedRepository caches public display reads only. Its values must never be
// passed into a purchase decision; order services own that authority.
type CachedRepository struct {
	backend  Repository
	store    CacheStore
	l1       *ristretto.Cache[string, cacheEntry]
	group    singleflight.Group
	config   CacheConfig
	metrics  cacheCounters
	genMu    sync.RWMutex
	localGen map[uint64]uint64
}

func NewCachedRepository(backend Repository, store CacheStore, config CacheConfig) (*CachedRepository, error) {
	if backend == nil {
		return nil, errors.New("coupon cache backend is required")
	}
	config = config.defaults()
	l1, err := ristretto.NewCache(&ristretto.Config[string, cacheEntry]{NumCounters: config.RistrettoCounters, MaxCost: config.RistrettoMaxCost, BufferItems: 64})
	if err != nil {
		return nil, fmt.Errorf("create coupon L1 cache: %w", err)
	}
	return &CachedRepository{backend: backend, store: store, l1: l1, config: config, localGen: make(map[uint64]uint64)}, nil
}

func (r *CachedRepository) FindByID(ctx context.Context, id uint64) (Activity, error) {
	if id == 0 {
		return Activity{}, ErrInvalidActivityID
	}
	key := activityCacheKey(id)
	if value, ok := r.l1.Get(key); ok {
		if cacheEntryValid(value) {
			return r.readDetail(value, true)
		}
		r.l1.Del(key)
	}
	if r.store != nil {
		if raw, err := r.store.Get(ctx, key); err == nil {
			var value cacheEntry
			if json.Unmarshal([]byte(raw), &value) == nil && cacheEntryValid(value) {
				r.metrics.l2.Add(1)
				r.l1.Set(key, value, 1)
				return r.readDetail(value, false)
			}
		}
	}
	result, err, _ := r.group.Do(key, func() (any, error) {
		r.metrics.backend.Add(1)
		loadCtx, cancel := context.WithTimeout(ctx, r.config.BackendTimeout)
		defer cancel()
		item, err := r.backend.FindByID(loadCtx, id)
		if errors.Is(err, ErrActivityNotFound) {
			r.write(key, cacheEntry{NotFound: true}, r.config.NegativeTTL)
		}
		if err != nil {
			return Activity{}, err
		}
		r.write(key, cacheEntry{Activity: &item}, r.config.TTL)
		return item, nil
	})
	if err != nil {
		return Activity{}, err
	}
	return result.(Activity), nil
}

func (r *CachedRepository) readDetail(value cacheEntry, l1 bool) (Activity, error) {
	if l1 {
		r.metrics.l1.Add(1)
	}
	if value.NotFound {
		r.metrics.negative.Add(1)
		return Activity{}, ErrActivityNotFound
	}
	if value.Activity == nil {
		return Activity{}, ErrCacheMiss
	}
	return *value.Activity, nil
}

func (r *CachedRepository) ListByShop(ctx context.Context, shopID uint64, offset, limit int) ([]Activity, int, error) {
	if shopID == 0 {
		return nil, 0, ErrInvalidShopID
	}
	if offset < 0 || limit < 1 {
		return nil, 0, ErrInvalidPagination
	}
	generation, sharedUsable := r.listGeneration(ctx, shopID)
	key := activityListCacheKey(shopID, generation, offset, limit)
	// A Redis error means the generation marker cannot be trusted. Bypass L1/L2
	// and load MySQL instead of serving a potentially pre-invalidation list.
	if !sharedUsable {
		return r.loadList(ctx, key, shopID, offset, limit)
	}
	if value, ok := r.l1.Get(key); ok {
		if cacheEntryValid(value) {
			r.metrics.l1.Add(1)
			return append([]Activity(nil), value.Activities...), value.Total, nil
		}
		r.l1.Del(key)
	}
	if r.store != nil {
		if raw, err := r.store.Get(ctx, key); err == nil {
			var value cacheEntry
			if json.Unmarshal([]byte(raw), &value) == nil && cacheEntryValid(value) {
				r.metrics.l2.Add(1)
				r.l1.Set(key, value, 1)
				return append([]Activity(nil), value.Activities...), value.Total, nil
			}
		}
	}
	return r.loadList(ctx, key, shopID, offset, limit)
}

func (r *CachedRepository) loadList(ctx context.Context, key string, shopID uint64, offset, limit int) ([]Activity, int, error) {
	result, err, _ := r.group.Do(key, func() (any, error) {
		r.metrics.backend.Add(1)
		loadCtx, cancel := context.WithTimeout(ctx, r.config.BackendTimeout)
		defer cancel()
		items, total, err := r.backend.ListByShop(loadCtx, shopID, offset, limit)
		if err != nil {
			return nil, err
		}
		value := cacheEntry{Activities: append([]Activity(nil), items...), Total: total}
		r.write(key, value, r.config.TTL)
		return value, nil
	})
	if err != nil {
		return nil, 0, err
	}
	value := result.(cacheEntry)
	return append([]Activity(nil), value.Activities...), value.Total, nil
}

func (r *CachedRepository) listGeneration(ctx context.Context, shopID uint64) (uint64, bool) {
	local := r.localGeneration(shopID)
	if r.store == nil {
		return local, true
	}
	raw, err := r.store.Get(ctx, activityListGenerationKey(shopID))
	if errors.Is(err, ErrCacheMiss) {
		return local, true
	}
	if err != nil {
		return 0, false
	}
	generation, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	if local > generation {
		generation = local
	}
	return generation, true
}

func (r *CachedRepository) localGeneration(shopID uint64) uint64 {
	r.genMu.RLock()
	defer r.genMu.RUnlock()
	return r.localGen[shopID]
}

func (r *CachedRepository) advanceLocalGeneration(shopID, generation uint64) {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	if generation > r.localGen[shopID] {
		r.localGen[shopID] = generation
	}
}

func (r *CachedRepository) write(key string, value cacheEntry, ttl time.Duration) {
	// Ristretto may reject a write; every cache miss has a correct backend path.
	value.ExpiresAt = time.Now().UTC().Add(ttl)
	r.l1.Set(key, value, 1)
	if r.store == nil {
		return
	}
	encoded, err := json.Marshal(value)
	if err == nil {
		writeCtx, cancel := context.WithTimeout(context.Background(), r.config.BackendTimeout)
		defer cancel()
		_ = r.store.Set(writeCtx, key, string(encoded), ttl)
	}
}

func cacheEntryValid(value cacheEntry) bool {
	return !value.ExpiresAt.IsZero() && time.Now().UTC().Before(value.ExpiresAt)
}

// Invalidate clears one detail and advances the list namespace monotonically.
// Old L1/L2 list pages become unreachable and expire naturally; this avoids
// calling Ristretto.Clear concurrently with request reads and writes.
func (r *CachedRepository) Invalidate(ctx context.Context, activityID, shopID uint64, generation uint64) error {
	r.advanceLocalGeneration(shopID, generation)
	r.l1.Del(activityCacheKey(activityID))
	if r.store == nil {
		return nil
	}
	if err := r.store.Del(ctx, activityCacheKey(activityID)); err != nil {
		return err
	}
	return r.store.Advance(ctx, activityListGenerationKey(shopID), generation, 24*time.Hour)
}

func (r *CachedRepository) Metrics() CacheMetrics {
	return CacheMetrics{r.metrics.l1.Load(), r.metrics.l2.Load(), r.metrics.backend.Load(), r.metrics.negative.Load()}
}
func (r *CachedRepository) Close()      { r.l1.Close() }
func activityCacheKey(id uint64) string { return fmt.Sprintf("coupon:activity:%d", id) }
func activityListCacheKey(shopID, generation uint64, offset, limit int) string {
	return fmt.Sprintf("coupon:shop:%d:activities:g%d:%d:%d", shopID, generation, offset, limit)
}
func activityListGenerationKey(shopID uint64) string {
	return fmt.Sprintf("coupon:shop:%d:activities:generation", shopID)
}
