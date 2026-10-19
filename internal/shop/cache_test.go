package shop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryCacheStore struct {
	mu     sync.Mutex
	values map[string]string
	getErr error
	setErr error
	delErr error
}

func newMemoryCacheStore() *memoryCacheStore {
	return &memoryCacheStore{values: make(map[string]string)}
}

func (s *memoryCacheStore) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return "", s.getErr
	}
	value, ok := s.values[key]
	if !ok {
		return "", ErrCacheMiss
	}
	return value, nil
}

func (s *memoryCacheStore) Set(_ context.Context, key, value string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.values[key] = value
	return nil
}

func (s *memoryCacheStore) Del(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delErr != nil {
		return s.delErr
	}
	delete(s.values, key)
	return nil
}

type countingRepository struct {
	mu       sync.Mutex
	shop     Shop
	err      error
	loads    int
	started  chan struct{}
	finished chan struct{}
	release  <-chan struct{}
}

func (r *countingRepository) List(context.Context, int, int) ([]Shop, int, error) { return nil, 0, nil }

func (r *countingRepository) FindByID(_ context.Context, _ int64) (Shop, error) {
	r.mu.Lock()
	r.loads++
	started := r.started
	release := r.release
	item, err := r.shop, r.err
	r.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if r.finished != nil {
		select {
		case r.finished <- struct{}{}:
		default:
		}
	}
	return item, err
}

func (r *countingRepository) loadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loads
}

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func newCachedForTest(t *testing.T, backend Repository, store CacheStore, now func() time.Time) *CachedRepository {
	t.Helper()
	repository, err := NewCachedRepository(backend, store, CacheConfig{
		SoftTTL: 100 * time.Millisecond, HardTTL: time.Second, NegativeTTL: 100 * time.Millisecond,
		BackendTimeout: time.Second, RefreshCapacity: 1, Now: now, Jitter: func(ttl time.Duration) time.Duration { return ttl },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Close)
	return repository
}

func TestCachedRepositoryConcurrentColdKeyLoadsBackendOnce(t *testing.T) {
	release := make(chan struct{})
	backend := &countingRepository{shop: Shop{ID: 1, Name: "shop"}, started: make(chan struct{}, 1), release: release}
	repository := newCachedForTest(t, backend, newMemoryCacheStore(), time.Now)

	const callers = 12
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := repository.FindByID(context.Background(), 1); err != nil {
				t.Errorf("FindByID() error = %v", err)
			}
		}()
	}
	<-backend.started
	close(release)
	group.Wait()
	if loads := backend.loadCount(); loads != 1 {
		t.Fatalf("backend loads = %d, want 1", loads)
	}
}

func TestCachedRepositoryNegativeCache(t *testing.T) {
	backend := &countingRepository{err: ErrShopNotFound}
	repository := newCachedForTest(t, backend, newMemoryCacheStore(), time.Now)
	for range 2 {
		_, err := repository.FindByID(context.Background(), 4)
		if !errors.Is(err, ErrShopNotFound) {
			t.Fatalf("FindByID() error = %v", err)
		}
	}
	if backend.loadCount() != 1 || repository.Metrics().NegativeHit != 1 {
		t.Fatalf("loads=%d metrics=%+v", backend.loadCount(), repository.Metrics())
	}
}

func TestCachedRepositoryInvalidateForcesFreshLoad(t *testing.T) {
	backend := &countingRepository{shop: Shop{ID: 5, Name: "before"}}
	store := newMemoryCacheStore()
	repository := newCachedForTest(t, backend, store, time.Now)
	if _, err := repository.FindByID(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.shop.Name = "after"
	backend.mu.Unlock()
	if err := repository.Invalidate(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	item, err := repository.FindByID(context.Background(), 5)
	if err != nil || item.Name != "after" || backend.loadCount() != 2 {
		t.Fatalf("item=%+v err=%v loads=%d", item, err, backend.loadCount())
	}
}

func TestCachedRepositorySoftAndHardExpiry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	backend := &countingRepository{
		shop:     Shop{ID: 6, Name: "first"},
		finished: make(chan struct{}, 2),
	}
	repository := newCachedForTest(t, backend, newMemoryCacheStore(), clock.Now)
	if _, err := repository.FindByID(context.Background(), 6); err != nil {
		t.Fatal(err)
	}
	<-backend.finished
	clock.Advance(150 * time.Millisecond)
	item, err := repository.FindByID(context.Background(), 6)
	if err != nil || item.Name != "first" {
		t.Fatalf("soft-expired item=%+v err=%v", item, err)
	}
	select {
	case <-backend.finished:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not finish")
	}

	backend.mu.Lock()
	backend.shop.Name = "second"
	backend.mu.Unlock()
	clock.Advance(time.Second)
	item, err = repository.FindByID(context.Background(), 6)
	if err != nil || item.Name != "second" {
		t.Fatalf("hard-expired item=%+v err=%v", item, err)
	}
}

func TestCachedRepositoryRedisFailureFallsBackToBackend(t *testing.T) {
	backend := &countingRepository{shop: Shop{ID: 7, Name: "fallback"}}
	store := newMemoryCacheStore()
	store.getErr = errors.New("redis unavailable")
	repository := newCachedForTest(t, backend, store, time.Now)
	item, err := repository.FindByID(context.Background(), 7)
	if err != nil || item.Name != "fallback" || backend.loadCount() != 1 {
		t.Fatalf("item=%+v err=%v loads=%d", item, err, backend.loadCount())
	}
}
