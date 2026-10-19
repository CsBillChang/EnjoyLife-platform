package order

import (
	"errors"
	"testing"
)

var allStates = []State{PendingPayment, Paid, Closed, Refunding, Refunded, Redeemed}

func TestTransitionGraph(t *testing.T) {
	allowed := map[[2]State]bool{
		{PendingPayment, Paid}: true, {PendingPayment, Closed}: true,
		{Paid, Refunding}: true, {Paid, Redeemed}: true,
		{Refunding, Refunded}: true, {Refunding, Paid}: true,
	}
	for _, from := range allStates {
		for _, to := range allStates {
			t.Run(string(from)+"/"+string(to), func(t *testing.T) {
				err := ValidateTransition(from, to)
				if allowed[[2]State{from, to}] {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("expected invalid transition, got %v", err)
				}
			})
		}
	}
}

func TestPaymentAndCloseWinner(t *testing.T) {
	paid, err := DecidePayment(PendingPayment, false)
	if err != nil || paid.Outcome != PaymentApplied || paid.NextState != Paid || !paid.IssueVoucher || paid.RefundPurpose != "" {
		t.Fatalf("first payment: %+v, %v", paid, err)
	}
	if err := ValidateTransition(paid.NextState, Closed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("close must lose after payment: %v", err)
	}
	if err := ValidateTransition(PendingPayment, Closed); err != nil {
		t.Fatal(err)
	}
	late, err := DecidePayment(Closed, false)
	if err != nil || late.Outcome != PaymentLate || late.NextState != Closed || late.IssueVoucher || late.RefundPurpose != LatePaymentRefund {
		t.Fatalf("payment after close: %+v, %v", late, err)
	}
	refunded, err := DecideRefund(Closed, late.RefundPurpose, RefundSucceeded)
	if err != nil || refunded.NextState != Closed || refunded.ReleaseInventory {
		t.Fatalf("late payment refund must not release inventory again: %+v, %v", refunded, err)
	}
}

func TestRefundAndRedemptionWinner(t *testing.T) {
	for _, first := range []State{Refunding, Redeemed} {
		if err := ValidateTransition(Paid, first); err != nil {
			t.Fatal(err)
		}
		second := Refunding
		if first == Refunding {
			second = Redeemed
		}
		if err := ValidateTransition(first, second); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s winner must block %s: %v", first, second, err)
		}
	}
	if _, err := DecideRefund(Redeemed, NormalRefund, RefundSucceeded); !errors.Is(err, ErrInvalidRefund) {
		t.Fatalf("redeemed order cannot finish a normal refund: %v", err)
	}
}

func TestNormalRefundResults(t *testing.T) {
	cases := []struct {
		result RefundResult
		want   RefundSettlement
	}{
		{RefundUnknown, RefundSettlement{NextState: Refunding}},
		{RefundSucceeded, RefundSettlement{NextState: Refunded, ReleaseInventory: true}},
		{RefundFailedFinal, RefundSettlement{NextState: Paid}},
	}
	for _, tc := range cases {
		t.Run(string(tc.result), func(t *testing.T) {
			got, err := DecideRefund(Refunding, NormalRefund, tc.result)
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
	if _, err := DecideRefund(Refunded, NormalRefund, RefundSucceeded); !errors.Is(err, ErrInvalidRefund) {
		t.Fatalf("an already refunded order must not produce another release: %v", err)
	}
}

func TestExtraPaymentDoesNotChangeFulfillment(t *testing.T) {
	for _, state := range []State{Paid, Refunding, Refunded, Redeemed} {
		t.Run(string(state), func(t *testing.T) {
			payment, err := DecidePayment(state, false)
			if err != nil || payment.Outcome != PaymentExtra || payment.NextState != state || payment.IssueVoucher || payment.RefundPurpose != ExtraPaymentRefund {
				t.Fatalf("extra payment: %+v, %v", payment, err)
			}
			for _, result := range []RefundResult{RefundUnknown, RefundSucceeded, RefundFailedFinal} {
				refund, err := DecideRefund(state, ExtraPaymentRefund, result)
				if err != nil || refund.NextState != state || refund.ReleaseInventory {
					t.Fatalf("compensation %s: %+v, %v", result, refund, err)
				}
			}
		})
	}
}

func TestRecordedPaymentIsAlwaysANoOp(t *testing.T) {
	for _, state := range allStates {
		got, err := DecidePayment(state, true)
		want := PaymentSettlement{Outcome: PaymentDuplicate, NextState: state}
		if err != nil || got != want {
			t.Fatalf("duplicate callback in %s: %+v, %v", state, got, err)
		}
	}
}

func TestInvalidInputs(t *testing.T) {
	for _, pair := range [][2]State{{"", Paid}, {Paid, "UNKNOWN"}} {
		if err := ValidateTransition(pair[0], pair[1]); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid state: %v", err)
		}
	}
	if _, err := DecidePayment("", false); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid payment state: %v", err)
	}
	if _, err := DecideRefund("", NormalRefund, RefundSucceeded); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid refund state: %v", err)
	}
	for _, tc := range []struct {
		state   State
		purpose RefundPurpose
		result  RefundResult
	}{
		{Refunding, "", RefundSucceeded},
		{Refunding, NormalRefund, ""},
		{Paid, LatePaymentRefund, RefundSucceeded},
		{Closed, ExtraPaymentRefund, RefundSucceeded},
		{PendingPayment, ExtraPaymentRefund, RefundSucceeded},
		{Closed, NormalRefund, RefundSucceeded},
	} {
		if _, err := DecideRefund(tc.state, tc.purpose, tc.result); !errors.Is(err, ErrInvalidRefund) {
			t.Fatalf("expected invalid refund for %+v: %v", tc, err)
		}
	}
}
