package order_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/redis/go-redis/v9"
)

// Stop the normal worker: this test owns the exact crash/retry points between
// receipt, reservation and database admission. Redis keys use a private prefix.
func TestAdmissionCoordinatorMySQLRedisRecovery(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 with MySQL/Redis running and the normal worker stopped")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"concurrent replay", "after reserve crash", "soldout", "disabled", "generation fence", "expired", "redis transient", "db transient", "db commit lost"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := createPurchaseFixture(t, ctx, db, 1, 1)
			var requestID string
			defer func() { cleanupRedeliveryFixture(t, db, fixture, requestID) }()
			prefix := fmt.Sprintf("coordinator-test:%d:%d", fixture.activityID, time.Now().UnixNano())
			store, err := admission.New(client, prefix)
			if err != nil {
				t.Fatal(err)
			}
			// Explicit fixture initialization is an administrative operation, not
			// behavior of Receive, Accept or Resume.
			stock := uint64(1)
			if scenario == "soldout" {
				stock = 0
			}
			var startsAt, endsAt time.Time
			if err := db.QueryRowContext(ctx, `SELECT starts_at, ends_at FROM coupon_activities WHERE id = ?`, fixture.activityID).Scan(&startsAt, &endsAt); err != nil {
				t.Fatal(err)
			}
			if err := store.Initialize(ctx, admission.ActivityState{ActivityID: fixture.activityID, Generation: 1, StockHint: stock, Open: scenario != "disabled", StartsAt: startsAt, EndsAt: endsAt}); err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanupCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				keys, err := client.Keys(cleanupCtx, prefix+":*").Result()
				if err != nil {
					t.Errorf("list isolated Redis fixture keys: %v", err)
					return
				}
				if len(keys) > 0 {
					if err := client.Del(cleanupCtx, keys...).Err(); err != nil {
						t.Errorf("delete isolated Redis fixture: %v", err)
					}
				}
			}()
			service := order.NewAsyncService(db)
			item, err := service.Receive(ctx, fixture.userIDs[0], fixture.activityID, "coordinator-"+scenarioKey(scenario), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			requestID = item.RequestID
			assertAdmissionOutboxCount(t, ctx, db, requestID, 0)
			if item.Status != order.RequestReceived || item.ReservationState != order.ReservationPending {
				t.Fatalf("receipt status=%s", item.Status)
			}
			coord := order.NewAdmissionCoordinator(service, store)
			if scenario == "after reserve crash" || scenario == "generation fence" || scenario == "expired" {
				_, snapshot, err := service.AdmissionSnapshot(ctx, requestID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Admit(ctx, snapshot); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "generation fence" {
				if _, err := db.ExecContext(ctx, `UPDATE coupon_activities SET generation = 2 WHERE id = ?`, fixture.activityID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "expired" {
				if _, err := db.ExecContext(ctx, `UPDATE purchase_requests SET accepted_at = ?, processing_deadline = ? WHERE request_id = ?`, time.Now().UTC().Add(-3*time.Minute), time.Now().UTC().Add(-time.Second), requestID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "redis transient" {
				lost := errors.New("injected Redis transport failure")
				broken := order.NewAdmissionCoordinator(service, transientReservation{ReservationStore: store, err: lost})
				if _, err := broken.Resume(ctx, requestID, time.Now()); !errors.Is(err, lost) {
					t.Fatalf("transient error=%v", err)
				}
				persisted, err := service.GetOwnedRequest(ctx, item.UserID, requestID)
				if err != nil || persisted.Status != order.RequestReceived {
					t.Fatalf("transient lost durable retry point: %+v %v", persisted, err)
				}
				assertAdmissionOutboxCount(t, ctx, db, requestID, 0)
			}
			if scenario == "db transient" || scenario == "db commit lost" {
				lost := errors.New("injected admission DB response failure")
				broken := order.NewAdmissionCoordinator(admissionCommitFault{AdmissionRepository: service, err: lost, commit: scenario == "db commit lost"}, store)
				if _, err := broken.Resume(ctx, requestID, time.Now()); !errors.Is(err, lost) {
					t.Fatalf("DB failure=%v", err)
				}
				persisted, err := service.GetOwnedRequest(ctx, item.UserID, requestID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "db transient" {
					if persisted.Status != order.RequestReceived {
						t.Fatalf("uncommitted admission changed status: %+v", persisted)
					}
					assertAdmissionOutboxCount(t, ctx, db, requestID, 0)
				} else {
					if persisted.Status != order.RequestAdmitted {
						t.Fatalf("lost response lost committed admission: %+v", persisted)
					}
					assertAdmissionOutboxCount(t, ctx, db, requestID, 1)
				}
				value, err := client.Get(ctx, fmt.Sprintf("%s:{%d:1}:stock", prefix, fixture.activityID)).Uint64()
				if err != nil || value != 0 {
					t.Fatalf("ambiguous DB result speculatively released: stock=%d err=%v", value, err)
				}
			}
			if scenario == "concurrent replay" {
				var wg sync.WaitGroup
				for range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if _, err := coord.Accept(ctx, item.UserID, item.ActivityID, item.IdempotencyKey, time.Now()); err != nil {
							t.Errorf("concurrent admission: %v", err)
						}
					}()
				}
				wg.Wait()
			}
			got, err := coord.Resume(ctx, requestID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			failed := scenario == "soldout" || scenario == "disabled" || scenario == "generation fence" || scenario == "expired"
			if failed {
				if got.Status != order.RequestFailed || got.ReservationState != order.ReservationReleased {
					t.Fatalf("failure not persisted: %+v", got)
				}
				if _, err := coord.Resume(ctx, requestID, time.Now()); err != nil {
					t.Fatal(err)
				}
				assertAdmissionOutboxCount(t, ctx, db, requestID, 0)
				assertPurchaseInvariants(t, ctx, db, fixture.activityID, 1, 0, 0)
				value, err := client.Get(ctx, fmt.Sprintf("%s:{%d:1}:stock", prefix, fixture.activityID)).Uint64()
				if err != nil || value != stock {
					t.Fatalf("release duplicated or lost: stock=%d want=%d err=%v", value, stock, err)
				}
			} else {
				if got.Status != order.RequestAdmitted || got.ReservationState != order.ReservationReserved {
					t.Fatalf("admission=%+v", got)
				}
				assertAdmissionOutboxCount(t, ctx, db, requestID, 1)
				staleFailure, err := service.FailAdmission(ctx, requestID, item.Generation, order.FailureSoldOut, time.Now())
				if err != nil || staleFailure.Status != order.RequestAdmitted {
					t.Fatalf("stale reservation failure cancelled admission: %+v %v", staleFailure, err)
				}
				completed, err := service.Process(ctx, requestID, time.Now())
				if err != nil || completed.Status != order.RequestSucceeded {
					t.Fatalf("process=%+v %v", completed, err)
				}
				for range 2 {
					if repaired, err := coord.Resume(ctx, requestID, time.Now()); err != nil || repaired.ReservationState != order.ReservationCommitted {
						t.Fatal(err)
					}
				}
				if _, err := store.Release(ctx, item.ActivityID, item.Generation, item.UserID, requestID); !errors.Is(err, admission.ErrOrderAlreadyCommitted) {
					t.Fatalf("success marker released: %v", err)
				}
				assertPurchaseInvariants(t, ctx, db, fixture.activityID, 0, 1, 1)
			}
		})
	}
}

type admissionCommitFault struct {
	order.AdmissionRepository
	err    error
	commit bool
}

func (f admissionCommitFault) AdmitReserved(ctx context.Context, requestID string, generation uint64, now time.Time) (order.PurchaseRequest, error) {
	if f.commit {
		item, err := f.AdmissionRepository.AdmitReserved(ctx, requestID, generation, now)
		if err != nil {
			return item, err
		}
		return item, f.err
	}
	return order.PurchaseRequest{}, f.err
}

type transientReservation struct {
	order.ReservationStore
	err error
}

func (s transientReservation) Admit(context.Context, admission.Request) (admission.Result, error) {
	return "", s.err
}

func scenarioKey(value string) string { // User keys cannot contain spaces.
	result := ""
	for _, r := range value {
		if r == ' ' {
			result += "-"
		} else {
			result += string(r)
		}
	}
	return result
}

func assertAdmissionOutboxCount(t *testing.T, ctx context.Context, db *sql.DB, requestID string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = ?`, requestID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("outbox count=%d want=%d", count, want)
	}
}
