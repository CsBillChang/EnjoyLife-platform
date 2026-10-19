package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
)

const (
	FailureUserReserved       = "USER_RESERVED"
	FailureAdmissionCancelled = "ADMISSION_CANCELLED"
)

// ReservationStore deliberately excludes Initialize: normal traffic cannot
// rebuild a generation. Release must tombstone the request even on a no-op so
// that an in-flight Admit cannot recreate a cancelled reservation afterwards.
type ReservationStore interface {
	Admit(context.Context, admission.Request) (admission.Result, error)
	Release(context.Context, uint64, uint64, uint64, string) (admission.Result, error)
	MarkOrdered(context.Context, uint64, uint64, uint64, string) error
}

type AdmissionRepository interface {
	Receive(context.Context, uint64, uint64, string, time.Time) (PurchaseRequest, error)
	GetOwnedRequest(context.Context, uint64, string) (PurchaseRequest, error)
	AdmissionSnapshot(context.Context, string) (PurchaseRequest, admission.Request, error)
	AdmitReserved(context.Context, string, uint64, time.Time) (PurchaseRequest, error)
	FailAdmission(context.Context, string, uint64, string, time.Time) (PurchaseRequest, error)
	CompleteReservation(context.Context, string, uint64, ReservationState) (PurchaseRequest, error)
}

type AdmissionCoordinator struct {
	requests     AdmissionRepository
	reservations ReservationStore
}

func NewAdmissionCoordinator(requests AdmissionRepository, reservations ReservationStore) *AdmissionCoordinator {
	return &AdmissionCoordinator{requests: requests, reservations: reservations}
}

func (c *AdmissionCoordinator) Accept(ctx context.Context, userID, activityID uint64, key string, now time.Time) (PurchaseRequest, error) {
	item, err := c.requests.Receive(ctx, userID, activityID, key, now)
	if err != nil {
		return item, err
	}
	return c.Resume(ctx, item.RequestID, now)
}

func (c *AdmissionCoordinator) GetOwnedRequest(ctx context.Context, userID uint64, requestID string) (PurchaseRequest, error) {
	return c.requests.GetOwnedRequest(ctx, userID, requestID)
}

// Resume is for trusted recovery workers, not an unauthenticated HTTP lookup.
// FAILED rows are durable release intents; SUCCEEDED rows are durable marker
// intents. Scan both terminal states until their Redis repair has succeeded.
func (c *AdmissionCoordinator) Resume(ctx context.Context, requestID string, now time.Time) (PurchaseRequest, error) {
	item, reservation, err := c.requests.AdmissionSnapshot(ctx, requestID)
	if err != nil {
		return item, err
	}
	if item.Status != RequestReceived {
		return c.reconcile(ctx, item)
	}
	result, reserveErr := c.reservations.Admit(ctx, reservation)
	if reserveErr != nil {
		code := admissionFailure(reserveErr)
		if code == "" {
			return item, fmt.Errorf("reserve admission: %w", reserveErr)
		}
		item, err = c.requests.FailAdmission(ctx, requestID, item.Generation, code, now)
	} else {
		if result != admission.Admitted && result != admission.Replay {
			return item, fmt.Errorf("%w: admission result %q", admission.ErrUnexpectedScriptResult, result)
		}
		item, err = c.requests.AdmitReserved(ctx, requestID, item.Generation, now)
	}
	// A failed/ambiguous DB commit must never trigger a speculative release.
	// Another Resume reads the authoritative state and repairs the same owner.
	if err != nil {
		return item, err
	}
	return c.reconcile(ctx, item)
}

func (c *AdmissionCoordinator) reconcile(ctx context.Context, item PurchaseRequest) (PurchaseRequest, error) {
	if item.ReservationState != ReservationPending && item.ReservationState != ReservationReserved {
		return item, nil
	}
	switch item.Status {
	case RequestFailed:
		_, err := c.reservations.Release(ctx, item.ActivityID, item.Generation, item.UserID, item.RequestID)
		if errors.Is(err, admission.ErrReservationNotOwned) {
			err = nil
		}
		if err != nil {
			return item, err
		}
		return c.requests.CompleteReservation(ctx, item.RequestID, item.Generation, ReservationReleased)
	case RequestSucceeded:
		if err := c.reservations.MarkOrdered(ctx, item.ActivityID, item.Generation, item.UserID, item.RequestID); err != nil {
			return item, err
		}
		return c.requests.CompleteReservation(ctx, item.RequestID, item.Generation, ReservationCommitted)
	case RequestAdmitted, RequestReceived:
		return item, nil
	default:
		return item, ErrStorageConflict
	}
}

func admissionFailure(err error) string {
	switch {
	case errors.Is(err, admission.ErrSoldOut):
		return FailureSoldOut
	case errors.Is(err, admission.ErrUserReserved):
		return FailureUserReserved
	case errors.Is(err, admission.ErrAdmissionDisabled):
		return FailureActivityUnavailable
	case errors.Is(err, admission.ErrProcessingDeadline):
		return FailureProcessingExpired
	case errors.Is(err, admission.ErrAcceptedOutsideWindow):
		return FailureInvalidAcceptedAt
	case errors.Is(err, admission.ErrGenerationMismatch):
		return FailureGenerationChanged
	case errors.Is(err, admission.ErrRequestCancelled):
		return FailureAdmissionCancelled
	default:
		return ""
	}
}

// AdmissionSnapshot reads only server-owned data. Current activity dates are
// rechecked under lock by AdmitReserved; Redis never determines final validity.
func (s *AsyncService) AdmissionSnapshot(ctx context.Context, requestID string) (PurchaseRequest, admission.Request, error) {
	if !validRequestUUID(requestID) {
		return PurchaseRequest{}, admission.Request{}, ErrInvalidPurchaseInput
	}
	item, err := scanPurchaseRequest(s.db.QueryRowContext(ctx, selectPurchaseRequestColumns+` WHERE request_id = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return item, admission.Request{}, ErrRequestNotFound
	}
	if err != nil {
		return item, admission.Request{}, asyncDBError("read admission request", err)
	}
	r := admission.Request{ActivityID: item.ActivityID, Generation: item.Generation, UserID: item.UserID, RequestID: item.RequestID,
		AcceptedAt: item.AcceptedAt, ProcessingDeadline: item.ProcessingDeadline}
	if item.Status == RequestReceived {
		err = s.db.QueryRowContext(ctx, `SELECT starts_at, ends_at FROM coupon_activities WHERE id = ?`, item.ActivityID).Scan(&r.StartsAt, &r.EndsAt)
		if err != nil {
			return item, r, asyncDBError("read admission window", err)
		}
	}
	return item, r, nil
}

func (s *AsyncService) AdmitReserved(ctx context.Context, requestID string, generation uint64, now time.Time) (PurchaseRequest, error) {
	return s.finishAdmission(ctx, requestID, generation, "", now)
}

// FailAdmission only fails RECEIVED. A stale Redis failure cannot cancel an
// ADMITTED request or release the reservation of an order committed meanwhile.
func (s *AsyncService) FailAdmission(ctx context.Context, requestID string, generation uint64, code string, now time.Time) (PurchaseRequest, error) {
	switch code {
	case FailureSoldOut, FailureUserReserved, FailureActivityUnavailable, FailureProcessingExpired, FailureInvalidAcceptedAt, FailureGenerationChanged, FailureAdmissionCancelled:
	default:
		return PurchaseRequest{}, ErrInvalidPurchaseInput
	}
	return s.finishAdmission(ctx, requestID, generation, code, now)
}

func (s *AsyncService) finishAdmission(ctx context.Context, requestID string, generation uint64, failure string, now time.Time) (PurchaseRequest, error) {
	if !validRequestUUID(requestID) || generation == 0 || !validFulfillmentTime(now) {
		return PurchaseRequest{}, ErrInvalidPurchaseInput
	}
	var activityID uint64
	if err := s.db.QueryRowContext(ctx, `SELECT activity_id FROM purchase_requests WHERE request_id = ?`, requestID).Scan(&activityID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PurchaseRequest{}, ErrRequestNotFound
		}
		return PurchaseRequest{}, asyncDBError("locate admission request", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return PurchaseRequest{}, asyncDBError("begin admission", err)
	}
	defer tx.Rollback()
	activity, err := lockAsyncActivity(ctx, tx, activityID)
	if err != nil {
		return PurchaseRequest{}, err
	}
	item, err := scanPurchaseRequest(tx.QueryRowContext(ctx, selectPurchaseRequestColumns+` WHERE request_id = ? FOR UPDATE`, requestID))
	if err != nil {
		return PurchaseRequest{}, asyncDBError("lock admission request", err)
	}
	if item.ActivityID != activityID {
		return item, ErrStorageConflict
	}
	if item.Generation != generation {
		return item, ErrRequestGenerationMismatch
	}
	if item.Status != RequestReceived {
		return item, nil
	}
	// Verify authoritative order evidence BEFORE choosing any failed state.
	existing, err := scanOrder(tx.QueryRowContext(ctx, selectOrderColumns+` WHERE request_id = ?`, requestID))
	if err == nil {
		if !orderMatchesRequest(existing, item) {
			return item, ErrStorageConflict
		}
		return succeedPurchaseRequest(ctx, tx, item, existing.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return item, asyncDBError("check admission order", err)
	}
	now = s.clock().UTC().Truncate(time.Microsecond)
	if !validFulfillmentTime(now) {
		return item, ErrInvalidPurchaseInput
	}
	if fence := validateReservedAdmission(item, activity, now); fence != "" {
		failure = fence
	}
	if failure == "" {
		failure, err = existingUserOrderConflict(ctx, tx, item.UserID, item.ActivityID, item.IdempotencyKey)
		if err != nil {
			return item, err
		}
	}
	if failure != "" {
		return failPurchaseRequest(ctx, tx, item, failure)
	}
	update, err := tx.ExecContext(ctx, `UPDATE purchase_requests SET status = 'ADMITTED', reservation_state = 'RESERVED' WHERE id = ? AND status = 'RECEIVED' AND generation = ? AND reservation_state = 'PENDING'`, item.ID, generation)
	if err != nil {
		return item, asyncDBError("admit reserved request", err)
	}
	if err := requireOneChanged(update, ErrStorageConflict); err != nil {
		return item, err
	}
	if err := writePurchaseOutbox(ctx, tx, requestID, now); err != nil {
		return item, err
	}
	if err := tx.Commit(); err != nil {
		return item, asyncDBError("commit reserved admission", err)
	}
	item.Status = RequestAdmitted
	item.ReservationState = ReservationReserved
	return item, nil
}

// CompleteReservation records Redis repair only after its idempotent action.
// The conditional update fences terminal status and generation. If its commit
// response is lost, replay reads the same target state instead of doing damage.
func (s *AsyncService) CompleteReservation(ctx context.Context, requestID string, generation uint64, target ReservationState) (PurchaseRequest, error) {
	if !validRequestUUID(requestID) || generation == 0 {
		return PurchaseRequest{}, ErrInvalidPurchaseInput
	}
	var status RequestStatus
	switch target {
	case ReservationReleased:
		status = RequestFailed
	case ReservationCommitted:
		status = RequestSucceeded
	default:
		return PurchaseRequest{}, ErrInvalidPurchaseInput
	}
	_, err := s.db.ExecContext(ctx, `UPDATE purchase_requests SET reservation_state = ?
		WHERE request_id = ? AND generation = ? AND status = ? AND reservation_state IN ('PENDING', 'RESERVED')`, target, requestID, generation, status)
	if err != nil {
		return PurchaseRequest{}, asyncDBError("complete reservation repair", err)
	}
	item, err := scanPurchaseRequest(s.db.QueryRowContext(ctx, selectPurchaseRequestColumns+` WHERE request_id = ?`, requestID))
	if err != nil {
		return item, asyncDBError("read reservation repair", err)
	}
	if item.Generation != generation {
		return item, ErrRequestGenerationMismatch
	}
	if item.Status != status || item.ReservationState != target {
		return item, ErrStorageConflict
	}
	return item, nil
}

func validateReservedAdmission(item PurchaseRequest, activity asyncActivity, now time.Time) string {
	if item.Generation != activity.Generation {
		return FailureGenerationChanged
	}
	if activity.AdmissionState != "OPEN" {
		return FailureActivityUnavailable
	}
	if !now.Before(item.ProcessingDeadline) {
		return FailureProcessingExpired
	}
	return validateAcceptedSnapshot(item, activity)
}
