package admission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/redis/go-redis/v9"
)

func admissionRedisFixture(t *testing.T) (context.Context, *redis.Client, *Store, Request) {
	t.Helper()
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run against local Redis")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	addr := os.Getenv("LOCAL_LIFE_REDIS_ADDR")
	if addr == "" {
		addr = cfg.RedisAddress
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = client.Close() })
	now, err := client.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(client, fmt.Sprintf("local-life:test:metadata:%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	r := validRequest(now)
	r.AcceptedAt = r.AcceptedAt.Truncate(time.Microsecond)
	r.StartsAt = r.StartsAt.Truncate(time.Microsecond)
	r.EndsAt = r.EndsAt.Truncate(time.Microsecond)
	r.ProcessingDeadline = r.ProcessingDeadline.Truncate(time.Microsecond)
	keys := append(store.activityKeys(r.ActivityID, r.Generation), store.activityKeys(r.ActivityID, r.Generation)[0]+":rebuild")
	t.Cleanup(func() { _ = client.Del(context.Background(), keys...).Err() })
	return ctx, client, store, r
}

func initializeRequest(t *testing.T, ctx context.Context, store *Store, r Request) {
	t.Helper()
	if err := store.Initialize(ctx, ActivityState{ActivityID: r.ActivityID, Generation: r.Generation, StockHint: 3, Open: true, StartsAt: r.StartsAt, EndsAt: r.EndsAt}); err != nil {
		t.Fatal(err)
	}
}

func dumpAdmission(t *testing.T, ctx context.Context, client *redis.Client, keys []string) []string {
	t.Helper()
	values := make([]string, len(keys))
	for i, key := range keys {
		var err error
		values[i], err = client.Dump(ctx, key).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			t.Fatal(err)
		}
	}
	return values
}

func assertAdmissionUnchanged(t *testing.T, ctx context.Context, client *redis.Client, keys []string, before []string) {
	t.Helper()
	after := dumpAdmission(t, ctx, client, keys)
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("failed script changed key %s", keys[i])
		}
	}
}

func TestRedisAdmissionLuaChecksWindowAndDeadlineWithoutGoValidation(t *testing.T) {
	for _, scenario := range []string{"before start", "at start", "before end", "at end", "expired", "after natural end", "large generation"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, client, store, r := admissionRedisFixture(t)
			want := "ADMITTED"
			switch scenario {
			case "before start":
				r.AcceptedAt = r.StartsAt.Add(-time.Microsecond)
				want = "ACCEPTED_OUTSIDE_WINDOW"
			case "at start":
				r.AcceptedAt = r.StartsAt
			case "before end":
				r.AcceptedAt = r.EndsAt.Add(-time.Microsecond)
				r.ProcessingDeadline = r.EndsAt.Add(time.Minute)
			case "at end":
				r.AcceptedAt = r.EndsAt
				r.ProcessingDeadline = r.EndsAt.Add(time.Minute)
				want = "ACCEPTED_OUTSIDE_WINDOW"
			case "expired":
				r.ProcessingDeadline = r.AcceptedAt.Add(time.Microsecond)
				want = "PROCESSING_EXPIRED"
			case "after natural end":
				r.EndsAt = r.AcceptedAt.Add(time.Microsecond)
			case "large generation":
				r.Generation = 18446744073709551615
			}
			keys := store.activityKeys(r.ActivityID, r.Generation)
			t.Cleanup(func() { _ = client.Del(context.Background(), keys...).Err() })
			initializeRequest(t, ctx, store, r)
			before := dumpAdmission(t, ctx, client, keys)
			args := []any{r.UserKey(), r.RequestID, strconv.FormatUint(r.Generation, 10)}
			args = append(args, timeArgs(r.AcceptedAt)...)
			args = append(args, timeArgs(r.ProcessingDeadline)...)
			got, err := client.Eval(ctx, admitLua, keys, args...).Text()
			if err != nil || got != want {
				t.Fatalf("result=%s err=%v want=%s", got, err, want)
			}
			if want != "ADMITTED" {
				assertAdmissionUnchanged(t, ctx, client, keys, before)
			}
		})
	}
}

func TestRedisAdmissionRechecksClockAfterGoPrecheck(t *testing.T) {
	ctx, client, store, r := admissionRedisFixture(t)
	r.ProcessingDeadline = r.AcceptedAt.Add(time.Microsecond)
	store.now = func() time.Time { return r.AcceptedAt }
	initializeRequest(t, ctx, store, r)
	keys := store.activityKeys(r.ActivityID, r.Generation)
	before := dumpAdmission(t, ctx, client, keys)
	if err := store.validate(r); err != nil {
		t.Fatalf("Go precheck should pass: %v", err)
	}
	if _, err := store.Admit(ctx, r); !errors.Is(err, ErrProcessingDeadline) {
		t.Fatalf("Redis must reject stale Go clock: %v", err)
	}
	assertAdmissionUnchanged(t, ctx, client, keys, before)
}

func TestRedisAdmissionMetadataDamageFailsClosed(t *testing.T) {
	for _, damage := range []string{"meta", "schema", "generation", "starts_sec", "ends_usec", "users", "requests", "user-sentinel", "request-sentinel", "release-sentinel", "wrong-type"} {
		t.Run(damage, func(t *testing.T) {
			ctx, client, store, r := admissionRedisFixture(t)
			initializeRequest(t, ctx, store, r)
			keys := store.activityKeys(r.ActivityID, r.Generation)
			want := ErrStatePartial
			var err error
			switch damage {
			case "meta":
				err = client.Del(ctx, keys[5]).Err()
			case "schema":
				err = client.HSet(ctx, keys[5], "schema", "1").Err()
			case "generation":
				err = client.HSet(ctx, keys[5], "generation", "3").Err()
				want = ErrGenerationMismatch
			case "starts_sec", "ends_usec":
				err = client.HDel(ctx, keys[5], damage).Err()
			case "users":
				err = client.Del(ctx, keys[2]).Err()
			case "requests":
				err = client.Del(ctx, keys[3]).Err()
			case "user-sentinel":
				err = client.HDel(ctx, keys[2], "__initialized").Err()
			case "request-sentinel":
				err = client.HDel(ctx, keys[3], "__initialized").Err()
			case "release-sentinel":
				err = client.HDel(ctx, keys[4], "__initialized").Err()
			case "wrong-type":
				err = client.Set(ctx, keys[2], "bad", 0).Err()
			}
			if err != nil {
				t.Fatal(err)
			}
			before := dumpAdmission(t, ctx, client, keys)
			if _, err := store.Admit(ctx, r); !errors.Is(err, want) {
				t.Fatalf("admit err=%v want=%v", err, want)
			}
			if err := store.SetOpen(ctx, r.ActivityID, r.Generation, true); !errors.Is(err, want) {
				t.Fatalf("reopen err=%v want=%v", err, want)
			}
			if _, err := store.Release(ctx, r.ActivityID, r.Generation, r.UserID, r.RequestID); !errors.Is(err, want) {
				t.Fatalf("release err=%v want=%v", err, want)
			}
			if _, err := store.RestoreCommittedStock(ctx, r.ActivityID, r.Generation, 1); !errors.Is(err, want) {
				t.Fatalf("restore err=%v want=%v", err, want)
			}
			assertAdmissionUnchanged(t, ctx, client, keys, before)
			if err := store.SetOpen(ctx, r.ActivityID, r.Generation, false); err != nil {
				t.Fatalf("must allow closing damaged/legacy state: %v", err)
			}
		})
	}
}

func TestRedisRebuildPublishesAndVerifiesTimeMetadata(t *testing.T) {
	ctx, client, store, r := admissionRedisFixture(t)
	snapshot := RebuildSnapshot{ActivityID: r.ActivityID, Generation: r.Generation, StockHint: 3, StartsAt: r.StartsAt, EndsAt: r.EndsAt, Owners: []CommittedOwner{{UserID: 99, RequestID: "historical-order"}}}
	if err := store.InstallRebuild(ctx, snapshot, "rebuild-test"); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyRebuild(ctx, snapshot, "rebuild-test"); err != nil {
		t.Fatal(err)
	}
	keys := store.activityKeys(r.ActivityID, r.Generation)
	if err := client.HSet(ctx, keys[5], "ends_usec", strconv.Itoa((r.EndsAt.Nanosecond()/1000+1)%1000000)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishRebuild(ctx, snapshot, "rebuild-test"); !errors.Is(err, ErrSnapshotMismatch) {
		t.Fatalf("corrupt window published: %v", err)
	}
	if open, err := client.Get(ctx, keys[1]).Result(); err != nil || open != "0" {
		t.Fatalf("failed publish opened traffic: %s %v", open, err)
	}
	if err := store.InstallRebuild(ctx, snapshot, "rebuild-test"); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishRebuild(ctx, snapshot, "rebuild-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	// A delayed installer must not erase a reservation after publication.
	before := dumpAdmission(t, ctx, client, keys)
	if err := store.InstallRebuild(ctx, snapshot, "rebuild-test"); err != nil {
		t.Fatal(err)
	}
	assertAdmissionUnchanged(t, ctx, client, keys, before)
	if err := client.HSet(ctx, keys[5], "schema", "1").Err(); err != nil {
		t.Fatal(err)
	}
	before = dumpAdmission(t, ctx, client, keys)
	if err := store.InstallRebuild(ctx, snapshot, "rebuild-test"); !errors.Is(err, ErrSnapshotMismatch) {
		t.Fatalf("published state with invalid metadata accepted: %v", err)
	}
	assertAdmissionUnchanged(t, ctx, client, keys, before)
}
