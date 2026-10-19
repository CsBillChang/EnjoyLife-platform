package order_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	_ "github.com/go-sql-driver/mysql"
)

func TestMySQLFulfillmentFlowAndPaymentReplay(t *testing.T) {
	db := fulfillmentTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := newFulfillmentFixture(t, ctx, db)
	service := order.NewFulfillmentService(db)
	input := fixture.payment("normal", fixture.now)
	paid, err := service.ApplySuccessfulPayment(ctx, input)
	if err != nil || paid.Order.Status != order.Paid || paid.Outcome != order.PaymentApplied || paid.VoucherCode == "" {
		t.Fatalf("payment result status=%s outcome=%s has_code=%v err=%v", paid.Order.Status, paid.Outcome, paid.VoucherCode != "", err)
	}
	var storedHash []byte
	if err := db.QueryRowContext(ctx, `SELECT code_hash FROM vouchers WHERE order_id = ?`, fixture.order.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(paid.VoucherCode))
	if hex.EncodeToString(storedHash) != hex.EncodeToString(wantHash[:]) {
		t.Fatal("voucher stored value is not the SHA-256 digest of the returned code")
	}
	replay, err := service.ApplySuccessfulPayment(ctx, input)
	if err != nil || replay.PaymentID != paid.PaymentID || replay.Outcome != order.PaymentDuplicate || replay.VoucherCode != "" {
		t.Fatalf("payment replay id=%d outcome=%s has_code=%v err=%v", replay.PaymentID, replay.Outcome, replay.VoucherCode != "", err)
	}
	assertFulfillmentCount(t, ctx, db, "vouchers", fixture.order.ID, 1)
	assertFulfillmentCount(t, ctx, db, "payments", fixture.order.ID, 1)
	if _, err := service.Redeem(ctx, fixture.customerID, paid.VoucherCode, fixture.now); !errors.Is(err, order.ErrMerchantForbidden) {
		t.Fatalf("customer redemption error=%v, want merchant forbidden", err)
	}
	redeemed, err := service.Redeem(ctx, fixture.merchantID, paid.VoucherCode, fixture.now)
	if err != nil || redeemed.Status != order.Redeemed {
		t.Fatalf("merchant redemption status=%s err=%v", redeemed.Status, err)
	}
	if _, err := service.Redeem(ctx, fixture.merchantID, paid.VoucherCode, fixture.now); !errors.Is(err, order.ErrOrderStateConflict) {
		t.Fatalf("duplicate redemption error=%v", err)
	}
	assertFulfillmentCount(t, ctx, db, "redemptions", fixture.order.ID, 1)
	assertFulfillmentFacts(t, ctx, db, fixture, order.Redeemed, 0)
}

func TestMySQLPaymentCloseCompetition(t *testing.T) {
	db := fulfillmentTestDB(t)
	paidWins, closeWins := 0, 0
	const rounds = 6
	for round := range rounds {
		t.Run(fmt.Sprintf("round_%d", round+1), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fixture := newFulfillmentFixture(t, ctx, db)
			service := order.NewFulfillmentService(db)
			now := fixture.order.PaymentDeadline.Add(time.Second)
			input := fixture.payment("race", now)
			start := make(chan struct{})
			type payResult struct {
				value order.PaymentResult
				err   error
			}
			type closeResult struct {
				value order.Order
				err   error
			}
			payments := make(chan payResult, 1)
			closures := make(chan closeResult, 1)
			go func() {
				<-start
				value, err := service.ApplySuccessfulPayment(ctx, input)
				payments <- payResult{value, err}
			}()
			go func() {
				<-start
				value, err := service.CloseExpired(ctx, fixture.order.ID, now)
				closures <- closeResult{value, err}
			}()
			close(start)
			payment, closure := <-payments, <-closures
			if payment.err != nil {
				t.Fatalf("payment race error=%v", payment.err)
			}
			switch payment.value.Outcome {
			case order.PaymentApplied:
				paidWins++
				if !errors.Is(closure.err, order.ErrOrderStateConflict) {
					t.Fatalf("payment won but close error=%v", closure.err)
				}
				assertFulfillmentCount(t, ctx, db, "vouchers", fixture.order.ID, 1)
				assertFulfillmentCount(t, ctx, db, "refunds", fixture.order.ID, 0)
				assertFulfillmentFacts(t, ctx, db, fixture, order.Paid, 0)
			case order.PaymentLate:
				closeWins++
				if closure.err != nil || closure.value.Status != order.Closed || payment.value.Refund == nil ||
					payment.value.Refund.Purpose != order.LatePaymentRefund || payment.value.Refund.Status != order.RefundPending {
					t.Fatalf("close winner missing compensation: closure_status=%s closure_err=%v refund=%+v", closure.value.Status, closure.err, payment.value.Refund)
				}
				assertFulfillmentCount(t, ctx, db, "vouchers", fixture.order.ID, 0)
				assertFulfillmentCount(t, ctx, db, "refunds", fixture.order.ID, 1)
				assertFulfillmentFacts(t, ctx, db, fixture, order.Closed, 1)
			default:
				t.Fatalf("unexpected payment race outcome=%s", payment.value.Outcome)
			}
			assertFulfillmentCount(t, ctx, db, "payments", fixture.order.ID, 1)
		})
	}
	t.Logf("payment-close observed winners: rounds=%d payment=%d close=%d", rounds, paidWins, closeWins)
}

func TestMySQLRefundRedemptionCompetition(t *testing.T) {
	db := fulfillmentTestDB(t)
	refundWins, redemptionWins := 0, 0
	const rounds = 6
	for round := range rounds {
		t.Run(fmt.Sprintf("round_%d", round+1), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fixture := newFulfillmentFixture(t, ctx, db)
			service := order.NewFulfillmentService(db)
			paid, err := service.ApplySuccessfulPayment(ctx, fixture.payment("refund-race", fixture.now))
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			type refundResult struct {
				value order.Refund
				err   error
			}
			type redeemResult struct {
				value order.Order
				err   error
			}
			refunds := make(chan refundResult, 1)
			redemptions := make(chan redeemResult, 1)
			go func() {
				<-start
				value, err := service.RequestNormalRefund(ctx, fixture.customerID, fixture.order.ID, "refund-race", fixture.now)
				refunds <- refundResult{value, err}
			}()
			go func() {
				<-start
				value, err := service.Redeem(ctx, fixture.merchantID, paid.VoucherCode, fixture.now)
				redemptions <- redeemResult{value, err}
			}()
			close(start)
			refund, redemption := <-refunds, <-redemptions
			switch {
			case refund.err == nil:
				refundWins++
				if !errors.Is(redemption.err, order.ErrOrderStateConflict) {
					t.Fatalf("refund won but redemption error=%v", redemption.err)
				}
				if refund.value.Purpose != order.NormalRefund || refund.value.Status != order.RefundPending {
					t.Fatalf("unexpected normal refund=%+v", refund.value)
				}
				assertFulfillmentFacts(t, ctx, db, fixture, order.Refunding, 0)
				for range 2 {
					result, err := service.ApplyRefundResult(ctx, refund.value.RequestID, order.RefundSucceeded, fixture.now)
					if err != nil || result.Status != string(order.RefundSucceeded) {
						t.Fatalf("refund success/replay=%+v err=%v", result, err)
					}
				}
				assertFulfillmentCount(t, ctx, db, "redemptions", fixture.order.ID, 0)
				assertFulfillmentFacts(t, ctx, db, fixture, order.Refunded, 1)
			case redemption.err == nil:
				redemptionWins++
				if !errors.Is(refund.err, order.ErrOrderStateConflict) || redemption.value.Status != order.Redeemed {
					t.Fatalf("redemption won but refund error=%v order=%s", refund.err, redemption.value.Status)
				}
				assertFulfillmentCount(t, ctx, db, "refunds", fixture.order.ID, 0)
				assertFulfillmentCount(t, ctx, db, "redemptions", fixture.order.ID, 1)
				assertFulfillmentFacts(t, ctx, db, fixture, order.Redeemed, 0)
			default:
				t.Fatalf("neither operation won: refund=%v redemption=%v", refund.err, redemption.err)
			}
		})
	}
	t.Logf("refund-redemption observed winners: rounds=%d refund=%d redemption=%d", rounds, refundWins, redemptionWins)
}

func TestMySQLRefundReplayAndLatePaymentCompensation(t *testing.T) {
	db := fulfillmentTestDB(t)
	t.Run("normal_refund_replay", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		fixture := newFulfillmentFixture(t, ctx, db)
		service := order.NewFulfillmentService(db)
		if _, err := service.ApplySuccessfulPayment(ctx, fixture.payment("normal-refund", fixture.now)); err != nil {
			t.Fatal(err)
		}
		refund, err := service.RequestNormalRefund(ctx, fixture.customerID, fixture.order.ID, "normal-refund", fixture.now)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, err := service.ApplyRefundResult(ctx, refund.RequestID, order.RefundSucceeded, fixture.now); err != nil {
				t.Fatal(err)
			}
		}
		assertFulfillmentFacts(t, ctx, db, fixture, order.Refunded, 1)
		assertFulfillmentCount(t, ctx, db, "refunds", fixture.order.ID, 1)
		var voucherStatus string
		if err := db.QueryRowContext(ctx, `SELECT status FROM vouchers WHERE order_id = ?`, fixture.order.ID).Scan(&voucherStatus); err != nil || voucherStatus != "INVALID" {
			t.Fatalf("refunded voucher status=%s err=%v", voucherStatus, err)
		}
	})
	t.Run("closed_late_payment_refund_replay", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		fixture := newFulfillmentFixture(t, ctx, db)
		service := order.NewFulfillmentService(db)
		now := fixture.order.PaymentDeadline.Add(time.Second)
		for range 2 {
			if _, err := service.CloseExpired(ctx, fixture.order.ID, now); err != nil {
				t.Fatal(err)
			}
		}
		paid, err := service.ApplySuccessfulPayment(ctx, fixture.payment("late", now))
		if err != nil || paid.Outcome != order.PaymentLate || paid.Refund == nil || paid.Refund.Purpose != order.LatePaymentRefund {
			t.Fatalf("late payment outcome=%s refund=%+v err=%v", paid.Outcome, paid.Refund, err)
		}
		for range 3 {
			if _, err := service.ApplyRefundResult(ctx, paid.Refund.RequestID, order.RefundSucceeded, now); err != nil {
				t.Fatal(err)
			}
		}
		assertFulfillmentFacts(t, ctx, db, fixture, order.Closed, 1)
		assertFulfillmentCount(t, ctx, db, "vouchers", fixture.order.ID, 0)
		assertFulfillmentCount(t, ctx, db, "refunds", fixture.order.ID, 1)
	})
}

func fulfillmentTestDB(t *testing.T) *sql.DB {
	t.Helper()
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
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

type fulfillmentFixture struct {
	shopID     uint64
	activityID uint64
	customerID uint64
	merchantID uint64
	order      order.Order
	now        time.Time
	suffix     string
}

func newFulfillmentFixture(t *testing.T, ctx context.Context, db *sql.DB) *fulfillmentFixture {
	t.Helper()
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	f := &fulfillmentFixture{now: time.Now().UTC().Truncate(time.Microsecond), suffix: hex.EncodeToString(random[:])}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insertID := func(query string, args ...any) uint64 {
		t.Helper()
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil || id <= 0 {
			t.Fatalf("fixture ID=%d err=%v", id, err)
		}
		return uint64(id)
	}
	f.shopID = insertID(`INSERT INTO shops (name, address, status) VALUES (?, 'isolated-fulfillment-test', 'OPEN')`, "fulfillment-"+f.suffix)
	f.customerID = insertID(`INSERT INTO users (username, password_hash, role) VALUES (?, 'not-a-login-hash', 'CUSTOMER')`, "fulfill-c-"+f.suffix)
	f.merchantID = insertID(`INSERT INTO users (username, password_hash, role) VALUES (?, 'not-a-login-hash', 'MERCHANT')`, "fulfill-m-"+f.suffix)
	if _, err := tx.ExecContext(ctx, `INSERT INTO merchant_shop_members (user_id, shop_id) VALUES (?, ?)`, f.merchantID, f.shopID); err != nil {
		t.Fatal(err)
	}
	f.activityID = insertID(`INSERT INTO coupon_activities
		(shop_id, title, price_cents, currency, starts_at, ends_at, voucher_valid_until, total_stock, available_stock, admission_state, generation)
		VALUES (?, 'isolated-fulfillment-test', 100, 'CNY', ?, ?, ?, 1, 1, 'OPEN', 1)`,
		f.shopID, f.now.Add(-time.Hour), f.now.Add(time.Hour), f.now.Add(24*time.Hour))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupFulfillmentFixture(t, db, f) })
	f.order, err = order.NewMySQLService(db).Purchase(ctx, f.customerID, f.activityID, "purchase-"+f.suffix, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fulfillmentFixture) payment(label string, now time.Time) order.SuccessfulPayment {
	return order.SuccessfulPayment{UserID: f.customerID, OrderID: f.order.ID,
		AttemptID: "attempt-" + f.suffix + "-" + label, Provider: "LOCAL",
		TransactionID: "transaction-" + f.suffix + "-" + label,
		AmountCents:   f.order.PriceCents, Currency: f.order.Currency, Now: now}
}

func cleanupFulfillmentFixture(t *testing.T, db *sql.DB, f *fulfillmentFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Every delete is constrained to a primary/foreign key captured from this
	// fixture. No database-wide cleanup or table truncation is performed.
	queries := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM redemptions WHERE order_id = ?`, []any{f.order.ID}},
		{`DELETE FROM refunds WHERE order_id = ?`, []any{f.order.ID}},
		{`DELETE FROM vouchers WHERE order_id = ?`, []any{f.order.ID}},
		{`DELETE FROM payments WHERE order_id = ?`, []any{f.order.ID}},
		{`DELETE FROM inventory_movements WHERE order_id = ?`, []any{f.order.ID}},
		{`DELETE FROM orders WHERE id = ?`, []any{f.order.ID}},
		{`DELETE FROM coupon_activities WHERE id = ?`, []any{f.activityID}},
		{`DELETE FROM merchant_shop_members WHERE user_id = ? AND shop_id = ?`, []any{f.merchantID, f.shopID}},
		{`DELETE FROM users WHERE id = ?`, []any{f.customerID}},
		{`DELETE FROM users WHERE id = ?`, []any{f.merchantID}},
		{`DELETE FROM shops WHERE id = ?`, []any{f.shopID}},
	}
	for _, query := range queries {
		if _, err := db.ExecContext(ctx, query.query, query.args...); err != nil {
			t.Errorf("cleanup fixture %s failed: %v", f.suffix, err)
		}
	}
}

func assertFulfillmentFacts(t *testing.T, ctx context.Context, db *sql.DB, f *fulfillmentFixture, wantState order.State, wantReleases int) {
	t.Helper()
	var stock, total, orders, deducts, releases int
	if err := db.QueryRowContext(ctx, `SELECT available_stock, total_stock FROM coupon_activities WHERE id = ?`, f.activityID).Scan(&stock, &total); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE activity_id = ?`, f.activityID).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE order_id = ? AND kind = 'DEDUCT'`, f.order.ID).Scan(&deducts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE order_id = ? AND kind = 'RELEASE'`, f.order.ID).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	var state order.State
	if err := db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = ? AND user_id = ? AND activity_id = ?`, f.order.ID, f.customerID, f.activityID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if stock < 0 || stock > total || orders != 1 || deducts != 1 || releases > 1 || releases != wantReleases || stock != total-deducts+releases || state != wantState {
		t.Fatalf("invariants: state=%s want=%s stock=%d total=%d orders=%d deducts=%d releases=%d want_releases=%d", state, wantState, stock, total, orders, deducts, releases, wantReleases)
	}
}

func assertFulfillmentCount(t *testing.T, ctx context.Context, db *sql.DB, table string, orderID uint64, want int) {
	t.Helper()
	// Table names are fixed by the test, never supplied by a client.
	switch table {
	case "vouchers", "payments", "refunds", "redemptions":
	default:
		t.Fatalf("unexpected assertion table %q", table)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE order_id = ?`, orderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s count=%d, want %d", table, count, want)
	}
}
