package order

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func validPaymentFixture() SuccessfulPayment {
	return SuccessfulPayment{UserID: 1, OrderID: 2, AttemptID: "attempt-1", Provider: "LOCAL",
		TransactionID: "transaction-1", AmountCents: 100, Currency: "CNY",
		Now: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)}
}

func TestSuccessfulPaymentInput(t *testing.T) {
	if err := validateSuccessfulPayment(validPaymentFixture()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*SuccessfulPayment){
		"missing_owner":         func(p *SuccessfulPayment) { p.UserID = 0 },
		"missing_order":         func(p *SuccessfulPayment) { p.OrderID = 0 },
		"missing_attempt":       func(p *SuccessfulPayment) { p.AttemptID = "" },
		"attempt_whitespace":    func(p *SuccessfulPayment) { p.AttemptID = "attempt " },
		"attempt_too_long":      func(p *SuccessfulPayment) { p.AttemptID = strings.Repeat("a", 129) },
		"missing_provider":      func(p *SuccessfulPayment) { p.Provider = "" },
		"provider_too_long":     func(p *SuccessfulPayment) { p.Provider = strings.Repeat("a", 33) },
		"missing_transaction":   func(p *SuccessfulPayment) { p.TransactionID = "" },
		"transaction_control":   func(p *SuccessfulPayment) { p.TransactionID = "tx\n" },
		"non_ascii_transaction": func(p *SuccessfulPayment) { p.TransactionID = "交易" },
		"transaction_too_long":  func(p *SuccessfulPayment) { p.TransactionID = strings.Repeat("t", 129) },
		"lowercase_currency":    func(p *SuccessfulPayment) { p.Currency = "cny" },
		"short_currency":        func(p *SuccessfulPayment) { p.Currency = "CN" },
		"currency_digits":       func(p *SuccessfulPayment) { p.Currency = "C12" },
		"zero_time":             func(p *SuccessfulPayment) { p.Now = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := validPaymentFixture()
			mutate(&input)
			if err := validateSuccessfulPayment(input); !errors.Is(err, ErrInvalidFulfillmentInput) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPaymentAmountAndTransactionIdentity(t *testing.T) {
	input := validPaymentFixture()
	item := Order{PriceCents: input.AmountCents, Currency: input.Currency}
	if err := validatePaymentAmount(item, input); err != nil {
		t.Fatal(err)
	}
	wrong := input
	wrong.AmountCents++
	if err := validatePaymentAmount(item, wrong); !errors.Is(err, ErrPaymentMismatch) {
		t.Fatalf("wrong amount: %v", err)
	}
	wrong = input
	wrong.Currency = "USD"
	if err := validatePaymentAmount(item, wrong); !errors.Is(err, ErrPaymentMismatch) {
		t.Fatalf("wrong currency: %v", err)
	}
	stored := recordedPayment{OrderID: input.OrderID, AttemptID: input.AttemptID,
		Provider: input.Provider, TransactionID: input.TransactionID,
		AmountCents: input.AmountCents, Currency: input.Currency}
	if !paymentMatches(stored, input) {
		t.Fatal("exact payment replay did not match")
	}
	for name, mutate := range map[string]func(*recordedPayment){
		"order":       func(p *recordedPayment) { p.OrderID++ },
		"attempt":     func(p *recordedPayment) { p.AttemptID += "different" },
		"provider":    func(p *recordedPayment) { p.Provider = "OTHER" },
		"transaction": func(p *recordedPayment) { p.TransactionID += "different" },
		"amount":      func(p *recordedPayment) { p.AmountCents++ },
		"currency":    func(p *recordedPayment) { p.Currency = "USD" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := stored
			mutate(&changed)
			if paymentMatches(changed, input) {
				t.Fatal("conflicting payment matched replay")
			}
		})
	}
}

func TestRedemptionFrozenAndExpired(t *testing.T) {
	now := validPaymentFixture().Now
	for _, state := range []State{PendingPayment, Closed, Refunding, Refunded, Redeemed} {
		if err := validateRedemption(state, "ACTIVE", now.Add(time.Hour), now); !errors.Is(err, ErrOrderStateConflict) {
			t.Fatalf("invalid redemption from %s: %v", state, err)
		}
	}
	for _, status := range []string{"INVALID", "REDEEMED", ""} {
		if err := validateRedemption(Paid, status, now.Add(time.Hour), now); !errors.Is(err, ErrVoucherUnavailable) {
			t.Fatalf("invalid voucher status %s: %v", status, err)
		}
	}
	for _, expires := range []time.Time{now, now.Add(-time.Microsecond)} {
		if err := validateRedemption(Paid, "ACTIVE", expires, now); !errors.Is(err, ErrVoucherUnavailable) {
			t.Fatalf("expired voucher accepted: %v", err)
		}
	}
	if err := validateRedemption(Paid, "ACTIVE", now.Add(time.Microsecond), now); err != nil {
		t.Fatal(err)
	}
}

func TestRefundResultMonotonicity(t *testing.T) {
	cases := []struct {
		current string
		next    RefundResult
		apply   bool
		err     error
	}{
		{RefundPending, RefundUnknown, true, nil},
		{RefundPending, RefundSucceeded, true, nil},
		{RefundPending, RefundFailedFinal, true, nil},
		{string(RefundUnknown), RefundUnknown, false, nil},
		{string(RefundUnknown), RefundSucceeded, true, nil},
		{string(RefundUnknown), RefundFailedFinal, true, nil},
		{string(RefundSucceeded), RefundSucceeded, false, nil},
		{string(RefundSucceeded), RefundUnknown, false, nil},
		{string(RefundSucceeded), RefundFailedFinal, false, ErrRefundConflict},
		{string(RefundFailedFinal), RefundFailedFinal, false, nil},
		{string(RefundFailedFinal), RefundUnknown, false, nil},
		{string(RefundFailedFinal), RefundSucceeded, false, ErrRefundConflict},
		{"", RefundSucceeded, false, ErrRefundConflict},
		{RefundPending, "", false, ErrInvalidFulfillmentInput},
	}
	for _, tc := range cases {
		t.Run(tc.current+"/"+string(tc.next), func(t *testing.T) {
			apply, err := shouldApplyRefundResult(tc.current, tc.next)
			if apply != tc.apply || !errors.Is(err, tc.err) {
				t.Fatalf("got apply=%v err=%v; want %v/%v", apply, err, tc.apply, tc.err)
			}
		})
	}
}

func TestStableRefundRequestIDs(t *testing.T) {
	first := normalRefundID(1, "key")
	if first != normalRefundID(1, "key") || !validFulfillmentToken(first, 128) {
		t.Fatal("normal refund ID is not stable or does not fit schema")
	}
	for _, other := range []string{normalRefundID(2, "key"), normalRefundID(1, "Key"), normalRefundID(1, "key2"), compensationRefundID(1)} {
		if other == first {
			t.Fatal("different refund identities collided")
		}
	}
	if compensationRefundID(1) != compensationRefundID(1) || compensationRefundID(1) == compensationRefundID(2) {
		t.Fatal("compensation IDs do not distinguish payment transactions")
	}
}

func TestRandomVoucherPlaintextAndHash(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		code, hash, err := newVoucherCode()
		if err != nil || !validVoucherCode(code) || hash != sha256.Sum256([]byte(code)) {
			t.Fatalf("invalid generated voucher (plaintext omitted): %v", err)
		}
		if seen[code] {
			t.Fatal("voucher code was repeated")
		}
		seen[code] = true
	}
	for _, code := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("G", 64), strings.Repeat("A", 64), strings.Repeat(" ", 64)} {
		if validVoucherCode(code) {
			t.Fatal("invalid voucher representation was accepted")
		}
	}
}

func TestFulfillmentInputFailsBeforeDatabase(t *testing.T) {
	service := NewFulfillmentService(nil)
	ctx := context.Background()
	now := validPaymentFixture().Now
	if _, err := service.ApplySuccessfulPayment(ctx, SuccessfulPayment{}); !errors.Is(err, ErrInvalidFulfillmentInput) {
		t.Fatalf("payment input: %v", err)
	}
	if _, err := service.CloseExpired(ctx, 0, now); !errors.Is(err, ErrInvalidFulfillmentInput) {
		t.Fatalf("closure input: %v", err)
	}
	if _, err := service.Redeem(ctx, 1, "invalid", now); !errors.Is(err, ErrInvalidFulfillmentInput) {
		t.Fatalf("redemption input: %v", err)
	}
	if _, err := service.RequestNormalRefund(ctx, 1, 1, "", now); !errors.Is(err, ErrInvalidFulfillmentInput) {
		t.Fatalf("normal refund input: %v", err)
	}
	if _, err := service.ApplyRefundResult(ctx, "request", "invalid", now); !errors.Is(err, ErrInvalidFulfillmentInput) {
		t.Fatalf("refund result input: %v", err)
	}
}

func TestFulfillmentErrorMapping(t *testing.T) {
	duplicate := &mysql.MySQLError{Number: 1062, Message: "test duplicate"}
	mapped := paymentDBError(fmt.Errorf("wrapped: %w", duplicate))
	if !errors.Is(mapped, ErrPaymentConflict) {
		t.Fatalf("payment duplicate: %v", mapped)
	}
	var original *mysql.MySQLError
	if !errors.As(mapped, &original) || original != duplicate {
		t.Fatal("mapping lost original MySQL cause")
	}
	deadlock := &mysql.MySQLError{Number: 1213}
	if err := paymentDBError(deadlock); !errors.Is(err, ErrTransactionRetryable) {
		t.Fatalf("deadlock did not remain retryable: %v", err)
	}
	if err := fulfillmentDBError("query", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("context cancellation was hidden: %v", err)
	}
}

type fulfillmentSQLResult struct {
	id       int64
	affected int64
	err      error
}

func (r fulfillmentSQLResult) LastInsertId() (int64, error) { return r.id, r.err }
func (r fulfillmentSQLResult) RowsAffected() (int64, error) { return r.affected, r.err }

func TestFulfillmentMutationResults(t *testing.T) {
	for _, affected := range []int64{0, 2} {
		if err := requireOneChanged(driver.RowsAffected(affected), ErrInventoryInvariant); !errors.Is(err, ErrInventoryInvariant) {
			t.Fatalf("affected=%d: %v", affected, err)
		}
	}
	if err := requireOneChanged(driver.RowsAffected(1), ErrInventoryInvariant); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("result unavailable")
	if err := requireOneChanged(fulfillmentSQLResult{err: failure}, ErrInventoryInvariant); !errors.Is(err, failure) {
		t.Fatalf("lost result error: %v", err)
	}
	if id, err := insertedID(fulfillmentSQLResult{id: 123}); err != nil || id != 123 {
		t.Fatalf("inserted ID %d: %v", id, err)
	}
	for _, id := range []int64{0, -1} {
		if _, err := insertedID(fulfillmentSQLResult{id: id}); !errors.Is(err, ErrStorageConflict) {
			t.Fatalf("invalid inserted ID %d: %v", id, err)
		}
	}
	if _, err := insertedID(fulfillmentSQLResult{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("lost inserted ID error: %v", err)
	}
}
