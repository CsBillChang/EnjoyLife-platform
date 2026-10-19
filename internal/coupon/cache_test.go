package coupon

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type memoryStore struct {
	mu      sync.Mutex
	values  map[string]string
	getErr  error
	deleted []string
}

func newMemoryStore() *memoryStore { return &memoryStore{values: map[string]string{}} }
func (s *memoryStore) Get(_ context.Context, k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return "", s.getErr
	}
	v, ok := s.values[k]
	if !ok {
		return "", ErrCacheMiss
	}
	return v, nil
}
func (s *memoryStore) Set(_ context.Context, k, v string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[k] = v
	return nil
}
func (s *memoryStore) Del(_ context.Context, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, k)
	s.deleted = append(s.deleted, k)
	return nil
}
func (s *memoryStore) Advance(_ context.Context, k string, generation uint64, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, _ := strconv.ParseUint(s.values[k], 10, 64)
	if generation > current {
		s.values[k] = strconv.FormatUint(generation, 10)
	}
	return nil
}

type countingRepo struct {
	mu           sync.Mutex
	item         Activity
	finds, lists int
	findErr      error
	gate         chan struct{}
}

func (r *countingRepo) FindByID(ctx context.Context, _ uint64) (Activity, error) {
	r.mu.Lock()
	r.finds++
	gate := r.gate
	item := r.item
	err := r.findErr
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Activity{}, ctx.Err()
		}
	}
	return item, err
}
func (r *countingRepo) ListByShop(_ context.Context, _ uint64, _, _ int) ([]Activity, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lists++
	return []Activity{r.item}, 1, nil
}
func (r *countingRepo) counts() (int, int) { r.mu.Lock(); defer r.mu.Unlock(); return r.finds, r.lists }

type redisCommandsFake struct{ get *redis.StringCmd }

func (f redisCommandsFake) Get(context.Context, string) *redis.StringCmd { return f.get }
func (f redisCommandsFake) Set(context.Context, string, any, time.Duration) *redis.StatusCmd {
	return redis.NewStatusResult("OK", nil)
}
func (f redisCommandsFake) Del(context.Context, ...string) *redis.IntCmd {
	return redis.NewIntResult(1, nil)
}
func (f redisCommandsFake) Eval(context.Context, string, []string, ...any) *redis.Cmd {
	return redis.NewCmdResult(int64(1), nil)
}

func TestRedisStoreMapsRedisNilToCouponMiss(t *testing.T) {
	store := NewRedisStore(redisCommandsFake{get: redis.NewStringResult("", redis.Nil)})
	if _, err := store.Get(context.Background(), "key"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("err=%v", err)
	}
}

func TestCachedRepositoryNegativeAndFallback(t *testing.T) {
	backend := &countingRepo{findErr: ErrActivityNotFound}
	store := newMemoryStore()
	cached, err := NewCachedRepository(backend, store, CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	for range 2 {
		if _, err := cached.FindByID(context.Background(), 9); !errors.Is(err, ErrActivityNotFound) {
			t.Fatalf("err=%v", err)
		}
	}
	finds, _ := backend.counts()
	if finds != 1 {
		t.Fatalf("negative cache finds=%d", finds)
	}
	backend.findErr = nil
	backend.item = Activity{ID: 10, ShopID: 1, Title: "ok"}
	store.getErr = errors.New("redis down")
	if got, err := cached.FindByID(context.Background(), 10); err != nil || got.ID != 10 {
		t.Fatalf("fallback=%+v %v", got, err)
	}
}
func TestCachedRepositoryHotDetailSingleflight(t *testing.T) {
	gate := make(chan struct{})
	backend := &countingRepo{item: Activity{ID: 1, ShopID: 2, Title: "hot"}, gate: gate}
	cached, _ := NewCachedRepository(backend, nil, CacheConfig{BackendTimeout: time.Second})
	defer cached.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := cached.FindByID(context.Background(), 1); errs <- e }()
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	finds, _ := backend.counts()
	if finds != 1 {
		t.Fatalf("singleflight loads=%d", finds)
	}
}
func TestCachedRepositoryInvalidatesDetailAndListGeneration(t *testing.T) {
	store := newMemoryStore()
	cached, _ := NewCachedRepository(&countingRepo{item: Activity{ID: 3, ShopID: 4, Title: "x"}}, store, CacheConfig{})
	defer cached.Close()
	if err := cached.Invalidate(context.Background(), 3, 4, 99); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.values[activityListGenerationKey(4)]; !ok {
		t.Fatal("list generation was not invalidated")
	}
	if got := store.values[activityListGenerationKey(4)]; got != "99" {
		t.Fatalf("generation=%q", got)
	}
}

func TestCachedRepositoryInvalidationRefreshesWarmListAndNeverRegressesGeneration(t *testing.T) {
	store := newMemoryStore()
	backend := &countingRepo{item: Activity{ID: 3, ShopID: 4, Title: "before"}}
	cached, _ := NewCachedRepository(backend, store, CacheConfig{})
	defer cached.Close()
	items, _, err := cached.ListByShop(context.Background(), 4, 0, 20)
	if err != nil || items[0].Title != "before" {
		t.Fatalf("warm list=%+v err=%v", items, err)
	}
	backend.mu.Lock()
	backend.item.Title = "after"
	backend.mu.Unlock()
	if err := cached.Invalidate(context.Background(), 3, 4, 20); err != nil {
		t.Fatal(err)
	}
	// A delayed older task must not move the list namespace backwards.
	if err := cached.Invalidate(context.Background(), 3, 4, 10); err != nil {
		t.Fatal(err)
	}
	items, _, err = cached.ListByShop(context.Background(), 4, 0, 20)
	if err != nil || items[0].Title != "after" {
		t.Fatalf("refreshed list=%+v err=%v", items, err)
	}
	if got := store.values[activityListGenerationKey(4)]; got != "20" {
		t.Fatalf("generation regressed to %q", got)
	}
	_, lists := backend.counts()
	if lists != 2 {
		t.Fatalf("backend list loads=%d", lists)
	}
}
