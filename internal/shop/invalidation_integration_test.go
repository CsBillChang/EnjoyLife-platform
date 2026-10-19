package shop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// This test uses a connection-local clone, leaving the real recovery_tasks
// untouched. It verifies MySQL lease/generation fencing and the Redis half of
// CachedRepository.Invalidate together; enable it explicitly for local Docker.
func isolatedRecoveryTasksDB(t *testing.T) (*sql.DB, *gorm.DB) {
	t.Helper()
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run MySQL+Redis invalidation test")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var table, definition string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE recovery_tasks").Scan(&table, &definition); err != nil {
		t.Fatalf("read recovery_tasks schema: %v", err)
	}
	const create = "CREATE TABLE `recovery_tasks`"
	if table != "recovery_tasks" || !strings.HasPrefix(definition, create) {
		t.Fatal("unexpected recovery_tasks schema")
	}
	definition = strings.Replace(definition, create, "CREATE TEMPORARY TABLE `recovery_tasks`", 1)
	if _, err := db.ExecContext(ctx, definition); err != nil {
		t.Fatalf("clone recovery_tasks: %v", err)
	}
	gdb, err := gorm.Open(mysql.New(mysql.Config{Conn: db}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return db, gdb
}

func TestMySQLRedisShopInvalidationFencesSupersededGeneration(t *testing.T) {
	db, gdb := isolatedRecoveryTasksDB(t)
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	redisClient := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	t.Cleanup(func() { _ = redisClient.Close() })
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect Redis: %v", err)
	}
	shopID := int64(9_000_000_000 + time.Now().UnixNano()%1_000_000)
	key := shopCacheKey(shopID)
	t.Cleanup(func() { _ = redisClient.Del(context.Background(), key).Err() })
	cached, err := NewCachedRepository(NewMemoryRepository([]Shop{{ID: shopID, Name: "fixture", Status: "OPEN"}}), NewRedisStore(redisClient), CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cached.Close)
	if _, err := cached.FindByID(ctx, shopID); err != nil {
		t.Fatal(err)
	}
	if _, err := redisClient.Get(ctx, key).Result(); err != nil {
		t.Fatalf("cache was not populated: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	payload, err := jsonPayload(shopID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recovery_tasks (kind, source_id, generation, status, attempts, available_at, payload) VALUES (?, ?, 1, 'PENDING', 0, ?, ?)`, shopCacheInvalidationKind, fmt.Sprint(shopID), now, payload); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewInvalidationDispatcher(gdb, cached, InvalidationConfig{LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	old, err := dispatcher.ClaimOne(ctx, now)
	if err != nil || old == nil {
		t.Fatalf("claim old=%+v err=%v", old, err)
	}
	newPayload, err := jsonPayload(shopID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertShopCacheInvalidation(gdb.WithContext(ctx), shopID, 2, newPayload); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.markSucceeded(ctx, *old, now); err != ErrInvalidationLeaseLost {
		t.Fatalf("old task overwrote newer generation: %v", err)
	}
	worked, err := dispatcher.DispatchOne(ctx, now.Add(time.Second))
	if err != nil || !worked {
		t.Fatalf("dispatch new worked=%v err=%v", worked, err)
	}
	if _, err := redisClient.Get(ctx, key).Result(); err == nil {
		t.Fatal("Redis detail survived invalidation")
	}
	var generation uint64
	var status string
	if err := db.QueryRowContext(ctx, `SELECT generation, status FROM recovery_tasks WHERE kind = ? AND source_id = ?`, shopCacheInvalidationKind, fmt.Sprint(shopID)).Scan(&generation, &status); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || status != "SUCCEEDED" {
		t.Fatalf("task = generation %d status %s", generation, status)
	}
}

func jsonPayload(id int64, generation uint64) ([]byte, error) {
	return json.Marshal(shopCacheInvalidationPayload{ShopID: id, Generation: generation})
}

type recordingShopInvalidator struct{ ids []int64 }

func (f *recordingShopInvalidator) Invalidate(_ context.Context, id int64) error {
	f.ids = append(f.ids, id)
	return nil
}

func TestMySQLShopInvalidationPoisonDoesNotStarveHealthyTasks(t *testing.T) {
	for _, bad := range []string{`{}`, `null`, `[]`, `{"shop_id":"bad","generation":1}`, `{"shop_id":10,"generation":9}`} {
		t.Run(bad, func(t *testing.T) {
			db, gdb := isolatedRecoveryTasksDB(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			good, err := jsonPayload(20, 1)
			if err != nil {
				t.Fatal(err)
			}
			for i, payload := range []string{bad, string(good)} {
				if _, err := db.Exec(`INSERT INTO recovery_tasks (kind,source_id,generation,status,available_at,payload) VALUES (?, ?, 1, 'PENDING', ?, ?)`, shopCacheInvalidationKind, fmt.Sprint(i+1), now, payload); err != nil {
					t.Fatal(err)
				}
			}
			inv := &recordingShopInvalidator{}
			dispatcher, err := NewInvalidationDispatcher(gdb, inv, InvalidationConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if worked, err := dispatcher.DispatchOne(context.Background(), now); err == nil || worked {
				t.Fatalf("invalid task worked=%v err=%v", worked, err)
			}
			var status string
			var reason sql.NullString
			var lease sql.NullTime
			if err := db.QueryRow(`SELECT status,last_error,lease_until FROM recovery_tasks WHERE source_id='1'`).Scan(&status, &reason, &lease); err != nil || status != "FAILED" || !reason.Valid || reason.String == "" || lease.Valid {
				t.Fatalf("poison status=%s reason=%v lease=%v err=%v", status, reason, lease, err)
			}
			if worked, err := dispatcher.DispatchOne(context.Background(), now); err != nil || !worked || len(inv.ids) != 1 || inv.ids[0] != 20 {
				t.Fatalf("healthy task worked=%v calls=%v err=%v", worked, inv.ids, err)
			}
		})
	}
}

func TestMySQLShopInvalidationPreservesActiveFinalLease(t *testing.T) {
	db, gdb := isolatedRecoveryTasksDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	payload, _ := jsonPayload(30, 1)
	if _, err := db.Exec(`INSERT INTO recovery_tasks (kind,source_id,generation,status,available_at,payload) VALUES (?, '30', 1, 'PENDING', ?, ?)`, shopCacheInvalidationKind, now, payload); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewInvalidationDispatcher(gdb, &recordingShopInvalidator{}, InvalidationConfig{LeaseDuration: time.Second, MaxAttempts: 1})
	task, err := dispatcher.ClaimOne(context.Background(), now)
	if err != nil || task == nil {
		t.Fatalf("claim=%+v err=%v", task, err)
	}
	if next, err := dispatcher.ClaimOne(context.Background(), now); err != nil || next != nil {
		t.Fatalf("active final lease=%+v err=%v", next, err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM recovery_tasks WHERE source_id='30'`).Scan(&status); err != nil || status != "RUNNING" {
		t.Fatalf("active status=%s err=%v", status, err)
	}
	if next, err := dispatcher.ClaimOne(context.Background(), task.LeaseUntil); err != nil || next != nil {
		t.Fatalf("expired final lease=%+v err=%v", next, err)
	}
	if err := db.QueryRow(`SELECT status FROM recovery_tasks WHERE source_id='30'`).Scan(&status); err != nil || status != "FAILED" {
		t.Fatalf("expired status=%s err=%v", status, err)
	}
	if err := dispatcher.markSucceeded(context.Background(), *task, task.LeaseUntil); !errors.Is(err, ErrInvalidationLeaseLost) {
		t.Fatalf("expired completion=%v", err)
	}
}
