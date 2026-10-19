package shop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

var ErrCacheMiss = errors.New("cache miss")

// CacheStore is the shared-cache boundary. RedisStore is its production adapter;
// tests can use an in-memory implementation without starting Redis.
type CacheStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

// RedisStore adapts a go-redis client to CacheStore.
type RedisStore struct {
	client redis.Cmdable
}

func NewRedisStore(client redis.Cmdable) RedisStore {
	return RedisStore{client: client}
}

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

func (s RedisStore) Del(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}

// CacheConfig controls bounded-staleness behavior for public shop details.
type CacheConfig struct {
	SoftTTL           time.Duration
	HardTTL           time.Duration
	NegativeTTL       time.Duration
	BackendTimeout    time.Duration
	RefreshCapacity   int
	Now               func() time.Time
	Jitter            func(time.Duration) time.Duration
	RistrettoCounters int64
	RistrettoMaxCost  int64
}

func (c CacheConfig) withDefaults() CacheConfig {
	if c.SoftTTL <= 0 {
		c.SoftTTL = 30 * time.Second
	}
	if c.HardTTL <= 0 {
		c.HardTTL = 2 * time.Minute
	}
	if c.HardTTL < c.SoftTTL {
		c.HardTTL = c.SoftTTL
	}
	if c.NegativeTTL <= 0 {
		c.NegativeTTL = 10 * time.Second
	}
	if c.BackendTimeout <= 0 {
		c.BackendTimeout = time.Second
	}
	if c.RefreshCapacity <= 0 {
		c.RefreshCapacity = 16
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Jitter == nil {
		c.Jitter = defaultJitter
	}
	if c.RistrettoCounters <= 0 {
		c.RistrettoCounters = 10_000
	}
	if c.RistrettoMaxCost <= 0 {
		c.RistrettoMaxCost = 1 << 20
	}
	return c
}

func defaultJitter(ttl time.Duration) time.Duration {
	if ttl < 10*time.Millisecond {
		return ttl
	}
	span := int64(ttl / 10)
	return ttl + time.Duration(rand.Int63n(2*span+1)-span)
}

// CacheMetrics reports atomic, process-local observations of the cache chain.
type CacheMetrics struct {
	L1Hit        uint64
	L2Hit        uint64
	BackendLoad  uint64
	NegativeHit  uint64
	RefreshDrop  uint64
	RefreshError uint64
}

type cacheCounters struct {
	l1Hit        atomic.Uint64
	l2Hit        atomic.Uint64
	backendLoad  atomic.Uint64
	negativeHit  atomic.Uint64
	refreshDrop  atomic.Uint64
	refreshError atomic.Uint64
}

// CachedRepository adds an L1 Ristretto cache and shared Redis cache to a
// repository. List intentionally delegates to the backend; only public detail
// reads have the explicitly bounded stale-data contract.
type CachedRepository struct {
	backend Repository
	store   CacheStore
	l1      *ristretto.Cache[string, cacheEnvelope]
	group   singleflight.Group
	config  CacheConfig
	refresh chan struct{}
	metrics cacheCounters
}

type cacheEnvelope struct {
	Shop          *Shop     `json:"shop,omitempty"`
	NotFound      bool      `json:"not_found"`
	SoftExpiresAt time.Time `json:"soft_expires_at"`
	HardExpiresAt time.Time `json:"hard_expires_at"`
}

// NewCachedRepository builds a cache layer around backend. A nil shared store
// is allowed: it keeps correct L1 plus backend behavior for local development.
func NewCachedRepository(backend Repository, store CacheStore, config CacheConfig) (*CachedRepository, error) {
	if backend == nil {
		return nil, errors.New("shop cache backend is required")
	}
	config = config.withDefaults()
	l1, err := ristretto.NewCache(&ristretto.Config[string, cacheEnvelope]{
		NumCounters: config.RistrettoCounters,
		MaxCost:     config.RistrettoMaxCost,
		BufferItems: 64,
	})
	if err != nil {
		return nil, fmt.Errorf("create shop L1 cache: %w", err)
	}
	return &CachedRepository{
		backend: backend,
		store:   store,
		l1:      l1,
		config:  config,
		refresh: make(chan struct{}, config.RefreshCapacity),
	}, nil
}

func (r *CachedRepository) List(ctx context.Context, offset, limit int) ([]Shop, int, error) {
	return r.backend.List(ctx, offset, limit)
}

// FindByID reads L1, then Redis, then the authoritative backend. Soft-expired
// positive values are served as bounded stale data while one background refresh
// is attempted. Hard-expired values never escape the cache layer.
func (r *CachedRepository) FindByID(ctx context.Context, id int64) (Shop, error) {
	if err := ctx.Err(); err != nil {
		return Shop{}, err
	}
	key := shopCacheKey(id)
	now := r.config.Now()
	if value, ok := r.l1.Get(key); ok {
		if item, hit, stale := r.readEnvelope(value, now); hit {
			r.metrics.l1Hit.Add(1)
			if value.NotFound {
				r.metrics.negativeHit.Add(1)
				return Shop{}, ErrShopNotFound
			}
			if stale {
				r.triggerRefresh(key, id)
			}
			return item, nil
		}
		r.l1.Del(key)
	}

	if r.store != nil {
		encoded, err := r.store.Get(ctx, key)
		if err == nil {
			var value cacheEnvelope
			if json.Unmarshal([]byte(encoded), &value) == nil {
				if item, hit, stale := r.readEnvelope(value, now); hit {
					r.metrics.l2Hit.Add(1)
					r.l1.Set(key, value, 1)
					if value.NotFound {
						r.metrics.negativeHit.Add(1)
						return Shop{}, ErrShopNotFound
					}
					if stale {
						r.triggerRefresh(key, id)
					}
					return item, nil
				}
			}
		}
	}

	return r.loadAndCache(ctx, key, id)
}

func (r *CachedRepository) readEnvelope(value cacheEnvelope, now time.Time) (Shop, bool, bool) {
	if value.HardExpiresAt.IsZero() || !now.Before(value.HardExpiresAt) {
		return Shop{}, false, false
	}
	if value.NotFound {
		return Shop{}, true, false
	}
	if value.Shop == nil {
		return Shop{}, false, false
	}
	return *value.Shop, true, !now.Before(value.SoftExpiresAt)
}

func (r *CachedRepository) loadAndCache(ctx context.Context, key string, id int64) (Shop, error) {
	result, err, _ := r.group.Do(key, func() (any, error) {
		r.metrics.backendLoad.Add(1)
		backendCtx, cancel := context.WithTimeout(ctx, r.config.BackendTimeout)
		defer cancel()
		item, err := r.backend.FindByID(backendCtx, id)
		if err != nil {
			if errors.Is(err, ErrShopNotFound) {
				r.writeEnvelope(backendCtx, key, cacheEnvelope{NotFound: true}, r.config.NegativeTTL, r.config.NegativeTTL)
			}
			return Shop{}, err
		}
		copy := item
		hardTTL := r.config.Jitter(r.config.HardTTL)
		if hardTTL <= 0 {
			hardTTL = r.config.HardTTL
		}
		softTTL := r.config.SoftTTL
		if softTTL > hardTTL {
			softTTL = hardTTL
		}
		r.writeEnvelope(backendCtx, key, cacheEnvelope{Shop: &copy}, softTTL, hardTTL)
		return item, nil
	})
	if err != nil {
		return Shop{}, err
	}
	return result.(Shop), nil
}

func (r *CachedRepository) writeEnvelope(ctx context.Context, key string, value cacheEnvelope, softTTL, hardTTL time.Duration) {
	now := r.config.Now()
	value.SoftExpiresAt = now.Add(softTTL)
	value.HardExpiresAt = now.Add(hardTTL)
	// Ristretto may reject writes under pressure. Correctness never depends on it.
	r.l1.Set(key, value, 1)
	if r.store == nil {
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	// Redis write failure is deliberately ignored: the backend result remains valid.
	_ = r.store.Set(ctx, key, string(encoded), hardTTL)
}

func (r *CachedRepository) triggerRefresh(key string, id int64) {
	select {
	case r.refresh <- struct{}{}:
		go func() {
			defer func() { <-r.refresh }()
			ctx, cancel := context.WithTimeout(context.Background(), r.config.BackendTimeout)
			defer cancel()
			if _, err := r.loadAndCache(ctx, key, id); err != nil {
				r.metrics.refreshError.Add(1)
			}
		}()
	default:
		r.metrics.refreshDrop.Add(1)
	}
}

// Invalidate removes a detail from both cache levels after the authoritative
// store has committed its update. L1 is removed even when Redis is unavailable.
func (r *CachedRepository) Invalidate(ctx context.Context, id int64) error {
	key := shopCacheKey(id)
	r.l1.Del(key)
	if r.store == nil {
		return nil
	}
	return r.store.Del(ctx, key)
}

func (r *CachedRepository) Metrics() CacheMetrics {
	return CacheMetrics{
		L1Hit:        r.metrics.l1Hit.Load(),
		L2Hit:        r.metrics.l2Hit.Load(),
		BackendLoad:  r.metrics.backendLoad.Load(),
		NegativeHit:  r.metrics.negativeHit.Load(),
		RefreshDrop:  r.metrics.refreshDrop.Load(),
		RefreshError: r.metrics.refreshError.Load(),
	}
}

func (r *CachedRepository) Close() {
	r.l1.Close()
}

func shopCacheKey(id int64) string {
	return fmt.Sprintf("shop:detail:%d", id)
}
