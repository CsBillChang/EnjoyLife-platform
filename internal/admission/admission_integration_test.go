package admission

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/redis/go-redis/v9"
)

func TestRedisAdmissionLifecycle(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run against local Redis")
	}
	addr := os.Getenv("LOCAL_LIFE_REDIS_ADDR")
	if addr == "" {
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		addr = cfg.RedisAddress
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("local Redis unavailable at %s: %v", addr, err)
	}
	prefix := "local-life:test:admission:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	s, err := New(client, prefix)
	if err != nil {
		t.Fatal(err)
	}
	// Use an unguessable, per-test prefix and delete only the keys we created;
	// never flush a developer's shared local Redis database.
	t.Cleanup(func() { _ = client.Del(context.Background(), s.activityKeys(1, 9)...).Err() })
	now := time.Now().UTC()
	if err := s.Initialize(ctx, ActivityState{ActivityID: 1, Generation: 9, StockHint: 1, Open: true, StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx, ActivityState{ActivityID: 1, Generation: 9, StockHint: 999, Open: true, StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute)}); !errors.Is(err, ErrStateAlreadyExists) {
		t.Fatalf("existing generation was destructively reinitialized: %v", err)
	}
	s.now = func() time.Time { return now }
	r := validRequest(now)
	r.ActivityID = 1
	r.Generation = 9
	if err := s.SetOpen(ctx, r.ActivityID, r.Generation, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, r); !errors.Is(err, ErrAdmissionDisabled) {
		t.Fatalf("closed admission = %v", err)
	}
	if err := s.SetOpen(ctx, r.ActivityID, r.Generation, true); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Admit(ctx, r); err != nil || got != Admitted {
		t.Fatalf("first admit = %q, %v", got, err)
	}
	if got, err := s.Admit(ctx, r); err != nil || got != Replay {
		t.Fatalf("replay = %q, %v", got, err)
	}
	r.RequestID = "request-b"
	if _, err := s.Admit(ctx, r); !errors.Is(err, ErrUserReserved) {
		t.Fatalf("second request error = %v", err)
	}
	r.RequestID = "request-a"
	if got, err := s.Release(ctx, r.ActivityID, r.Generation, r.UserID, r.RequestID); err != nil || got != Released {
		t.Fatalf("release = %q, %v", got, err)
	}
	if got, err := s.Release(ctx, r.ActivityID, r.Generation, r.UserID, r.RequestID); err != nil || got != Cancelled {
		t.Fatalf("second release = %q, %v", got, err)
	}
	if _, err := s.Admit(ctx, r); !errors.Is(err, ErrRequestCancelled) {
		t.Fatalf("late admission after release was not fenced: %v", err)
	}
	// A new request may use the released user's slot; the cancelled request can
	// never race back in and take it again.
	r.RequestID = "request-c"
	if got, err := s.Admit(ctx, r); err != nil || got != Admitted {
		t.Fatalf("new request after release = %q, %v", got, err)
	}
	if err := s.MarkOrdered(ctx, r.ActivityID, r.Generation, r.UserID, r.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release(ctx, r.ActivityID, r.Generation, r.UserID, r.RequestID); !errors.Is(err, ErrOrderAlreadyCommitted) {
		t.Fatalf("committed release = %v", err)
	}
}

func TestRedisReleaseFailsClosedOnMissingOrCorruptState(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run against local Redis")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, damage := range []string{"stock", "open", "user", "request", "releases", "all", "invalid-stock", "overflow"} {
		t.Run(damage, func(t *testing.T) {
			store, err := New(client, "local-life:test:release:"+strconv.FormatInt(time.Now().UnixNano(), 10))
			if err != nil {
				t.Fatal(err)
			}
			r := validRequest(time.Now().UTC())
			keys := store.activityKeys(r.ActivityID, r.Generation)
			t.Cleanup(func() { _ = client.Del(context.Background(), keys...).Err() })
			if err := store.Initialize(ctx, ActivityState{ActivityID: r.ActivityID, Generation: r.Generation, StockHint: 1, Open: true, StartsAt: r.StartsAt, EndsAt: r.EndsAt}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Admit(ctx, r); err != nil {
				t.Fatal(err)
			}
			var damageErr error
			switch damage {
			case "stock":
				damageErr = client.Del(ctx, keys[0]).Err()
			case "open":
				damageErr = client.Del(ctx, keys[1]).Err()
			case "user":
				damageErr = client.HDel(ctx, keys[2], r.UserKey()).Err()
			case "request":
				damageErr = client.HDel(ctx, keys[3], r.RequestID).Err()
			case "releases":
				damageErr = client.Del(ctx, keys[4]).Err()
			case "all":
				damageErr = client.Del(ctx, keys...).Err()
			case "invalid-stock":
				damageErr = client.Set(ctx, keys[0], "not-stock", 0).Err()
			case "overflow":
				damageErr = client.Set(ctx, keys[0], "9223372036854775807", 0).Err()
			}
			if damageErr != nil {
				t.Fatal(damageErr)
			}
			// DUMP captures the exact remaining representation. A failed release
			// must preserve the evidence and may not recreate even an empty key.
			before := make([]string, len(keys))
			for i, key := range keys {
				before[i], err = client.Dump(ctx, key).Result()
				if err != nil && !errors.Is(err, redis.Nil) {
					t.Fatal(err)
				}
			}
			want := ErrStatePartial
			if damage == "all" {
				want = ErrStateMissing
			}
			for range 2 {
				if result, err := store.Release(ctx, r.ActivityID, r.Generation, r.UserID, r.RequestID); !errors.Is(err, want) || result != Noop {
					t.Fatalf("release=%s err=%v want=%v", result, err, want)
				}
			}
			for i, key := range keys {
				after, err := client.Dump(ctx, key).Result()
				if err != nil && !errors.Is(err, redis.Nil) {
					t.Fatal(err)
				}
				if after != before[i] {
					t.Fatalf("failed release mutated key %d", i)
				}
			}
		})
	}
}

func TestRedisRestoreCommittedStockIsIdempotentAndKeepsEligibility(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run against local Redis")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := New(client, "local-life:test:restore:"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err != nil {
		t.Fatal(err)
	}
	r := validRequest(time.Now().UTC())
	keys := store.activityKeys(r.ActivityID, r.Generation)
	t.Cleanup(func() { _ = client.Del(context.Background(), keys...).Err() })
	if err := store.Initialize(ctx, ActivityState{ActivityID: r.ActivityID, Generation: r.Generation, StockHint: 1, Open: true, StartsAt: r.StartsAt, EndsAt: r.EndsAt}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOrdered(ctx, r.ActivityID, r.Generation, r.UserID, r.RequestID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []Result{Released, Replay, Replay} {
		if result, err := store.RestoreCommittedStock(ctx, r.ActivityID, r.Generation, 42); err != nil || result != want {
			t.Fatalf("restore=%s err=%v want=%s", result, err, want)
		}
	}
	if stock, err := client.Get(ctx, keys[0]).Int(); err != nil || stock != 1 {
		t.Fatalf("duplicate restoration: stock=%d err=%v", stock, err)
	}
	r.RequestID = "second-purchase"
	if _, err := store.Admit(ctx, r); !errors.Is(err, ErrUserReserved) {
		t.Fatalf("restoration restored old user's eligibility: %v", err)
	}
	r.UserID++
	if _, err := store.Admit(ctx, r); err != nil {
		t.Fatalf("restored slot unavailable to new user: %v", err)
	}
	if err := client.Del(ctx, keys[0]).Err(); err != nil {
		t.Fatal(err)
	}
	for _, orderID := range []uint64{42, 43} {
		if result, err := store.RestoreCommittedStock(ctx, r.ActivityID, r.Generation, orderID); result != Noop || !errors.Is(err, ErrStatePartial) {
			t.Fatalf("missing stock restore=%s err=%v", result, err)
		}
	}
	if exists, err := client.Exists(ctx, keys[0]).Result(); err != nil || exists != 0 {
		t.Fatalf("restore recreated missing stock: exists=%d err=%v", exists, err)
	}
	if exists, err := client.HExists(ctx, keys[4], "43").Result(); err != nil || exists {
		t.Fatalf("failed restoration wrote receipt: exists=%v err=%v", exists, err)
	}
	// Losing only the receipt hash must also fail closed. Otherwise a replay
	// of order 42 could silently add its already restored unit a second time.
	if err := client.Set(ctx, keys[0], "0", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Del(ctx, keys[4]).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RestoreCommittedStock(ctx, r.ActivityID, r.Generation, 42); !errors.Is(err, ErrStatePartial) {
		t.Fatalf("missing restoration receipts accepted replay: %v", err)
	}
	if stock, err := client.Get(ctx, keys[0]).Int(); err != nil || stock != 0 {
		t.Fatalf("missing receipts changed stock=%d err=%v", stock, err)
	}
}
