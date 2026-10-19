package order

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrRequestNotFound           = errors.New("purchase request not found")
	ErrRequestInProgress         = errors.New("user already has a live purchase request for this activity")
	ErrRequestNotAdmitted        = errors.New("purchase request has not been admitted")
	ErrRequestGenerationMismatch = errors.New("purchase request generation does not match current activity")
)

type RequestStatus string
type ReservationState string

const (
	ReservationNone      ReservationState = "NONE"
	ReservationPending   ReservationState = "PENDING"
	ReservationReserved  ReservationState = "RESERVED"
	ReservationCommitted ReservationState = "COMMITTED"
	ReservationReleased  ReservationState = "RELEASED"
)

const (
	RequestReceived  RequestStatus = "RECEIVED"
	RequestAdmitted  RequestStatus = "ADMITTED"
	RequestSucceeded RequestStatus = "SUCCEEDED"
	RequestFailed    RequestStatus = "FAILED"

	PurchaseScope    = "PURCHASE"
	ProcessingWindow = 2 * time.Minute

	FailureProcessingExpired       = "PROCESSING_EXPIRED"
	FailureSoldOut                 = "SOLD_OUT"
	FailureAlreadyPurchased        = "ALREADY_PURCHASED"
	FailureIdempotencyConflict     = "IDEMPOTENCY_CONFLICT"
	FailureInvalidAcceptedAt       = "INVALID_ACCEPTED_AT"
	FailureActivitySnapshotChanged = "ACTIVITY_SNAPSHOT_CHANGED"
	FailureActivityUnavailable     = "ACTIVITY_UNAVAILABLE"
	FailureGenerationChanged       = "ACTIVITY_GENERATION_CHANGED"
)

type PurchaseRequest struct {
	ID                 uint64           `json:"-"`
	RequestID          string           `json:"request_id"`
	UserID             uint64           `json:"user_id"`
	ActivityID         uint64           `json:"activity_id"`
	Scope              string           `json:"-"`
	IdempotencyKey     string           `json:"-"`
	ParamsDigest       []byte           `json:"-"`
	AcceptedAt         time.Time        `json:"accepted_at"`
	ProcessingDeadline time.Time        `json:"processing_deadline"`
	ShopID             uint64           `json:"shop_id"`
	PriceCents         uint64           `json:"price_cents"`
	Currency           string           `json:"currency"`
	Generation         uint64           `json:"generation"`
	Status             RequestStatus    `json:"status"`
	OrderID            uint64           `json:"order_id,omitempty"`
	ErrorCode          string           `json:"error_code,omitempty"`
	ReservationState   ReservationState `json:"-"`
}

func (s RequestStatus) Terminal() bool {
	return s == RequestSucceeded || s == RequestFailed
}

type AsyncService struct {
	db    *sql.DB
	clock func() time.Time
}

func NewAsyncService(db *sql.DB) *AsyncService {
	return &AsyncService{db: db, clock: time.Now}
}

type asyncActivity struct {
	purchaseActivity
	Generation uint64
}

// Accept persists an ADMITTED request and its outbox event together. It does not
// deduct inventory or promise an order. now is a trusted server timestamp.
func (s *AsyncService) Accept(ctx context.Context, userID, activityID uint64, key string, now time.Time) (PurchaseRequest, error) {
	return s.receive(ctx, userID, activityID, key, now, RequestAdmitted)
}

// Receive commits the acceptance snapshot before touching Redis. RECEIVED is a
// durable retry point, not permission for a consumer to create an order.
func (s *AsyncService) Receive(ctx context.Context, userID, activityID uint64, key string, now time.Time) (PurchaseRequest, error) {
	return s.receive(ctx, userID, activityID, key, now, RequestReceived)
}

func (s *AsyncService) receive(ctx context.Context, userID, activityID uint64, key string, now time.Time, status RequestStatus) (PurchaseRequest, error) {
	if err := validateAsyncAcceptance(userID, activityID, key, now); err != nil {
		return PurchaseRequest{}, err
	}
	now = now.UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return PurchaseRequest{}, asyncDBError("begin request acceptance", err)
	}
	defer tx.Rollback()
	activity, err := lockAsyncActivity(ctx, tx, activityID)
	if err != nil {
		return PurchaseRequest{}, err
	}
	// Generate the authoritative acceptance timestamp after waiting for the
	// activity lock. A timestamp sampled before lock contention could otherwise
	// admit a request after the sale or processing boundary had already passed.
	now = s.clock().UTC().Truncate(time.Microsecond)
	if err := validateAsyncAcceptance(userID, activityID, key, now); err != nil {
		return PurchaseRequest{}, err
	}
	digest := purchaseParamsDigest(activityID)
	previous, err := scanPurchaseRequest(tx.QueryRowContext(ctx, selectPurchaseRequestColumns+
		` WHERE user_id = ? AND scope = ? AND idempotency_key = ?`, userID, PurchaseScope, key))
	if err == nil {
		return matchAcceptedRequest(previous, activityID, digest[:])
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PurchaseRequest{}, asyncDBError("read request replay", err)
	}
	if conflict, err := existingUserOrderConflict(ctx, tx, userID, activityID, key); err != nil {
		return PurchaseRequest{}, err
	} else if conflict != "" {
		if conflict == FailureAlreadyPurchased {
			return PurchaseRequest{}, ErrAlreadyPurchased
		}
		return PurchaseRequest{}, ErrIdempotencyConflict
	}
	var activeID uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM purchase_requests
		WHERE user_id = ? AND activity_id = ? AND status IN ('RECEIVED', 'ADMITTED')
		AND processing_deadline > ? LIMIT 1`, userID, activityID, now).Scan(&activeID)
	if err == nil {
		return PurchaseRequest{}, ErrRequestInProgress
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PurchaseRequest{}, asyncDBError("check active purchase request", err)
	}
	if err := validatePurchaseActivity(activity.purchaseActivity, now); err != nil {
		return PurchaseRequest{}, err
	}
	requestID, err := newRequestID()
	if err != nil {
		return PurchaseRequest{}, fmt.Errorf("generate async request ID: %w", err)
	}
	item := PurchaseRequest{RequestID: requestID, UserID: userID, ActivityID: activityID,
		Scope: PurchaseScope, IdempotencyKey: key, ParamsDigest: digest[:], AcceptedAt: now,
		ProcessingDeadline: now.Add(ProcessingWindow), ShopID: activity.ShopID,
		PriceCents: activity.PriceCents, Currency: activity.Currency, Generation: activity.Generation,
		Status: status}
	item.ReservationState = ReservationNone
	if status == RequestReceived {
		item.ReservationState = ReservationPending
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO purchase_requests
		(request_id, user_id, activity_id, scope, idempotency_key, params_digest, accepted_at,
		processing_deadline, shop_id, price_cents, currency, generation, status, reservation_state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.RequestID, item.UserID, item.ActivityID, item.Scope, item.IdempotencyKey,
		item.ParamsDigest, item.AcceptedAt, item.ProcessingDeadline, item.ShopID,
		item.PriceCents, item.Currency, item.Generation, item.Status, item.ReservationState, now)
	if err != nil {
		mapped := mapPurchaseDBError(err)
		if errors.Is(mapped, ErrStorageConflict) {
			if err := tx.Rollback(); err != nil {
				return PurchaseRequest{}, asyncDBError("rollback acceptance conflict", err)
			}
			// Different activity locks can race on the same user-scoped key.
			previous, readErr := scanPurchaseRequest(s.db.QueryRowContext(ctx, selectPurchaseRequestColumns+
				` WHERE user_id = ? AND scope = ? AND idempotency_key = ?`, userID, PurchaseScope, key))
			if readErr == nil {
				return matchAcceptedRequest(previous, activityID, digest[:])
			}
			if !errors.Is(readErr, sql.ErrNoRows) {
				return PurchaseRequest{}, asyncDBError("resolve acceptance conflict", readErr)
			}
		}
		return PurchaseRequest{}, asyncDBError("insert purchase request", mapped)
	}
	item.ID, err = insertedID(insert)
	if err != nil {
		return PurchaseRequest{}, err
	}
	if status == RequestAdmitted {
		if err := writePurchaseOutbox(ctx, tx, item.RequestID, now); err != nil {
			return PurchaseRequest{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PurchaseRequest{}, asyncDBError("commit purchase acceptance", err)
	}
	return item, nil
}

func writePurchaseOutbox(ctx context.Context, tx *sql.Tx, requestID string, now time.Time) error {
	eventID, err := newRequestID()
	if err != nil {
		return fmt.Errorf("generate purchase event ID: %w", err)
	}
	payload, err := purchaseEventPayload(requestID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_events
		(event_id, aggregate_type, aggregate_id, event_type, payload, status, available_at, created_at)
		VALUES (?, 'PURCHASE_REQUEST', ?, 'PURCHASE_ACCEPTED', ?, 'PENDING', ?, ?)`,
		eventID, requestID, payload, now, now); err != nil {
		return asyncDBError("write purchase outbox", err)
	}
	return nil
}

// Process returns committed SUCCEEDED/FAILED business results with nil error.
// Infrastructure failures and admission/generation fences leave the request
// pending and return an error. Consumers acknowledge only committed results.
func (s *AsyncService) Process(ctx context.Context, requestID string, now time.Time) (PurchaseRequest, error) {
	if !validRequestUUID(requestID) || !validFulfillmentTime(now) || now.UTC().Add(PaymentWindow).Year() > 9999 {
		return PurchaseRequest{}, ErrInvalidPurchaseInput
	}
	now = now.UTC().Truncate(time.Microsecond)
	var activityID uint64
	if err := s.db.QueryRowContext(ctx, `SELECT activity_id FROM purchase_requests WHERE request_id = ?`, requestID).Scan(&activityID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PurchaseRequest{}, ErrRequestNotFound
		}
		return PurchaseRequest{}, asyncDBError("locate purchase request", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return PurchaseRequest{}, asyncDBError("begin async purchase", err)
	}
	defer tx.Rollback()
	activity, err := lockAsyncActivity(ctx, tx, activityID)
	if err != nil {
		return PurchaseRequest{}, err
	}
	item, err := scanPurchaseRequest(tx.QueryRowContext(ctx, selectPurchaseRequestColumns+` WHERE request_id = ? FOR UPDATE`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return PurchaseRequest{}, ErrRequestNotFound
	}
	if err != nil {
		return PurchaseRequest{}, asyncDBError("lock purchase request", err)
	}
	if item.ActivityID != activityID {
		return PurchaseRequest{}, ErrStorageConflict
	}
	if item.Status.Terminal() {
		return item, nil
	}
	// Re-sample after both business locks are held. The caller's timestamp is
	// useful for early validation only; it must not let a lock waiter cross the
	// persisted processing deadline and still create an order.
	now = s.clock().UTC().Truncate(time.Microsecond)
	if !validFulfillmentTime(now) || now.UTC().Add(PaymentWindow).Year() > 9999 {
		return PurchaseRequest{}, ErrInvalidPurchaseInput
	}
	if activity.AdmissionState != "OPEN" {
		return failPurchaseRequest(ctx, tx, item, FailureActivityUnavailable)
	}
	if item.Generation != activity.Generation {
		return failPurchaseRequest(ctx, tx, item, FailureGenerationChanged)
	}
	// Check for an already committed matching order before expiration. Normal
	// creation updates both in one transaction; this also handles recovered data.
	existing, err := scanOrder(tx.QueryRowContext(ctx, selectOrderColumns+` WHERE request_id = ?`, item.RequestID))
	if err == nil {
		if !orderMatchesRequest(existing, item) {
			return PurchaseRequest{}, ErrStorageConflict
		}
		return succeedPurchaseRequest(ctx, tx, item, existing.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PurchaseRequest{}, asyncDBError("find request order", err)
	}
	if failure, validationErr := validateRequestProcessing(item, activity, now); validationErr != nil {
		return PurchaseRequest{}, validationErr
	} else if failure != "" {
		return failPurchaseRequest(ctx, tx, item, failure)
	}
	if conflict, err := existingUserOrderConflict(ctx, tx, item.UserID, item.ActivityID, item.IdempotencyKey); err != nil {
		return PurchaseRequest{}, err
	} else if conflict != "" {
		return failPurchaseRequest(ctx, tx, item, conflict)
	}
	if activity.AvailableStock == 0 {
		return failPurchaseRequest(ctx, tx, item, FailureSoldOut)
	}
	// Preserve activity/request locks but make the debit reversible if an order
	// unique key races with a synchronous purchase on a different activity.
	if _, err := tx.ExecContext(ctx, `SAVEPOINT async_order_creation`); err != nil {
		return PurchaseRequest{}, asyncDBError("save purchase creation boundary", err)
	}
	debit, err := tx.ExecContext(ctx, `UPDATE coupon_activities SET available_stock = available_stock - 1
		WHERE id = ? AND available_stock > 0 AND admission_state = 'OPEN' AND generation = ?`, item.ActivityID, item.Generation)
	if err != nil {
		return PurchaseRequest{}, asyncDBError("deduct async inventory", err)
	}
	if err := requireOneChanged(debit, ErrSoldOut); err != nil {
		if errors.Is(err, ErrSoldOut) {
			return failPurchaseRequest(ctx, tx, item, FailureSoldOut)
		}
		return PurchaseRequest{}, err
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO orders
		(request_id, idempotency_key, user_id, activity_id, shop_id, price_cents, currency,
		status, payment_deadline, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'PENDING_PAYMENT', ?, ?)`,
		item.RequestID, item.IdempotencyKey, item.UserID, item.ActivityID, item.ShopID,
		item.PriceCents, item.Currency, now.Add(PaymentWindow), now)
	if err != nil {
		if errors.Is(mapPurchaseDBError(err), ErrStorageConflict) {
			if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT async_order_creation`); rollbackErr != nil {
				return PurchaseRequest{}, asyncDBError("undo conflicting async debit", rollbackErr)
			}
			if conflict, readErr := existingUserOrderConflict(ctx, tx, item.UserID, item.ActivityID, item.IdempotencyKey); readErr != nil {
				return PurchaseRequest{}, readErr
			} else if conflict != "" {
				return failPurchaseRequest(ctx, tx, item, conflict)
			}
		}
		return PurchaseRequest{}, asyncDBError("insert async order", err)
	}
	orderID, err := insertedID(insert)
	if err != nil {
		return PurchaseRequest{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inventory_movements
		(order_id, activity_id, kind, delta, created_at) VALUES (?, ?, 'DEDUCT', -1, ?)`, orderID, item.ActivityID, now); err != nil {
		return PurchaseRequest{}, asyncDBError("record async deduction", err)
	}
	return succeedPurchaseRequest(ctx, tx, item, orderID)
}

func (s *AsyncService) GetOwnedRequest(ctx context.Context, userID uint64, requestID string) (PurchaseRequest, error) {
	if userID == 0 || !validRequestUUID(requestID) {
		return PurchaseRequest{}, ErrInvalidPurchaseInput
	}
	item, err := scanPurchaseRequest(s.db.QueryRowContext(ctx, selectPurchaseRequestColumns+` WHERE request_id = ? AND user_id = ?`, requestID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return PurchaseRequest{}, ErrRequestNotFound
	}
	if err != nil {
		return PurchaseRequest{}, asyncDBError("read owned purchase request", err)
	}
	return item, nil
}

func lockAsyncActivity(ctx context.Context, tx *sql.Tx, activityID uint64) (asyncActivity, error) {
	var activity asyncActivity
	err := tx.QueryRowContext(ctx, `SELECT id, shop_id, price_cents, currency, starts_at, ends_at,
		admission_state, available_stock, generation FROM coupon_activities WHERE id = ? FOR UPDATE`, activityID).
		Scan(&activity.ID, &activity.ShopID, &activity.PriceCents, &activity.Currency, &activity.StartsAt,
			&activity.EndsAt, &activity.AdmissionState, &activity.AvailableStock, &activity.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return asyncActivity{}, ErrActivityNotFound
	}
	if err != nil {
		return asyncActivity{}, asyncDBError("lock async activity", err)
	}
	return activity, nil
}

func existingUserOrderConflict(ctx context.Context, tx *sql.Tx, userID, activityID uint64, key string) (string, error) {
	var id uint64
	err := tx.QueryRowContext(ctx, `SELECT id FROM orders WHERE user_id = ? AND activity_id = ?`, userID, activityID).Scan(&id)
	if err == nil {
		return FailureAlreadyPurchased, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", asyncDBError("check async purchase eligibility", err)
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM orders WHERE user_id = ? AND idempotency_key = ?`, userID, key).Scan(&id)
	if err == nil {
		return FailureIdempotencyConflict, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", asyncDBError("check synchronous idempotency collision", err)
	}
	return "", nil
}

func failPurchaseRequest(ctx context.Context, tx *sql.Tx, item PurchaseRequest, code string) (PurchaseRequest, error) {
	update, err := tx.ExecContext(ctx, `UPDATE purchase_requests SET status = 'FAILED', error_code = ?, order_id = NULL
		WHERE id = ? AND status = ? AND order_id IS NULL`, code, item.ID, item.Status)
	if err != nil {
		return PurchaseRequest{}, asyncDBError("record purchase failure", err)
	}
	if err := requireOneChanged(update, ErrStorageConflict); err != nil {
		return PurchaseRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return PurchaseRequest{}, asyncDBError("commit purchase failure", err)
	}
	item.Status, item.ErrorCode, item.OrderID = RequestFailed, code, 0
	return item, nil
}

func succeedPurchaseRequest(ctx context.Context, tx *sql.Tx, item PurchaseRequest, orderID uint64) (PurchaseRequest, error) {
	update, err := tx.ExecContext(ctx, `UPDATE purchase_requests SET status = 'SUCCEEDED', order_id = ?, error_code = NULL
		WHERE id = ? AND status = ?`, orderID, item.ID, item.Status)
	if err != nil {
		return PurchaseRequest{}, asyncDBError("record purchase success", err)
	}
	if err := requireOneChanged(update, ErrStorageConflict); err != nil {
		return PurchaseRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return PurchaseRequest{}, asyncDBError("commit async purchase", err)
	}
	item.Status, item.OrderID, item.ErrorCode = RequestSucceeded, orderID, ""
	return item, nil
}

const selectPurchaseRequestColumns = `SELECT id, request_id, user_id, activity_id, scope, idempotency_key,
	params_digest, accepted_at, processing_deadline, shop_id, price_cents, currency, generation,
	status, order_id, error_code, reservation_state FROM purchase_requests`

func scanPurchaseRequest(row interface{ Scan(...any) error }) (PurchaseRequest, error) {
	var item PurchaseRequest
	var orderID sql.NullInt64
	var errorCode sql.NullString
	err := row.Scan(&item.ID, &item.RequestID, &item.UserID, &item.ActivityID, &item.Scope,
		&item.IdempotencyKey, &item.ParamsDigest, &item.AcceptedAt, &item.ProcessingDeadline,
		&item.ShopID, &item.PriceCents, &item.Currency, &item.Generation, &item.Status, &orderID, &errorCode, &item.ReservationState)
	if orderID.Valid && orderID.Int64 > 0 {
		item.OrderID = uint64(orderID.Int64)
	}
	if errorCode.Valid {
		item.ErrorCode = errorCode.String
	}
	return item, err
}

func purchaseParamsDigest(activityID uint64) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s\nactivity_id=%d", PurchaseScope, activityID)))
}

func matchAcceptedRequest(item PurchaseRequest, activityID uint64, digest []byte) (PurchaseRequest, error) {
	if item.ActivityID != activityID || item.Scope != PurchaseScope || !bytes.Equal(item.ParamsDigest, digest) {
		return PurchaseRequest{}, ErrIdempotencyConflict
	}
	return item, nil
}

func validateAsyncAcceptance(userID, activityID uint64, key string, now time.Time) error {
	if userID == 0 || activityID == 0 || !validFulfillmentToken(key, 128) ||
		!validFulfillmentTime(now) || now.UTC().Add(ProcessingWindow).Year() > 9999 {
		return ErrInvalidPurchaseInput
	}
	return nil
}

func validateAcceptedSnapshot(item PurchaseRequest, activity asyncActivity) string {
	if item.AcceptedAt.Before(activity.StartsAt) || !item.AcceptedAt.Before(activity.EndsAt) {
		return FailureInvalidAcceptedAt
	}
	if item.ActivityID != activity.ID || item.ShopID != activity.ShopID ||
		item.PriceCents != activity.PriceCents || item.Currency != activity.Currency {
		return FailureActivitySnapshotChanged
	}
	return ""
}

func validateRequestProcessing(item PurchaseRequest, activity asyncActivity, now time.Time) (string, error) {
	if !now.Before(item.ProcessingDeadline) {
		return FailureProcessingExpired, nil
	}
	if item.Status != RequestAdmitted {
		return "", ErrRequestNotAdmitted
	}
	return validateAcceptedSnapshot(item, activity), nil
}

func orderMatchesRequest(item Order, request PurchaseRequest) bool {
	return item.RequestID == request.RequestID && item.UserID == request.UserID &&
		item.ActivityID == request.ActivityID && item.ShopID == request.ShopID &&
		item.PriceCents == request.PriceCents && item.Currency == request.Currency &&
		item.IdempotencyKey == request.IdempotencyKey
}

func purchaseEventPayload(requestID string) ([]byte, error) {
	return json.Marshal(struct {
		RequestID string `json:"request_id"`
	}{RequestID: requestID})
}

func validRequestUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i := range len(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(value[i] >= '0' && value[i] <= '9') && !(value[i] >= 'a' && value[i] <= 'f') {
			return false
		}
	}
	return true
}

func asyncDBError(operation string, err error) error {
	return fmt.Errorf("%s: %w", operation, mapPurchaseDBError(err))
}
