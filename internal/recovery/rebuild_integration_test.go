package recovery_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/recovery"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

func TestMySQLRedisRebuildCrashResumeAndHistoricalEligibility(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 and stop the normal worker for controlled recovery experiments")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	for _, failAt := range []recovery.Phase{recovery.PhaseFrozen, recovery.PhaseNormalized, recovery.PhaseInstalled, recovery.PhaseVerified, recovery.PhaseRedisOpen, recovery.PhaseDone} {
		t.Run(string(failAt), func(t *testing.T) {
			fixture := newRebuildFixture(t, ctx, db, client)
			store := fixture.store
			injected := errors.New("injected rebuild interruption")
			service := recovery.NewService(db, store, func(phase recovery.Phase) error {
				if phase == failAt {
					return injected
				}
				return nil
			})
			task, err := service.Rebuild(ctx, fixture.activity)
			if failAt != recovery.PhaseDone {
				if !errors.Is(err, injected) {
					t.Fatalf("task=%+v interruption=%v", task, err)
				}
				var state string
				var generation uint64
				if err := db.QueryRowContext(ctx, `SELECT admission_state,generation FROM coupon_activities WHERE id = ?`, fixture.activity).Scan(&state, &generation); err != nil {
					t.Fatal(err)
				}
				if state == "OPEN" || generation != 2 {
					t.Fatalf("interrupted rebuild reopened traffic: state=%s generation=%d", state, generation)
				}
				if _, err := order.NewAsyncService(db).Receive(ctx, fixture.users[5], fixture.activity, "must-stay-closed", time.Now()); !errors.Is(err, order.ErrActivityUnavailable) {
					t.Fatalf("receipt crossed freeze: %v", err)
				}
				if _, err := order.NewFulfillmentService(db).CloseExpired(ctx, fixture.orders[0].ID, time.Now().Add(time.Hour)); !errors.Is(err, order.ErrActivityUnavailable) {
					t.Fatalf("inventory release crossed freeze: %v", err)
				}
				replayed, replayErr := recovery.NewService(db, store, nil).Start(ctx, fixture.activity)
				if replayErr != nil || replayed.ID != task.ID || replayed.Generation != 2 {
					t.Fatalf("resume advanced generation: %+v %v", replayed, replayErr)
				}
				task, err = recovery.NewService(db, store, nil).Resume(ctx, task.ID)
			}
			if err != nil || task.Status != "SUCCEEDED" || task.Generation != 2 || task.Phase != recovery.PhaseDone || task.Stock != 2 || task.Owners != 3 {
				t.Fatalf("completion=%+v err=%v", task, err)
			}
			assertRebuildFacts(t, ctx, db, fixture)
			stockKey := fmt.Sprintf("%s:{%d:2}:stock", fixture.prefix, fixture.activity)
			if stock, err := client.Get(ctx, stockKey).Int(); err != nil || stock != 2 {
				t.Fatalf("rebuilt stock=%d err=%v", stock, err)
			}
			// PENDING_PAYMENT, CLOSED (already RELEASEd), and the healed async order
			// all retain permanent user markers, independent of order status.
			for _, index := range []int{0, 1, 4} {
				r := fixture.reservation(index, 2, fmt.Sprintf("another-purchase-%d", index))
				if _, err := store.Admit(ctx, r); !errors.Is(err, admission.ErrUserReserved) {
					t.Fatalf("historical user %d regained eligibility: %v", index, err)
				}
			}
			for _, req := range fixture.requests[:2] {
				item, err := order.NewAsyncService(db).GetOwnedRequest(ctx, req.UserID, req.RequestID)
				if err != nil || item.Status != order.RequestFailed || item.ReservationState != order.ReservationReleased {
					t.Fatalf("old live request not retired: %+v %v", item, err)
				}
				// A worker that read the old marker before the freeze may arrive late.
				// Its generation is immutable: the new stock must not change.
				if _, err := store.Release(ctx, req.ActivityID, req.Generation, req.UserID, req.RequestID); err != nil {
					t.Fatal(err)
				}
			}
			// A delayed inventory-release projection is likewise confined to its
			// original generation, even when delivered more than once.
			for _, want := range []admission.Result{admission.Released, admission.Replay} {
				result, err := store.RestoreCommittedStock(ctx, fixture.activity, 1, fixture.orders[1].ID)
				if err != nil || result != want {
					t.Fatalf("late old inventory restore=%s err=%v", result, err)
				}
			}
			if stock, err := client.Get(ctx, stockKey).Int(); err != nil || stock != 2 {
				t.Fatalf("late old release changed new generation: %d %v", stock, err)
			}
			healed, err := order.NewAsyncService(db).GetOwnedRequest(ctx, fixture.requests[2].UserID, fixture.requests[2].RequestID)
			if err != nil || healed.Status != order.RequestSucceeded || healed.ReservationState != order.ReservationCommitted || healed.OrderID != fixture.orders[2].ID {
				t.Fatalf("order evidence lost to old request status: %+v %v", healed, err)
			}
			// Normal traffic resumes only after MySQL is OPEN. Retrying the finished
			// task must not reset this fresh reservation's stock decrement.
			coordinator := order.NewAdmissionCoordinator(order.NewAsyncService(db), store)
			fresh, err := coordinator.Accept(ctx, fixture.users[2], fixture.activity, "new-generation", time.Now())
			if err != nil || fresh.Status != order.RequestAdmitted || fresh.Generation != 2 {
				t.Fatalf("reopened admission=%+v %v", fresh, err)
			}
			replayed, err := recovery.NewService(db, store, nil).Resume(ctx, task.ID)
			if err != nil || replayed.Generation != 2 {
				t.Fatalf("finished task replay=%+v %v", replayed, err)
			}
			snapshot := admission.RebuildSnapshot{ActivityID: fixture.activity, Generation: 2, StockHint: 2}
			if err := db.QueryRowContext(ctx, `SELECT starts_at, ends_at FROM coupon_activities WHERE id = ?`, fixture.activity).Scan(&snapshot.StartsAt, &snapshot.EndsAt); err != nil {
				t.Fatal(err)
			}
			for _, existing := range fixture.orders {
				snapshot.Owners = append(snapshot.Owners, admission.CommittedOwner{UserID: existing.UserID, RequestID: existing.RequestID})
			}
			if err := store.InstallRebuild(ctx, snapshot, fmt.Sprintf("rebuild:%d:2", task.ID)); err != nil {
				t.Fatal(err)
			}
			if stock, err := client.Get(ctx, stockKey).Int(); err != nil || stock != 1 {
				t.Fatalf("late installer erased fresh reservation: %d %v", stock, err)
			}
			t.Logf("phase=%s task=%d generation=%d owners=%d stock=%d: resumed, old releases isolated", failAt, task.ID, task.Generation, task.Owners, task.Stock)
		})
	}
}

type rebuildFixture struct {
	activity, shop uint64
	users          []uint64
	orders         []order.Order
	requests       []order.PurchaseRequest
	prefix         string
	store          *admission.Store
}

func TestMySQLRedisRebuildRejectsInventoryLedgerMismatch(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 and stop the normal worker for controlled recovery experiments")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f := newRebuildFixture(t, ctx, db, client)
	// The activity remains inside 0..total_stock, so a bounds-only check cannot
	// detect this corruption. Its movement ledger still proves available=2.
	if _, err := db.ExecContext(ctx, `UPDATE coupon_activities SET available_stock = 3 WHERE id = ?`, f.activity); err != nil {
		t.Fatal(err)
	}
	service := recovery.NewService(db, f.store, nil)
	task, err := service.Rebuild(ctx, f.activity)
	if !errors.Is(err, recovery.ErrInventoryMismatch) {
		t.Fatalf("mismatched ledger accepted: task=%+v err=%v", task, err)
	}
	if _, err := service.Resume(ctx, task.ID); !errors.Is(err, recovery.ErrInventoryMismatch) {
		t.Fatalf("retry bypassed ledger check: %v", err)
	}
	var state string
	if err := db.QueryRowContext(ctx, `SELECT admission_state FROM coupon_activities WHERE id = ?`, f.activity).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == "OPEN" {
		t.Fatal("ledger mismatch reopened MySQL")
	}
	keys, err := client.Keys(ctx, fmt.Sprintf("%s:{%d:2}:*", f.prefix, f.activity)).Result()
	if err != nil || len(keys) != 0 {
		t.Fatalf("corrupt snapshot published to Redis: keys=%v err=%v", keys, err)
	}
	// Repair only the fixture corruption, then resume the durable task and
	// verify that the original generation was not advanced a second time.
	if _, err := db.ExecContext(ctx, `UPDATE coupon_activities SET available_stock = 2 WHERE id = ?`, f.activity); err != nil {
		t.Fatal(err)
	}
	finished, err := service.Resume(ctx, task.ID)
	if err != nil || finished.Generation != 2 || finished.Status != "SUCCEEDED" {
		t.Fatalf("repair could not resume task: %+v %v", finished, err)
	}
	assertRebuildFacts(t, ctx, db, f)
}

func newRebuildFixture(t *testing.T, ctx context.Context, db *sql.DB, client *redis.Client) *rebuildFixture {
	t.Helper()
	f := &rebuildFixture{prefix: fmt.Sprintf("rebuild-test:%d", time.Now().UnixNano())}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if f.activity != 0 {
			for _, query := range []string{
				`DELETE FROM outbox_events WHERE aggregate_id IN (SELECT request_id FROM purchase_requests WHERE activity_id = ?)`,
				`DELETE FROM purchase_requests WHERE activity_id = ?`,
				`DELETE FROM inventory_movements WHERE activity_id = ?`,
				`DELETE FROM orders WHERE activity_id = ?`,
				`DELETE FROM coupon_activities WHERE id = ?`,
			} {
				if _, err := db.ExecContext(cleanup, query, f.activity); err != nil {
					t.Errorf("clean rebuild fixture: %v", err)
				}
			}
			if _, err := db.ExecContext(cleanup, `DELETE FROM recovery_tasks WHERE kind = ? AND source_id = ?`, recovery.TaskKind, fmt.Sprintf("%d:2", f.activity)); err != nil {
				t.Error(err)
			}
		}
		for _, user := range f.users {
			if _, err := db.ExecContext(cleanup, `DELETE FROM users WHERE id = ?`, user); err != nil {
				t.Error(err)
			}
		}
		if f.shop != 0 {
			if _, err := db.ExecContext(cleanup, `DELETE FROM shops WHERE id = ?`, f.shop); err != nil {
				t.Error(err)
			}
		}
		keys, err := client.Keys(cleanup, f.prefix+":*").Result()
		if err != nil {
			t.Error(err)
		} else if len(keys) > 0 {
			if err := client.Del(cleanup, keys...).Err(); err != nil {
				t.Error(err)
			}
		}
	})
	insert, err := db.ExecContext(ctx, `INSERT INTO shops (name,address,status) VALUES (?, 'rebuild-test', 'OPEN')`, f.prefix)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := insert.LastInsertId()
	f.shop = uint64(id)
	now := time.Now().UTC()
	insert, err = db.ExecContext(ctx, `INSERT INTO coupon_activities (shop_id,title,price_cents,currency,starts_at,ends_at,voucher_valid_until,total_stock,available_stock,admission_state,generation) VALUES (?, 'rebuild-test', 100, 'CNY', ?, ?, ?, 4, 4, 'OPEN', 1)`, f.shop, now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	id, _ = insert.LastInsertId()
	f.activity = uint64(id)
	for index := range 6 {
		insert, err := db.ExecContext(ctx, `INSERT INTO users (username,password_hash,role) VALUES (?, 'test-only', 'CUSTOMER')`, fmt.Sprintf("rebuild-%d-%d", f.activity, index))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := insert.LastInsertId()
		f.users = append(f.users, uint64(id))
	}
	synchronous := order.NewMySQLService(db)
	for index := range 2 {
		item, err := synchronous.Purchase(ctx, f.users[index], f.activity, fmt.Sprintf("history-%d", index), now)
		if err != nil {
			t.Fatal(err)
		}
		f.orders = append(f.orders, item)
	}
	if _, err := order.NewFulfillmentService(db).CloseExpired(ctx, f.orders[1].ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.store, err = admission.New(client, f.prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Initialize(ctx, admission.ActivityState{ActivityID: f.activity, Generation: 1, StockHint: 3, Open: true, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	async := order.NewAsyncService(db)
	coordinator := order.NewAdmissionCoordinator(async, f.store)
	received, err := async.Receive(ctx, f.users[2], f.activity, "old-received", now)
	if err != nil {
		t.Fatal(err)
	}
	f.requests = append(f.requests, received)
	_, reservation, err := async.AdmissionSnapshot(ctx, received.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Admit(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{3, 4} {
		item, err := coordinator.Accept(ctx, f.users[index], f.activity, fmt.Sprintf("old-admitted-%d", index), now)
		if err != nil {
			t.Fatal(err)
		}
		f.requests = append(f.requests, item)
	}
	completed, err := async.Process(ctx, f.requests[2].RequestID, now)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := synchronous.GetOwnedOrder(ctx, completed.UserID, completed.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	f.orders = append(f.orders, existing)
	// Deliberately corrupt only the request projection to prove recovery gives
	// precedence to an existing authoritative order, not its stale request state.
	if _, err := db.ExecContext(ctx, `UPDATE purchase_requests SET status = 'RECEIVED', order_id = NULL WHERE request_id = ?`, completed.RequestID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *rebuildFixture) reservation(index int, generation uint64, requestID string) admission.Request {
	now := time.Now().UTC()
	return admission.Request{ActivityID: f.activity, Generation: generation, UserID: f.users[index], RequestID: requestID, AcceptedAt: now, ProcessingDeadline: now.Add(time.Minute), StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}
}

func assertRebuildFacts(t *testing.T, ctx context.Context, db *sql.DB, f *rebuildFixture) {
	t.Helper()
	var stock, total, orders, deducts, releases int
	var state string
	if err := db.QueryRowContext(ctx, `SELECT available_stock,total_stock,admission_state FROM coupon_activities WHERE id = ?`, f.activity).Scan(&stock, &total, &state); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE activity_id = ?`, f.activity).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE activity_id = ? AND kind = 'DEDUCT'`, f.activity).Scan(&deducts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE activity_id = ? AND kind = 'RELEASE'`, f.activity).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if stock != 2 || stock > total || orders != 3 || deducts != 3 || releases != 1 || state != "OPEN" {
		t.Fatalf("stock=%d/%d orders=%d deducts=%d releases=%d state=%s", stock, total, orders, deducts, releases, state)
	}
}
