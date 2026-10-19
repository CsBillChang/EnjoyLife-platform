package order

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
)

type admissionRepoFake struct {
	item                  PurchaseRequest
	err                   error
	finishErr             error
	repairErr             error
	failCalls, admitCalls int
}

func (f *admissionRepoFake) Receive(context.Context, uint64, uint64, string, time.Time) (PurchaseRequest, error) {
	return f.item, f.err
}
func (f *admissionRepoFake) GetOwnedRequest(context.Context, uint64, string) (PurchaseRequest, error) {
	return f.item, f.err
}
func (f *admissionRepoFake) AdmissionSnapshot(context.Context, string) (PurchaseRequest, admission.Request, error) {
	return f.item, admission.Request{RequestID: f.item.RequestID}, f.err
}
func (f *admissionRepoFake) CompleteReservation(_ context.Context, _ string, _ uint64, target ReservationState) (PurchaseRequest, error) {
	if f.repairErr == nil {
		f.item.ReservationState = target
	}
	return f.item, f.repairErr
}
func (f *admissionRepoFake) AdmitReserved(context.Context, string, uint64, time.Time) (PurchaseRequest, error) {
	f.admitCalls++
	if f.finishErr == nil {
		f.item.Status = RequestAdmitted
		f.item.ReservationState = ReservationReserved
	}
	return f.item, f.finishErr
}
func (f *admissionRepoFake) FailAdmission(_ context.Context, _ string, _ uint64, code string, _ time.Time) (PurchaseRequest, error) {
	f.failCalls++
	if f.finishErr == nil {
		f.item.Status = RequestFailed
		f.item.ErrorCode = code
	}
	return f.item, f.finishErr
}

type reservationFake struct {
	err, releaseErr, markErr            error
	admitCalls, releaseCalls, markCalls int
	result                              admission.Result
}

func (f *reservationFake) Admit(context.Context, admission.Request) (admission.Result, error) {
	f.admitCalls++
	return f.result, f.err
}
func (f *reservationFake) Release(context.Context, uint64, uint64, uint64, string) (admission.Result, error) {
	f.releaseCalls++
	return admission.Released, f.releaseErr
}
func (f *reservationFake) MarkOrdered(context.Context, uint64, uint64, uint64, string) error {
	f.markCalls++
	return f.markErr
}

func TestAdmissionCoordinatorDefiniteFailuresAndTransientErrors(t *testing.T) {
	transient := errors.New("connection lost")
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"soldout", admission.ErrSoldOut, FailureSoldOut},
		{"reserved", admission.ErrUserReserved, FailureUserReserved},
		{"disabled", admission.ErrAdmissionDisabled, FailureActivityUnavailable},
		{"deadline", admission.ErrProcessingDeadline, FailureProcessingExpired},
		{"window", admission.ErrAcceptedOutsideWindow, FailureInvalidAcceptedAt},
		{"generation", admission.ErrGenerationMismatch, FailureGenerationChanged},
		{"cancelled", admission.ErrRequestCancelled, FailureAdmissionCancelled},
		{"network", transient, ""},
		{"missing", admission.ErrStateMissing, ""},
		{"partial", admission.ErrStatePartial, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item, _ := asyncRuleFixture()
			item.Status = RequestReceived
			item.ReservationState = ReservationPending
			repo := &admissionRepoFake{item: item}
			redis := &reservationFake{err: fmt.Errorf("wrapped: %w", tc.err)}
			got, err := NewAdmissionCoordinator(repo, redis).Accept(context.Background(), item.UserID, item.ActivityID, item.IdempotencyKey, item.AcceptedAt)
			if tc.code == "" {
				if !errors.Is(err, tc.err) || got.Status != RequestReceived || repo.failCalls != 0 || redis.releaseCalls != 0 {
					t.Fatalf("transient became terminal: %+v %v", got, err)
				}
			} else {
				if err != nil || got.Status != RequestFailed || got.ErrorCode != tc.code || repo.failCalls != 1 || redis.releaseCalls != 1 {
					t.Fatalf("failure not durably compensated: %+v %v", got, err)
				}
			}
		})
	}
}

func TestAdmissionCoordinatorAmbiguousCommitNeverReleases(t *testing.T) {
	for _, reserveErr := range []error{nil, admission.ErrSoldOut} {
		item, _ := asyncRuleFixture()
		item.Status = RequestReceived
		item.ReservationState = ReservationPending
		lost := errors.New("commit response lost")
		repo := &admissionRepoFake{item: item, finishErr: lost}
		redis := &reservationFake{result: admission.Replay, err: reserveErr}
		_, err := NewAdmissionCoordinator(repo, redis).Resume(context.Background(), item.RequestID, item.AcceptedAt)
		if !errors.Is(err, lost) || redis.releaseCalls != 0 {
			t.Fatalf("ambiguous commit released slot: %v", err)
		}
	}
}

func TestAdmissionCoordinatorReplayAndRepair(t *testing.T) {
	for _, state := range []RequestStatus{RequestReceived, RequestAdmitted, RequestSucceeded, RequestFailed} {
		t.Run(string(state), func(t *testing.T) {
			item, _ := asyncRuleFixture()
			item.Status = state
			item.ReservationState = ReservationPending
			repo := &admissionRepoFake{item: item}
			redis := &reservationFake{result: admission.Replay, releaseErr: admission.ErrReservationNotOwned}
			c := NewAdmissionCoordinator(repo, redis)
			for range 2 {
				if _, err := c.Resume(context.Background(), item.RequestID, item.AcceptedAt); err != nil {
					t.Fatal(err)
				}
			}
			if state == RequestReceived {
				if redis.admitCalls != 1 || repo.admitCalls != 1 {
					t.Fatal("replay repeated admission")
				}
			} else if redis.admitCalls != 0 {
				t.Fatal("terminal replay attempted reservation")
			}
			if state == RequestFailed && redis.releaseCalls != 1 {
				t.Fatal("FAILED is not recoverable release intent")
			}
			if state == RequestSucceeded && redis.markCalls != 1 {
				t.Fatal("SUCCEEDED not reconciled")
			}
		})
	}
}

func TestAdmissionCoordinatorRepairFailureRemainsObservable(t *testing.T) {
	item, _ := asyncRuleFixture()
	item.Status = RequestFailed
	item.ReservationState = ReservationPending
	lost := errors.New("redis offline")
	repo := &admissionRepoFake{item: item}
	redis := &reservationFake{releaseErr: lost}
	got, err := NewAdmissionCoordinator(repo, redis).Resume(context.Background(), item.RequestID, item.AcceptedAt)
	if got.Status != RequestFailed || !errors.Is(err, lost) {
		t.Fatalf("repair was silently discarded: %+v %v", got, err)
	}
}

func TestAdmissionCoordinatorRepairCommitLostCanReplay(t *testing.T) {
	for _, status := range []RequestStatus{RequestFailed, RequestSucceeded} {
		item, _ := asyncRuleFixture()
		item.Status = status
		item.ReservationState = ReservationReserved
		lost := errors.New("repair commit lost")
		repo := &admissionRepoFake{item: item, repairErr: lost}
		redis := &reservationFake{}
		c := NewAdmissionCoordinator(repo, redis)
		if _, err := c.Resume(context.Background(), item.RequestID, item.AcceptedAt); !errors.Is(err, lost) {
			t.Fatal(err)
		}
		repo.repairErr = nil
		if _, err := c.Resume(context.Background(), item.RequestID, item.AcceptedAt); err != nil {
			t.Fatal(err)
		}
		if status == RequestFailed && redis.releaseCalls != 2 {
			t.Fatal("release retry was lost")
		}
		if status == RequestSucceeded && redis.markCalls != 2 {
			t.Fatal("ordered-marker retry was lost")
		}
	}
}

func TestAdmissionCoordinatorSkipsLegacyAndCompletedRepair(t *testing.T) {
	for _, state := range []ReservationState{ReservationNone, ReservationCommitted, ReservationReleased} {
		for _, status := range []RequestStatus{RequestFailed, RequestSucceeded} {
			item, _ := asyncRuleFixture()
			item.Status = status
			item.ReservationState = state
			repo := &admissionRepoFake{item: item}
			redis := &reservationFake{}
			if _, err := NewAdmissionCoordinator(repo, redis).Resume(context.Background(), item.RequestID, item.AcceptedAt); err != nil {
				t.Fatal(err)
			}
			if redis.releaseCalls+redis.markCalls+redis.admitCalls != 0 {
				t.Fatal("legacy/completed request touched Redis")
			}
		}
	}
}

func TestReservedAdmissionFenceRules(t *testing.T) {
	item, activity := asyncRuleFixture()
	item.Status = RequestReceived
	if code := validateReservedAdmission(item, activity, activity.EndsAt.Add(time.Second)); code != "" {
		t.Fatalf("current sale end replaced accepted_at: %s", code)
	}
	if code := validateReservedAdmission(item, activity, item.ProcessingDeadline); code != FailureProcessingExpired {
		t.Fatal(code)
	}
	activity.Generation++
	if code := validateReservedAdmission(item, activity, item.AcceptedAt); code != FailureGenerationChanged {
		t.Fatal(code)
	}
	activity.Generation--
	activity.AdmissionState = "PAUSED"
	if code := validateReservedAdmission(item, activity, item.AcceptedAt); code != FailureActivityUnavailable {
		t.Fatal(code)
	}
}
