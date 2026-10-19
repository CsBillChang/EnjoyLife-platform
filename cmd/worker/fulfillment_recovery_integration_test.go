package main

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

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

// One connection keeps the temporary tables shadowing their live names for
// every query. No recovery task or activity in the configured database is edited.
func isolatedStockRestoreDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 for MySQL stock-restore recovery tests")
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
	var name, definition string
	if err := db.QueryRow(`SHOW CREATE TABLE recovery_tasks`).Scan(&name, &definition); err != nil {
		t.Fatal(err)
	}
	const prefix = "CREATE TABLE `recovery_tasks`"
	if name != "recovery_tasks" || !strings.HasPrefix(definition, prefix) {
		t.Fatal("unexpected recovery_tasks definition")
	}
	if _, err := db.Exec(strings.Replace(definition, prefix, "CREATE TEMPORARY TABLE `recovery_tasks`", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TEMPORARY TABLE coupon_activities (id BIGINT UNSIGNED PRIMARY KEY, generation BIGINT UNSIGNED NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertStockRestoreTask(t *testing.T, db *sql.DB, source string, generation uint64, payload string) {
	t.Helper()
	// Make fixtures explicitly ready on the database clock, independent of any
	// clock skew between the host running Go and the disposable MySQL instance.
	if _, err := db.Exec(`INSERT INTO recovery_tasks (kind,source_id,generation,status,available_at,payload) VALUES (?, ?, ?, 'PENDING', CURRENT_TIMESTAMP(6)-INTERVAL 1 SECOND, ?)`, order.StockRestoreTaskKind, source, generation, payload); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLStockRestorePoisonDoesNotStarveHealthyTasks(t *testing.T) {
	for _, bad := range []string{`{}`, `null`, `[]`, `{"activity_id":"bad","order_id":1,"generation":1}`, `{"activity_id":10,"order_id":1,"generation":9}`} {
		t.Run(bad, func(t *testing.T) {
			db := isolatedStockRestoreDB(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			if _, err := db.Exec(`INSERT INTO coupon_activities VALUES (10,1)`); err != nil {
				t.Fatal(err)
			}
			good, _ := json.Marshal(order.StockRestorePayload{ActivityID: 10, Generation: 1, OrderID: 2})
			for i, payload := range []string{bad, string(good)} {
				insertStockRestoreTask(t, db, fmt.Sprint(i+1), 1, payload)
			}
			repository := stockRestoreRepository{db: db}
			restorer := &stockRestorerFake{}
			if err := recoverCommittedStock(context.Background(), repository, restorer, 10, now); err == nil {
				t.Fatal("invalid task was not reported")
			}
			var status string
			var reason sql.NullString
			var lease sql.NullTime
			if err := db.QueryRow(`SELECT status,last_error,lease_until FROM recovery_tasks WHERE source_id='1'`).Scan(&status, &reason, &lease); err != nil || status != "FAILED" || !reason.Valid || reason.String == "" || lease.Valid {
				t.Fatalf("poison status=%s reason=%v lease=%v err=%v", status, reason, lease, err)
			}
			if err := recoverCommittedStock(context.Background(), repository, restorer, 10, now); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT status FROM recovery_tasks WHERE source_id='2'`).Scan(&status); err != nil || status != "SUCCEEDED" || restorer.calls != 1 {
				t.Fatalf("healthy status=%s calls=%d err=%v", status, restorer.calls, err)
			}
		})
	}
}

func claimStockRestoreFixture(t *testing.T, db *sql.DB) (stockRestoreRepository, stockRestoreTask) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	payload, _ := json.Marshal(order.StockRestorePayload{ActivityID: 10, Generation: 1, OrderID: 20})
	insertStockRestoreTask(t, db, "20", 1, string(payload))
	repository := stockRestoreRepository{db: db}
	task, found, err := repository.Claim(context.Background(), now)
	if err != nil || !found {
		t.Fatalf("claim found=%v task=%+v err=%v", found, task, err)
	}
	return repository, task
}

func expireStockRestoreLease(t *testing.T, db *sql.DB, id uint64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE recovery_tasks SET lease_until=CURRENT_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLStockRestoreLeaseFencesStaleOwners(t *testing.T) {
	for _, operation := range []string{"success", "retry"} {
		t.Run(operation, func(t *testing.T) {
			db := isolatedStockRestoreDB(t)
			repository, old := claimStockRestoreFixture(t, db)
			expireStockRestoreLease(t, db, old.ID)
			current, found, err := repository.Claim(context.Background(), time.Now())
			if err != nil || !found || current.Attempts != old.Attempts+1 || current.leaseToken == "" || current.leaseToken == old.leaseToken {
				t.Fatalf("reclaim found=%v task=%+v err=%v", found, current, err)
			}
			if operation == "success" {
				err = repository.Succeed(context.Background(), old)
			} else {
				err = repository.Retry(context.Background(), old, time.Now(), fmt.Errorf("old owner failed"))
			}
			if !errors.Is(err, errStockRestoreLeaseLost) {
				t.Fatalf("stale owner error=%v", err)
			}
			var status string
			var attempts uint64
			var reason sql.NullString
			if err := db.QueryRow(`SELECT status,attempts,last_error FROM recovery_tasks WHERE id=?`, current.ID).Scan(&status, &attempts, &reason); err != nil || status != "RUNNING" || attempts != current.Attempts || reason.Valid {
				t.Fatalf("current lease status=%s attempts=%d reason=%v err=%v", status, attempts, reason, err)
			}
			if err := repository.Succeed(context.Background(), current); err != nil {
				t.Fatalf("current owner could not complete: %v", err)
			}
			if err := repository.Retry(context.Background(), old, time.Now(), fmt.Errorf("late failure")); !errors.Is(err, errStockRestoreLeaseLost) {
				t.Fatalf("late retry error=%v", err)
			}
		})
	}
}

func TestMySQLStockRestoreExpiredLeaseCannotFinish(t *testing.T) {
	for _, operation := range []string{"success", "retry"} {
		t.Run(operation, func(t *testing.T) {
			db := isolatedStockRestoreDB(t)
			repository, task := claimStockRestoreFixture(t, db)
			expireStockRestoreLease(t, db, task.ID)
			var err error
			if operation == "success" {
				err = repository.Succeed(context.Background(), task)
			} else {
				err = repository.Retry(context.Background(), task, time.Now(), fmt.Errorf("expired owner"))
			}
			if !errors.Is(err, errStockRestoreLeaseLost) {
				t.Fatalf("expired owner error=%v", err)
			}
		})
	}
}

func TestMySQLStockRestoreLiveLeaseCanRetryAndComplete(t *testing.T) {
	db := isolatedStockRestoreDB(t)
	repository, first := claimStockRestoreFixture(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repository.Retry(context.Background(), first, now, fmt.Errorf("transient Redis error")); err != nil {
		t.Fatal(err)
	}
	var status string
	var delayMicros int64
	var lease sql.NullTime
	if err := db.QueryRow(`SELECT status,TIMESTAMPDIFF(MICROSECOND,CURRENT_TIMESTAMP(6),available_at),lease_until FROM recovery_tasks WHERE id=?`, first.ID).Scan(&status, &delayMicros, &lease); err != nil || status != "PENDING" || delayMicros <= 0 || delayMicros > 2_000_000 || lease.Valid {
		t.Fatalf("retry status=%s database delay=%d us lease=%v err=%v", status, delayMicros, lease, err)
	}
	if _, err := db.Exec(`UPDATE recovery_tasks SET available_at=CURRENT_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, found, err := repository.Claim(context.Background(), time.Now())
	if err != nil || !found || second.Attempts != first.Attempts+1 {
		t.Fatalf("retry claim=%+v found=%v err=%v", second, found, err)
	}
	if err := repository.Succeed(context.Background(), second); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLStockRestoreLeaseChecksEveryIdentityField(t *testing.T) {
	db := isolatedStockRestoreDB(t)
	repository, current := claimStockRestoreFixture(t, db)
	for _, field := range []string{"generation", "attempt", "token", "missing-token"} {
		t.Run(field, func(t *testing.T) {
			stale := current
			switch field {
			case "generation":
				stale.Generation++
			case "attempt":
				stale.Attempts++
			case "token":
				stale.leaseToken = "another-owner"
			case "missing-token":
				stale.leaseToken = ""
			}
			if err := repository.Succeed(context.Background(), stale); !errors.Is(err, errStockRestoreLeaseLost) {
				t.Fatalf("mismatched %s success error=%v", field, err)
			}
			if err := repository.Retry(context.Background(), stale, time.Now(), fmt.Errorf("wrong identity")); !errors.Is(err, errStockRestoreLeaseLost) {
				t.Fatalf("mismatched %s retry error=%v", field, err)
			}
		})
	}
	if err := repository.Succeed(context.Background(), current); err != nil {
		t.Fatalf("valid owner after rejected updates: %v", err)
	}
}

func TestMySQLRedisStockRestoreCrashReplayPreservesPurchaseLimit(t *testing.T) {
	db := isolatedStockRestoreDB(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("local-life:test:worker-restore:%d", time.Now().UnixNano())
	base := prefix + ":{10:1}"
	t.Cleanup(func() {
		_ = client.Del(context.Background(), base+":stock", base+":open", base+":users", base+":requests", base+":stock:releases", base+":meta").Err()
	})
	store, err := admission.New(client, prefix)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	starts, ends := now.Add(-time.Minute), now.Add(time.Hour)
	if err := store.Initialize(ctx, admission.ActivityState{ActivityID: 10, Generation: 1, StockHint: 1, Open: true, StartsAt: starts, EndsAt: ends}); err != nil {
		t.Fatal(err)
	}
	request := admission.Request{ActivityID: 10, Generation: 1, UserID: 55, RequestID: "ordered-request", AcceptedAt: now, ProcessingDeadline: now.Add(time.Minute), StartsAt: starts, EndsAt: ends}
	if _, err := store.Admit(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOrdered(ctx, 10, 1, 55, request.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO coupon_activities VALUES (10,1)`); err != nil {
		t.Fatal(err)
	}
	repository, old := claimStockRestoreFixture(t, db)
	if result, err := store.RestoreCommittedStock(ctx, 10, 1, 20); err != nil || result != admission.Released {
		t.Fatalf("first restore=%s err=%v", result, err)
	}
	// Crash after the Redis side effect, before marking the durable task done.
	expireStockRestoreLease(t, db, old.ID)
	if err := recoverCommittedStock(ctx, repository, store, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := repository.Succeed(ctx, old); !errors.Is(err, errStockRestoreLeaseLost) {
		t.Fatalf("late pre-crash success=%v", err)
	}
	stock, err := client.Get(ctx, base+":stock").Int()
	if err != nil || stock != 1 {
		t.Fatalf("stock was restored more than once: stock=%d err=%v", stock, err)
	}
	request.RequestID = "second-purchase"
	if _, err := store.Admit(ctx, request); !errors.Is(err, admission.ErrUserReserved) {
		t.Fatalf("refund/close restored purchase eligibility: %v", err)
	}
	var status string
	var attempts uint64
	if err := db.QueryRow(`SELECT status,attempts FROM recovery_tasks WHERE id=?`, old.ID).Scan(&status, &attempts); err != nil || status != "SUCCEEDED" || attempts != 2 {
		t.Fatalf("recovered task status=%s attempts=%d err=%v", status, attempts, err)
	}
}

func TestMySQLStockRestoreClaimUsesDatabaseClock(t *testing.T) {
	for _, skew := range []time.Duration{time.Hour, -time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			db := isolatedStockRestoreDB(t)
			repository, first := claimStockRestoreFixture(t, db)
			if _, err := db.Exec(`UPDATE recovery_tasks SET available_at=CURRENT_TIMESTAMP(6)-INTERVAL 2 HOUR WHERE id=?`, first.ID); err != nil {
				t.Fatal(err)
			}
			wantFound := skew < 0
			if wantFound {
				expireStockRestoreLease(t, db, first.ID)
			}
			task, found, err := repository.Claim(context.Background(), time.Now().Add(skew))
			if err != nil || found != wantFound {
				t.Fatalf("app clock skew=%s found=%v want=%v task=%+v err=%v", skew, found, wantFound, task, err)
			}
			if wantFound && (task.Attempts != first.Attempts+1 || task.leaseToken == first.leaseToken) {
				t.Fatalf("expired task did not get a new owner: %+v", task)
			}
		})
	}
}

func TestMySQLStockRestoreRetryUsesDatabaseClock(t *testing.T) {
	for _, skew := range []time.Duration{time.Hour, -time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			db := isolatedStockRestoreDB(t)
			repository, task := claimStockRestoreFixture(t, db)
			if err := repository.Retry(context.Background(), task, time.Now().Add(skew), fmt.Errorf("retry fixture")); err != nil {
				t.Fatal(err)
			}
			var delayMicros int64
			if err := db.QueryRow(`SELECT TIMESTAMPDIFF(MICROSECOND,CURRENT_TIMESTAMP(6),available_at) FROM recovery_tasks WHERE id=?`, task.ID).Scan(&delayMicros); err != nil || delayMicros <= 0 || delayMicros > 2_000_000 {
				t.Fatalf("app clock skew=%s database delay=%d us err=%v", skew, delayMicros, err)
			}
		})
	}
}

// Row-lock tests need multiple connections; temporary tables are connection
// local. Only an explicitly authorized, uniquely named disposable schema is used.
func isolatedConcurrentStockRestoreDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LOCAL_LIFE_MIGRATION_TEST_DSN")
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" || dsn == "" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 and LOCAL_LIFE_MIGRATION_TEST_DSN for disposable-schema lease lock tests")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("huixiang_stock_lease_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("drop owned lease test schema: %v", err)
		}
	})
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	// Reuse the migrated table definition, without writing configured task rows.
	source := isolatedStockRestoreDB(t)
	var table, definition string
	if err := source.QueryRow(`SHOW CREATE TABLE recovery_tasks`).Scan(&table, &definition); err != nil {
		t.Fatal(err)
	}
	const prefix = "CREATE TEMPORARY TABLE `recovery_tasks`"
	if table != "recovery_tasks" || !strings.HasPrefix(definition, prefix) {
		t.Fatal("unexpected temporary recovery_tasks definition")
	}
	if _, err := db.Exec(strings.Replace(definition, prefix, "CREATE TABLE `recovery_tasks`", 1)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMySQLStockRestoreRechecksLeaseAfterRowLock(t *testing.T) {
	for _, scenario := range []struct {
		operation    string
		replaceOwner bool
	}{{"success", false}, {"retry", false}, {"success", true}, {"retry", true}} {
		t.Run(fmt.Sprintf("%s/replaced_%t", scenario.operation, scenario.replaceOwner), func(t *testing.T) {
			db := isolatedConcurrentStockRestoreDB(t)
			repository, task := claimStockRestoreFixture(t, db)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := db.ExecContext(ctx, `UPDATE recovery_tasks SET lease_until=CURRENT_TIMESTAMP(6)+INTERVAL 2 SECOND WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			var id uint64
			if err := blocker.QueryRowContext(ctx, `SELECT id FROM recovery_tasks WHERE id=? FOR UPDATE`, task.ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if scenario.operation == "success" {
					done <- repository.Succeed(ctx, task)
				} else {
					done <- repository.Retry(ctx, task, time.Now(), fmt.Errorf("blocked retry"))
				}
			}()
			// Observe an actual InnoDB row-lock wait before expiry. A sleep alone
			// cannot prove that the finalizer's SQL started while its lease was live.
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var waits int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.data_lock_waits w
					JOIN performance_schema.threads th ON th.THREAD_ID=w.REQUESTING_THREAD_ID
					WHERE th.PROCESSLIST_DB=DATABASE()`).Scan(&waits); err != nil {
					t.Fatal(err)
				}
				if waits != 0 {
					var live bool
					if err := db.QueryRowContext(ctx, `SELECT lease_until>CURRENT_TIMESTAMP(6) FROM recovery_tasks WHERE id=?`, task.ID).Scan(&live); err != nil || !live {
						t.Fatalf("lock wait was not observed before expiry: live=%v err=%v", live, err)
					}
					break
				}
				select {
				case err := <-done:
					t.Fatalf("finalizer returned without waiting for row lock: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if scenario.replaceOwner {
				if _, err := blocker.ExecContext(ctx, `UPDATE recovery_tasks SET attempts=attempts+1,
					payload=JSON_SET(payload,'$.lease_token','replacement-owner'),
					lease_until=CURRENT_TIMESTAMP(6)+INTERVAL 10 SECOND WHERE id=?`, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			for !scenario.replaceOwner {
				var expired bool
				if err := db.QueryRowContext(ctx, `SELECT lease_until<=CURRENT_TIMESTAMP(6) FROM recovery_tasks WHERE id=?`, task.ID).Scan(&expired); err != nil {
					t.Fatal(err)
				}
				if expired {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, errStockRestoreLeaseLost) {
					t.Fatalf("finalizer ignored lease change while waiting: error=%v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var status string
			var attempts uint64
			var reason sql.NullString
			wantAttempts := task.Attempts
			if scenario.replaceOwner {
				wantAttempts++
			}
			if err := db.QueryRow(`SELECT status,attempts,last_error FROM recovery_tasks WHERE id=?`, task.ID).Scan(&status, &attempts, &reason); err != nil || status != "RUNNING" || attempts != wantAttempts || reason.Valid {
				t.Fatalf("stale finalizer mutated task: status=%s attempts=%d reason=%v err=%v", status, attempts, reason, err)
			}
			next := task
			if scenario.replaceOwner {
				next.Attempts++
				next.leaseToken = "replacement-owner"
			} else {
				var found bool
				next, found, err = repository.Claim(ctx, time.Now())
				if err != nil || !found || next.Attempts != task.Attempts+1 || next.leaseToken == task.leaseToken {
					t.Fatalf("reclaim after rejected finalizer: task=%+v found=%v err=%v", next, found, err)
				}
			}
			if err := repository.Succeed(ctx, next); err != nil {
				t.Fatalf("new owner could not complete: %v", err)
			}
		})
	}
}
