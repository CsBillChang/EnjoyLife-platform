package payment

import (
	"errors"
	"sync"
	"testing"
)

func TestChargeIsIdempotentForSameAttemptAndParameters(t *testing.T) {
	simulator := NewSimulator()
	request := ChargeRequest{AttemptID: "attempt-1", OrderID: "order-1", Amount: 1099, Currency: "cny", InitialStatus: StatusSuccess}

	first, err := simulator.Charge(request)
	if err != nil {
		t.Fatalf("first charge: %v", err)
	}
	second, err := simulator.Charge(request)
	if err != nil {
		t.Fatalf("repeated charge: %v", err)
	}
	if first != second {
		t.Fatalf("same request should return same result: first=%+v second=%+v", first, second)
	}
}

func TestChargeRejectsIdempotencyParameterConflict(t *testing.T) {
	simulator := NewSimulator()
	_, err := simulator.Charge(ChargeRequest{AttemptID: "attempt-1", OrderID: "order-1", Amount: 1099, Currency: "CNY", InitialStatus: StatusSuccess})
	if err != nil {
		t.Fatalf("first charge: %v", err)
	}

	_, err = simulator.Charge(ChargeRequest{AttemptID: "attempt-1", OrderID: "order-1", Amount: 1100, Currency: "CNY", InitialStatus: StatusSuccess})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting amount error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestQueryCanResolveUnknownChargeWhenControlled(t *testing.T) {
	simulator := NewSimulator()
	created, err := simulator.Charge(ChargeRequest{AttemptID: "attempt-unknown", OrderID: "order-1", Amount: 1099, Currency: "CNY", InitialStatus: StatusUnknown})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if created.Status != StatusUnknown {
		t.Fatalf("created status = %s, want UNKNOWN", created.Status)
	}

	if err := simulator.SetChargeQueryStatus("attempt-unknown", StatusSuccess); err != nil {
		t.Fatalf("resolve query: %v", err)
	}
	queried, err := simulator.Query("attempt-unknown")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if queried.Status != StatusSuccess || queried.ProviderTransactionID != created.ProviderTransactionID {
		t.Fatalf("query result = %+v, created = %+v", queried, created)
	}
}

func TestRefundSupportsFinalFailureAndIdempotency(t *testing.T) {
	simulator := NewSimulator()
	request := RefundRequest{RefundRequestID: "refund-1", ProviderTransactionID: "pay_000001", Amount: 1099, Currency: "CNY", InitialStatus: StatusFailedFinal}

	first, err := simulator.Refund(request)
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}
	if first.Status != StatusFailedFinal {
		t.Fatalf("refund status = %s, want FAILED_FINAL", first.Status)
	}
	second, err := simulator.Refund(request)
	if err != nil {
		t.Fatalf("repeated refund: %v", err)
	}
	if first != second {
		t.Fatalf("same refund request should return same result: first=%+v second=%+v", first, second)
	}

	_, err = simulator.Refund(RefundRequest{RefundRequestID: "refund-1", ProviderTransactionID: "pay_000001", Amount: 1100, Currency: "CNY", InitialStatus: StatusFailedFinal})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting refund error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestChargeRejectsInvalidAmountAndCurrency(t *testing.T) {
	simulator := NewSimulator()
	_, err := simulator.Charge(ChargeRequest{AttemptID: "attempt-1", Amount: 0, Currency: "CNY", InitialStatus: StatusSuccess})
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero amount error = %v, want ErrInvalidAmount", err)
	}
	_, err = simulator.Charge(ChargeRequest{AttemptID: "attempt-2", Amount: 1, Currency: "  ", InitialStatus: StatusSuccess})
	if !errors.Is(err, ErrEmptyCurrency) {
		t.Fatalf("empty currency error = %v, want ErrEmptyCurrency", err)
	}
}

func TestConcurrentSameAttemptCreatesOneProviderTransaction(t *testing.T) {
	simulator := NewSimulator()
	request := ChargeRequest{AttemptID: "attempt-concurrent", OrderID: "order-1", Amount: 1099, Currency: "CNY", InitialStatus: StatusSuccess}

	const callers = 64
	results := make(chan ChargeResult, callers)
	errs := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			result, err := simulator.Charge(request)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent charge: %v", err)
	}
	var providerTransactionID string
	for result := range results {
		if providerTransactionID == "" {
			providerTransactionID = result.ProviderTransactionID
		}
		if result.ProviderTransactionID != providerTransactionID {
			t.Fatalf("created more than one provider transaction: %q and %q", providerTransactionID, result.ProviderTransactionID)
		}
	}
}
