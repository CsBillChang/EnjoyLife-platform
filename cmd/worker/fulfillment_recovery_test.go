package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/payment"
)

type recoveryServiceFake struct {
	expired   []uint64
	closed    []uint64
	refunds   []order.PendingRefund
	applied   []string
	closeErrs map[uint64]error
}

func (f *recoveryServiceFake) ListExpiredOrderIDs(_ context.Context, after uint64, limit int, _ time.Time) ([]uint64, error) {
	return uint64Page(f.expired, after, limit), nil
}
func (f *recoveryServiceFake) CloseExpired(_ context.Context, id uint64, _ time.Time) (order.Order, error) {
	f.closed = append(f.closed, id)
	return order.Order{}, f.closeErrs[id]
}
func (f *recoveryServiceFake) ListPendingRefunds(_ context.Context, after uint64, limit int) ([]order.PendingRefund, error) {
	result := make([]order.PendingRefund, 0, limit)
	for _, item := range f.refunds {
		if item.ID > after && len(result) < limit {
			result = append(result, item)
		}
	}
	return result, nil
}
func (f *recoveryServiceFake) ApplyRefundResult(_ context.Context, requestID string, result order.RefundResult, _ time.Time) (order.Refund, error) {
	if result != order.RefundSucceeded {
		return order.Refund{}, errors.New("unexpected result")
	}
	f.applied = append(f.applied, requestID)
	return order.Refund{RequestID: requestID, Status: string(result)}, nil
}

func uint64Page(values []uint64, after uint64, limit int) []uint64 {
	result := make([]uint64, 0, limit)
	for _, value := range values {
		if value > after && len(result) < limit {
			result = append(result, value)
		}
	}
	return result
}

func TestRecoverExpiredOrdersContinuesPastConcurrentWinner(t *testing.T) {
	service := &recoveryServiceFake{expired: []uint64{1, 2, 3}, closeErrs: map[uint64]error{2: order.ErrOrderStateConflict}}
	if err := recoverExpiredOrders(context.Background(), service, 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(service.closed) != 3 || service.closed[2] != 3 {
		t.Fatalf("closed=%v", service.closed)
	}
}

func TestRecoverPendingRefundsUsesStableLocalProviderIdentity(t *testing.T) {
	service := &recoveryServiceFake{refunds: []order.PendingRefund{{Refund: order.Refund{ID: 7, PaymentID: 9, RequestID: "refund-7", AmountCents: 100, Currency: "CNY", Status: order.RefundPending}}}}
	provider := payment.NewSimulator()
	if err := recoverPendingRefunds(context.Background(), service, provider, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(service.applied) != 1 || service.applied[0] != "refund-7" {
		t.Fatalf("applied=%v", service.applied)
	}
	result, err := provider.QueryRefund("refund-7")
	if err != nil || result.Status != payment.StatusSuccess {
		t.Fatalf("provider result=%+v err=%v", result, err)
	}
}

type stockRepositoryFake struct {
	tasks      []stockRestoreTask
	generation uint64
	succeeded  []uint64
	retried    []uint64
}

func (f *stockRepositoryFake) Claim(_ context.Context, _ time.Time) (stockRestoreTask, bool, error) {
	if len(f.tasks) == 0 {
		return stockRestoreTask{}, false, nil
	}
	task := f.tasks[0]
	f.tasks = f.tasks[1:]
	return task, true, nil
}
func (f *stockRepositoryFake) CurrentGeneration(context.Context, uint64) (uint64, error) {
	return f.generation, nil
}
func (f *stockRepositoryFake) Succeed(_ context.Context, task stockRestoreTask) error {
	f.succeeded = append(f.succeeded, task.ID)
	return nil
}
func (f *stockRepositoryFake) Retry(_ context.Context, task stockRestoreTask, _ time.Time, _ error) error {
	f.retried = append(f.retried, task.ID)
	return nil
}

type stockRestorerFake struct{ calls int }

func (f *stockRestorerFake) RestoreCommittedStock(context.Context, uint64, uint64, uint64) (admission.Result, error) {
	f.calls++
	return admission.Released, nil
}

func TestRecoverCommittedStockRestoresCurrentAndAbsorbsOldGeneration(t *testing.T) {
	repository := &stockRepositoryFake{generation: 3, tasks: []stockRestoreTask{
		{ID: 1, Generation: 3, Payload: order.StockRestorePayload{ActivityID: 10, Generation: 3, OrderID: 100}},
		{ID: 2, Generation: 2, Payload: order.StockRestorePayload{ActivityID: 10, Generation: 2, OrderID: 101}},
	}}
	restorer := &stockRestorerFake{}
	if err := recoverCommittedStock(context.Background(), repository, restorer, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	if restorer.calls != 1 || len(repository.succeeded) != 2 || len(repository.retried) != 0 {
		t.Fatalf("calls=%d succeeded=%v retried=%v", restorer.calls, repository.succeeded, repository.retried)
	}
}
