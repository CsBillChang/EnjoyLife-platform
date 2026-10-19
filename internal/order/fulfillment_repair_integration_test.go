package order_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/order"
)

func TestMySQLVoucherReissueAfterLostResponse(t *testing.T) {
	db := fulfillmentTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := newFulfillmentFixture(t, ctx, db)
	service := order.NewFulfillmentService(db)
	if _, err := service.ReissueOwnedVoucher(ctx, f.customerID, f.order.ID, f.now); !errors.Is(err, order.ErrOrderStateConflict) {
		t.Fatalf("unpaid reissue=%v", err)
	}
	paid, err := service.ApplySuccessfulPayment(ctx, f.payment("lost-response", f.now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReissueOwnedVoucher(ctx, f.merchantID, f.order.ID, f.now); !errors.Is(err, order.ErrOrderNotFound) {
		t.Fatalf("other owner reissue=%v", err)
	}
	first, err := service.ReissueOwnedVoucher(ctx, f.customerID, f.order.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if first.VoucherCode == "" || first.VoucherCode == paid.VoucherCode {
		t.Fatal("voucher code was not rotated")
	}
	if _, err := service.Redeem(ctx, f.merchantID, paid.VoucherCode, f.now); !errors.Is(err, order.ErrVoucherNotFound) {
		t.Fatalf("old payment code still active: %v", err)
	}
	// Losing another response can be repaired again, without creating another
	// voucher row or resurrecting the old code.
	second, err := service.ReissueOwnedVoucher(ctx, f.customerID, f.order.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Redeem(ctx, f.merchantID, first.VoucherCode, f.now); !errors.Is(err, order.ErrVoucherNotFound) {
		t.Fatalf("old reissue code still active: %v", err)
	}
	if _, err := service.Redeem(ctx, f.merchantID, second.VoucherCode, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReissueOwnedVoucher(ctx, f.customerID, f.order.ID, f.now); !errors.Is(err, order.ErrOrderStateConflict) {
		t.Fatalf("redeemed voucher reissued: %v", err)
	}
	assertFulfillmentCount(t, ctx, db, "vouchers", f.order.ID, 1)
	assertFulfillmentCount(t, ctx, db, "redemptions", f.order.ID, 1)
}

func TestMySQLVoucherReissueRejectsRefundingAndExpired(t *testing.T) {
	db := fulfillmentTestDB(t)
	for _, scenario := range []string{"refunding", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f := newFulfillmentFixture(t, ctx, db)
			service := order.NewFulfillmentService(db)
			if _, err := service.ApplySuccessfulPayment(ctx, f.payment(scenario, f.now)); err != nil {
				t.Fatal(err)
			}
			want := order.ErrVoucherUnavailable
			if scenario == "refunding" {
				if _, err := service.RequestNormalRefund(ctx, f.customerID, f.order.ID, "pending-refund", f.now); err != nil {
					t.Fatal(err)
				}
				want = order.ErrOrderStateConflict
			} else if _, err := db.ExecContext(ctx, `UPDATE vouchers SET expires_at = ? WHERE order_id = ?`, time.Now().UTC().Add(-time.Second), f.order.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.ReissueOwnedVoucher(ctx, f.customerID, f.order.ID, f.now); !errors.Is(err, want) {
				t.Fatalf("reissue error=%v want=%v", err, want)
			}
		})
	}
}

func TestMySQLPurchaseAndRedeemRecheckTimeAfterLocks(t *testing.T) {
	db := fulfillmentTestDB(t)
	t.Run("purchase sale ended during activity lock wait", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		fixture := createPurchaseFixture(t, ctx, db, 1, 1)
		defer fixture.cleanup(t, ctx, db)
		end := time.Now().UTC().Add(300 * time.Millisecond).Truncate(time.Microsecond)
		if _, err := db.ExecContext(ctx, `UPDATE coupon_activities SET ends_at = ? WHERE id = ?`, end, fixture.activityID); err != nil {
			t.Fatal(err)
		}
		block := lockFixtureActivity(t, ctx, db, fixture.activityID)
		defer block.Rollback()
		result := make(chan error, 1)
		go func() {
			_, err := order.NewMySQLService(db).Purchase(ctx, fixture.userIDs[0], fixture.activityID, "wait-until-ended", end.Add(-time.Second))
			result <- err
		}()
		waitPastBoundary(t, ctx, result, end)
		if err := block.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, order.ErrActivityEnded) {
			t.Fatalf("stale acceptance time crossed sale end: %v", err)
		}
		assertPurchaseInvariants(t, ctx, db, fixture.activityID, 1, 0, 0)
	})
	t.Run("voucher expires during activity lock wait", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		f := newFulfillmentFixture(t, ctx, db)
		service := order.NewFulfillmentService(db)
		paid, err := service.ApplySuccessfulPayment(ctx, f.payment("expires-waiting", f.now))
		if err != nil {
			t.Fatal(err)
		}
		end := time.Now().UTC().Add(300 * time.Millisecond).Truncate(time.Microsecond)
		if _, err := db.ExecContext(ctx, `UPDATE vouchers SET expires_at = ? WHERE order_id = ?`, end, f.order.ID); err != nil {
			t.Fatal(err)
		}
		block := lockFixtureActivity(t, ctx, db, f.activityID)
		defer block.Rollback()
		result := make(chan error, 1)
		go func() { _, err := service.Redeem(ctx, f.merchantID, paid.VoucherCode, f.now); result <- err }()
		waitPastBoundary(t, ctx, result, end)
		if err := block.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, order.ErrVoucherUnavailable) {
			t.Fatalf("stale redemption time crossed expiry: %v", err)
		}
		assertFulfillmentFacts(t, ctx, db, f, order.Paid, 0)
		assertFulfillmentCount(t, ctx, db, "redemptions", f.order.ID, 0)
	})
}

func lockFixtureActivity(t *testing.T, ctx context.Context, db *sql.DB, activityID uint64) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var id uint64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM coupon_activities WHERE id = ? FOR UPDATE`, activityID).Scan(&id); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	return tx
}

func waitPastBoundary(t *testing.T, ctx context.Context, result <-chan error, end time.Time) {
	t.Helper()
	timer := time.NewTimer(time.Until(end) + 30*time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-result:
		t.Fatalf("operation returned before lock release: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
}

func TestMySQLInventoryReleaseAndRestoreIntentAreAtomic(t *testing.T) {
	db := fulfillmentTestDB(t)
	// The clock-skew case changes only this connection's session timestamp.
	db.SetMaxOpenConns(1)
	for _, scenario := range []string{"close", "close with database clock behind", "refund rollback then success", "legacy none"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			f := createPurchaseFixture(t, ctx, db, 1, 1)
			async := order.NewAsyncService(db)
			fulfill := order.NewFulfillmentService(db)
			var requestID string
			var orderID uint64
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if orderID != 0 {
					if _, err := db.ExecContext(cleanup, `DELETE FROM recovery_tasks WHERE kind = ? AND source_id = ?`, order.StockRestoreTaskKind, orderID); err != nil {
						t.Error(err)
					}
					for _, table := range []string{"refunds", "vouchers", "payments"} {
						if _, err := db.ExecContext(cleanup, `DELETE FROM `+table+` WHERE order_id = ?`, orderID); err != nil {
							t.Error(err)
						}
					}
				}
				cleanupAsyncRequest(t, cleanup, db, requestID)
				f.cleanup(t, cleanup, db)
			}()
			var item order.PurchaseRequest
			var err error
			if scenario == "legacy none" {
				item, err = async.Accept(ctx, f.userIDs[0], f.activityID, "legacy-restore", time.Now())
			} else {
				item, err = async.Receive(ctx, f.userIDs[0], f.activityID, "restore", time.Now())
				if err == nil {
					item, err = async.AdmitReserved(ctx, item.RequestID, item.Generation, time.Now())
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			requestID = item.RequestID
			item, err = async.Process(ctx, requestID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			orderID = item.OrderID
			if _, err := db.ExecContext(ctx, `UPDATE coupon_activities SET generation = 2 WHERE id = ?`, f.activityID); err != nil {
				t.Fatal(err)
			}
			if scenario == "close with database clock behind" {
				if _, err := db.ExecContext(ctx, `SET timestamp = UNIX_TIMESTAMP()-3600`); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := db.Exec(`SET timestamp = 0`); err != nil {
						t.Error(err)
					}
				}()
			}
			if scenario == "refund rollback then success" {
				input := order.SuccessfulPayment{UserID: item.UserID, OrderID: orderID, AttemptID: requestID, Provider: "LOCAL", TransactionID: "payment-" + requestID, AmountCents: item.PriceCents, Currency: item.Currency, Now: time.Now()}
				if _, err := fulfill.ApplySuccessfulPayment(ctx, input); err != nil {
					t.Fatal(err)
				}
				refund, err := fulfill.RequestNormalRefund(ctx, item.UserID, orderID, "refund", time.Now())
				if err != nil {
					t.Fatal(err)
				}
				pending, err := fulfill.ListPendingRefunds(ctx, refund.ID-1, 1)
				if err != nil || len(pending) != 1 || pending[0].RequestID != refund.RequestID || pending[0].ProviderTransactionID != input.TransactionID {
					t.Fatalf("refund scan=%+v %v", pending, err)
				}
				// Failure after RELEASE and task insertion must roll both back.
				if _, err := db.ExecContext(ctx, `UPDATE vouchers SET status = 'INVALID' WHERE order_id = ?`, orderID); err != nil {
					t.Fatal(err)
				}
				if _, err := fulfill.ApplyRefundResult(ctx, refund.RequestID, order.RefundSucceeded, time.Now()); !errors.Is(err, order.ErrVoucherUnavailable) {
					t.Fatalf("injected late-transaction failure=%v", err)
				}
				assertRestoreCount(t, ctx, db, orderID, 0)
				assertPurchaseInvariants(t, ctx, db, f.activityID, 0, 1, 1)
				if _, err := db.ExecContext(ctx, `UPDATE vouchers SET status = 'ACTIVE' WHERE order_id = ?`, orderID); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if _, err := fulfill.ApplyRefundResult(ctx, refund.RequestID, order.RefundSucceeded, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				ids, err := fulfill.ListExpiredOrderIDs(ctx, orderID-1, 1, time.Now().Add(time.Hour))
				if err != nil || len(ids) != 1 || ids[0] != orderID {
					t.Fatalf("expiry scan=%v %v", ids, err)
				}
				for range 2 {
					if _, err := fulfill.CloseExpired(ctx, orderID, time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
			}
			want := 1
			if scenario == "legacy none" {
				want = 0
			}
			assertRestoreCount(t, ctx, db, orderID, want)
			if want == 1 {
				var generation uint64
				var raw []byte
				if err := db.QueryRowContext(ctx, `SELECT generation,payload FROM recovery_tasks WHERE kind = ? AND source_id = ?`, order.StockRestoreTaskKind, orderID).Scan(&generation, &raw); err != nil {
					t.Fatal(err)
				}
				var payload order.StockRestorePayload
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				if generation != 2 || payload.Generation != 2 || payload.RequestGeneration != 1 || payload.ActivityID != f.activityID || payload.OrderID != orderID {
					t.Fatalf("restore epoch must be RELEASE epoch: %+v row=%d", payload, generation)
				}
				if scenario == "close with database clock behind" {
					var delayMicros int64
					if err := db.QueryRowContext(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND,CURRENT_TIMESTAMP(6),available_at) FROM recovery_tasks WHERE kind=? AND source_id=?`, order.StockRestoreTaskKind, orderID).Scan(&delayMicros); err != nil || delayMicros != 0 {
						t.Fatalf("initial task is not ready on database clock: delay=%d us err=%v", delayMicros, err)
					}
				}
			}
			var releases int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE order_id = ? AND kind = 'RELEASE'`, orderID).Scan(&releases); err != nil || releases != 1 {
				t.Fatalf("release count=%d %v", releases, err)
			}
			assertPurchaseInvariants(t, ctx, db, f.activityID, 1, 1, 1)
		})
	}
}

func assertRestoreCount(t *testing.T, ctx context.Context, db *sql.DB, orderID uint64, want int) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_tasks WHERE kind = ? AND source_id = ?`, order.StockRestoreTaskKind, orderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("stock restore task count=%d want=%d", count, want)
	}
}
