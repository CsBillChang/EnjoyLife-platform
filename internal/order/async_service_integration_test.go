package order_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	_ "github.com/go-sql-driver/mysql"
)

func TestAsyncPurchaseMySQLIdempotencyAndTerminalFailures(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run against the project MySQL")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}

	t.Run("duplicate messages create one order", func(t *testing.T) {
		fixture := createPurchaseFixture(t, ctx, db, 1, 1)
		var requestID string
		defer func() {
			cleanupAsyncRequest(t, ctx, db, requestID)
			fixture.cleanup(t, ctx, db)
		}()
		service := order.NewAsyncService(db)
		accepted, err := service.Accept(ctx, fixture.userIDs[0], fixture.activityID, "async-duplicate", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		requestID = accepted.RequestID
		replay, err := service.Accept(ctx, fixture.userIDs[0], fixture.activityID, "async-duplicate", time.Now())
		if err != nil || replay.RequestID != requestID {
			t.Fatalf("accept replay=%+v err=%v", replay, err)
		}

		const consumers = 8
		var wg sync.WaitGroup
		results := make(chan order.PurchaseRequest, consumers)
		errors := make(chan error, consumers)
		for range consumers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				item, processErr := service.Process(ctx, requestID, time.Now())
				results <- item
				errors <- processErr
			}()
		}
		wg.Wait()
		close(results)
		close(errors)
		for processErr := range errors {
			if processErr != nil {
				t.Fatalf("duplicate processing error: %v", processErr)
			}
		}
		var orderID uint64
		for item := range results {
			if item.Status != order.RequestSucceeded || item.OrderID == 0 {
				t.Fatalf("unexpected terminal result: %+v", item)
			}
			if orderID == 0 {
				orderID = item.OrderID
			} else if item.OrderID != orderID {
				t.Fatalf("duplicate messages returned different orders: %d and %d", orderID, item.OrderID)
			}
		}
		assertPurchaseInvariants(t, ctx, db, fixture.activityID, 0, 1, 1)
		var outboxCount int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = ?`, requestID).Scan(&outboxCount); err != nil {
			t.Fatal(err)
		}
		if outboxCount != 1 {
			t.Fatalf("outbox events=%d, want 1", outboxCount)
		}
	})

	for _, scenario := range []struct {
		name     string
		key      string
		mutate   func(context.Context, *sql.DB, order.PurchaseRequest) error
		wantCode string
	}{
		{
			name: "processing deadline",
			key:  "async-processing-deadline",
			mutate: func(ctx context.Context, db *sql.DB, item order.PurchaseRequest) error {
				now := time.Now().UTC()
				_, err := db.ExecContext(ctx, `UPDATE purchase_requests SET accepted_at = ?, processing_deadline = ? WHERE request_id = ?`, now.Add(-2*time.Minute), now.Add(-time.Minute), item.RequestID)
				return err
			},
			wantCode: order.FailureProcessingExpired,
		},
		{
			name: "generation fence",
			key:  "async-generation-fence",
			mutate: func(ctx context.Context, db *sql.DB, item order.PurchaseRequest) error {
				_, err := db.ExecContext(ctx, `UPDATE coupon_activities SET generation = generation + 1 WHERE id = ?`, item.ActivityID)
				return err
			},
			wantCode: order.FailureGenerationChanged,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := createPurchaseFixture(t, ctx, db, 1, 1)
			var requestID string
			defer func() {
				cleanupAsyncRequest(t, ctx, db, requestID)
				fixture.cleanup(t, ctx, db)
			}()
			service := order.NewAsyncService(db)
			accepted, err := service.Accept(ctx, fixture.userIDs[0], fixture.activityID, scenario.key, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			requestID = accepted.RequestID
			if err := scenario.mutate(ctx, db, accepted); err != nil {
				t.Fatal(err)
			}
			result, err := service.Process(ctx, requestID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != order.RequestFailed || result.ErrorCode != scenario.wantCode {
				t.Fatalf("result=%+v, want FAILED/%s", result, scenario.wantCode)
			}
			assertPurchaseInvariants(t, ctx, db, fixture.activityID, 1, 0, 0)
		})
	}
}

func cleanupAsyncRequest(t *testing.T, ctx context.Context, db *sql.DB, requestID string) {
	t.Helper()
	if requestID == "" {
		return
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM outbox_events WHERE aggregate_id = ?`, requestID)
	_, _ = db.ExecContext(ctx, `DELETE FROM purchase_requests WHERE request_id = ?`, requestID)
}
