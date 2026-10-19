package coupon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	_ "github.com/go-sql-driver/mysql"
)

func isolatedCouponRecoveryDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 for coupon invalidation integration")
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
		t.Fatal(err)
	}
	const create = "CREATE TABLE `recovery_tasks`"
	if table != "recovery_tasks" || !strings.HasPrefix(definition, create) {
		t.Fatal("unexpected recovery_tasks schema")
	}
	definition = strings.Replace(definition, create, "CREATE TEMPORARY TABLE `recovery_tasks`", 1)
	if _, err := db.ExecContext(ctx, definition); err != nil {
		t.Fatal(err)
	}
	return db
}

type sequencedInvalidator struct {
	errs  []error
	calls int
}

func (f *sequencedInvalidator) Invalidate(context.Context, uint64, uint64, uint64) error {
	f.calls++
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func insertCouponInvalidationTask(t *testing.T, db *sql.DB, source string, generation uint64, status string, attempts uint32, available, lease time.Time, payload []byte) {
	t.Helper()
	var leaseValue any
	if !lease.IsZero() {
		leaseValue = lease.UTC().Truncate(time.Microsecond)
	}
	if _, err := db.Exec(`INSERT INTO recovery_tasks (kind,source_id,generation,status,attempts,available_at,lease_until,payload,last_error) VALUES (?,?,?,?,?,?,?,?,NULL)`,
		couponCacheInvalidationKind, source, generation, status, attempts, available.UTC().Truncate(time.Microsecond), leaseValue, payload); err != nil {
		t.Fatal(err)
	}
}

func TestCouponInvalidationRetriesThenSucceedsIntegration(t *testing.T) {
	db := isolatedCouponRecoveryDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	payload, _ := (couponInvalidationPayload{ActivityID: 11, ShopID: 22, Generation: 1}).marshal()
	insertCouponInvalidationTask(t, db, "11", 1, "PENDING", 0, now, time.Time{}, payload)
	inv := &sequencedInvalidator{errs: []error{errors.New("redis unavailable"), nil}}
	dispatcher, err := NewInvalidationDispatcher(db, inv, InvalidationConfig{LeaseDuration: time.Second, BackoffBase: time.Millisecond, BackoffMax: time.Millisecond, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := dispatcher.DispatchOne(context.Background(), now); err != nil || !worked {
		t.Fatalf("first dispatch worked=%v err=%v", worked, err)
	}
	var status string
	var attempts uint32
	if err := db.QueryRow(`SELECT status,attempts FROM recovery_tasks WHERE source_id='11'`).Scan(&status, &attempts); err != nil || status != "PENDING" || attempts != 1 {
		t.Fatalf("after failure status=%s attempts=%d err=%v", status, attempts, err)
	}
	if worked, err := dispatcher.DispatchOne(context.Background(), time.Now().UTC().Add(10*time.Millisecond)); err != nil || !worked {
		t.Fatalf("retry worked=%v err=%v", worked, err)
	}
	if err := db.QueryRow(`SELECT status,attempts FROM recovery_tasks WHERE source_id='11'`).Scan(&status, &attempts); err != nil || status != "SUCCEEDED" || attempts != 2 || inv.calls != 2 {
		t.Fatalf("after retry status=%s attempts=%d calls=%d err=%v", status, attempts, inv.calls, err)
	}
}

func TestCouponInvalidationPoisonAndLeaseFencesIntegration(t *testing.T) {
	db := isolatedCouponRecoveryDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	bad, _ := (couponInvalidationPayload{ActivityID: 31, ShopID: 41, Generation: 99}).marshal()
	good, _ := (couponInvalidationPayload{ActivityID: 32, ShopID: 42, Generation: 2}).marshal()
	insertCouponInvalidationTask(t, db, "31", 1, "PENDING", 0, now, time.Time{}, bad)
	insertCouponInvalidationTask(t, db, "32", 2, "PENDING", 0, now, time.Time{}, good)
	inv := &sequencedInvalidator{}
	dispatcher, _ := NewInvalidationDispatcher(db, inv, InvalidationConfig{LeaseDuration: 30 * time.Millisecond, MaxAttempts: 2})
	if worked, err := dispatcher.DispatchOne(context.Background(), now); err == nil || worked {
		t.Fatalf("poison task worked=%v err=%v", worked, err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM recovery_tasks WHERE source_id='31'`).Scan(&status); err != nil || status != "FAILED" {
		t.Fatalf("poison status=%s err=%v", status, err)
	}
	if worked, err := dispatcher.DispatchOne(context.Background(), now); err != nil || !worked || inv.calls != 1 {
		t.Fatalf("healthy task starved worked=%v calls=%d err=%v", worked, inv.calls, err)
	}

	leaseUntil := time.Now().UTC().Add(time.Second)
	payload, _ := (couponInvalidationPayload{ActivityID: 33, ShopID: 43, Generation: 3}).marshal()
	insertCouponInvalidationTask(t, db, "33", 3, "RUNNING", 2, now, leaseUntil, payload)
	if worked, err := dispatcher.DispatchOne(context.Background(), now); err != nil || worked {
		t.Fatalf("active final lease was claimed worked=%v err=%v", worked, err)
	}
	if err := db.QueryRow(`SELECT status FROM recovery_tasks WHERE source_id='33'`).Scan(&status); err != nil || status != "RUNNING" {
		t.Fatalf("active lease status=%s err=%v", status, err)
	}
	if worked, err := dispatcher.DispatchOne(context.Background(), leaseUntil.Add(time.Microsecond)); err != nil || worked {
		t.Fatalf("expired exhausted task worked=%v err=%v", worked, err)
	}
	if err := db.QueryRow(`SELECT status FROM recovery_tasks WHERE source_id='33'`).Scan(&status); err != nil || status != "FAILED" {
		t.Fatalf("expired exhausted status=%s err=%v", status, err)
	}

	payload, _ = (couponInvalidationPayload{ActivityID: 34, ShopID: 44, Generation: 4}).marshal()
	insertCouponInvalidationTask(t, db, "34", 4, "PENDING", 0, now, time.Time{}, payload)
	task, err := dispatcher.claim(context.Background(), now)
	if err != nil || task == nil {
		t.Fatalf("claim stale lease task=%+v err=%v", task, err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := dispatcher.succeed(context.Background(), *task, now); !errors.Is(err, ErrInvalidationLeaseLost) {
		t.Fatalf("stale lease completion=%v", err)
	}
	if err := db.QueryRow(`SELECT status FROM recovery_tasks WHERE source_id='34'`).Scan(&status); err != nil || status != "RUNNING" {
		t.Fatalf("stale lease overwrote status=%s err=%v", status, err)
	}
}
