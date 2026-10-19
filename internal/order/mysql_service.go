package order

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	ErrInvalidPurchaseInput = errors.New("invalid purchase input")
	ErrActivityNotFound     = errors.New("activity not found")
	ErrActivityUnavailable  = errors.New("activity admission is unavailable")
	ErrActivityNotStarted   = errors.New("activity has not started")
	ErrActivityEnded        = errors.New("activity has ended")
	ErrSoldOut              = errors.New("activity is sold out")
	ErrAlreadyPurchased     = errors.New("user has already purchased this activity")
	ErrIdempotencyConflict  = errors.New("idempotency key belongs to another activity")
	ErrOrderNotFound        = errors.New("order not found")
	ErrStorageConflict      = errors.New("purchase storage constraint conflict")
	ErrTransactionRetryable = errors.New("purchase transaction can be retried")
)

const PaymentWindow = 15 * time.Minute

type Order struct {
	ID              uint64    `json:"id"`
	RequestID       string    `json:"request_id"`
	IdempotencyKey  string    `json:"-"`
	UserID          uint64    `json:"user_id"`
	ActivityID      uint64    `json:"activity_id"`
	ShopID          uint64    `json:"shop_id"`
	PriceCents      uint64    `json:"price_cents"`
	Currency        string    `json:"currency"`
	Status          State     `json:"status"`
	PaymentDeadline time.Time `json:"payment_deadline"`
	CreatedAt       time.Time `json:"created_at"`
}

type MySQLService struct {
	db    *sql.DB
	clock func() time.Time
}

func NewMySQLService(db *sql.DB) *MySQLService {
	return &MySQLService{db: db, clock: time.Now}
}

type purchaseActivity struct {
	ID             uint64
	ShopID         uint64
	PriceCents     uint64
	Currency       string
	StartsAt       time.Time
	EndsAt         time.Time
	AdmissionState string
	AvailableStock uint64
}

// Purchase creates an order and its inventory deduction atomically. Existing
// orders consume eligibility permanently, including CLOSED and REFUNDED orders.
// now is a trusted server timestamp, never an HTTP request field. A successful
// commit whose response is lost is recovered using the same idempotency key.
func (s *MySQLService) Purchase(ctx context.Context, userID, activityID uint64, idempotencyKey string, now time.Time) (Order, error) {
	if err := validatePurchaseInput(userID, activityID, idempotencyKey, now); err != nil {
		return Order{}, err
	}
	now = now.UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Order{}, fmt.Errorf("begin purchase: %w", mapPurchaseDBError(err))
	}
	defer tx.Rollback()

	var activity purchaseActivity
	err = tx.QueryRowContext(ctx, `SELECT id, shop_id, price_cents, currency,
		starts_at, ends_at, admission_state, available_stock
		FROM coupon_activities WHERE id = ? FOR UPDATE`, activityID).
		Scan(&activity.ID, &activity.ShopID, &activity.PriceCents, &activity.Currency,
			&activity.StartsAt, &activity.EndsAt, &activity.AdmissionState, &activity.AvailableStock)
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, ErrActivityNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("lock purchase activity: %w", mapPurchaseDBError(err))
	}

	// Replay precedes admission checks: an already committed order remains
	// retrievable when the activity is frozen, ended, or out of stock.
	existing, err := scanOrder(tx.QueryRowContext(ctx, selectOrderColumns+
		` WHERE user_id = ? AND idempotency_key = ?`, userID, idempotencyKey))
	if err == nil {
		if existing.ActivityID != activityID {
			return Order{}, ErrIdempotencyConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Order{}, fmt.Errorf("find purchase replay: %w", mapPurchaseDBError(err))
	}
	var previousID uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM orders WHERE user_id = ? AND activity_id = ?`, userID, activityID).Scan(&previousID)
	if err == nil {
		return Order{}, ErrAlreadyPurchased
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Order{}, fmt.Errorf("check purchase eligibility: %w", mapPurchaseDBError(err))
	}
	// The activity lock may have blocked past the sale boundary. Only the
	// timestamp sampled while owning that lock can authorize a new purchase.
	now = s.clock().UTC().Truncate(time.Microsecond)
	if err := validatePurchaseInput(userID, activityID, idempotencyKey, now); err != nil {
		return Order{}, err
	}
	if err := validatePurchaseActivity(activity, now); err != nil {
		return Order{}, err
	}
	requestID, err := newRequestID()
	if err != nil {
		return Order{}, fmt.Errorf("generate purchase request ID: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE coupon_activities
		SET available_stock = available_stock - 1
		WHERE id = ? AND admission_state = 'OPEN' AND available_stock > 0`, activityID)
	if err != nil {
		return Order{}, fmt.Errorf("deduct purchase inventory: %w", mapPurchaseDBError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Order{}, fmt.Errorf("read purchase deduction result: %w", err)
	}
	if affected != 1 {
		return Order{}, ErrSoldOut
	}
	item := Order{
		RequestID: requestID, IdempotencyKey: idempotencyKey, UserID: userID,
		ActivityID: activityID, ShopID: activity.ShopID, PriceCents: activity.PriceCents,
		Currency: activity.Currency, Status: PendingPayment,
		PaymentDeadline: now.Add(PaymentWindow), CreatedAt: now,
	}
	result, err = tx.ExecContext(ctx, `INSERT INTO orders
		(request_id, idempotency_key, user_id, activity_id, shop_id, price_cents,
		currency, status, payment_deadline, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.RequestID, item.IdempotencyKey, item.UserID, item.ActivityID, item.ShopID,
		item.PriceCents, item.Currency, item.Status, item.PaymentDeadline, item.CreatedAt)
	if err != nil {
		mapped := mapPurchaseDBError(err)
		if errors.Is(mapped, ErrStorageConflict) {
			// A different activity can race on the same user's idempotency key.
			// Roll back our deduction before consulting the committed winner.
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return Order{}, fmt.Errorf("rollback conflicting purchase: %w", rollbackErr)
			}
			return s.resolvePurchaseConflict(ctx, userID, activityID, idempotencyKey, mapped)
		}
		return Order{}, fmt.Errorf("insert purchase order: %w", mapped)
	}
	id, err := result.LastInsertId()
	if err != nil || id <= 0 {
		if err == nil {
			err = ErrStorageConflict
		}
		return Order{}, fmt.Errorf("read new order ID: %w", err)
	}
	item.ID = uint64(id)
	if _, err := tx.ExecContext(ctx, `INSERT INTO inventory_movements
		(order_id, activity_id, kind, delta) VALUES (?, ?, 'DEDUCT', -1)`, item.ID, activityID); err != nil {
		return Order{}, fmt.Errorf("record purchase deduction: %w", mapPurchaseDBError(err))
	}
	if err := tx.Commit(); err != nil {
		return Order{}, fmt.Errorf("commit purchase: %w", mapPurchaseDBError(err))
	}
	return item, nil
}

func (s *MySQLService) GetOwnedOrder(ctx context.Context, userID, orderID uint64) (Order, error) {
	if userID == 0 || orderID == 0 {
		return Order{}, ErrInvalidPurchaseInput
	}
	item, err := scanOrder(s.db.QueryRowContext(ctx, selectOrderColumns+` WHERE id = ? AND user_id = ?`, orderID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, ErrOrderNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("read owned order: %w", mapPurchaseDBError(err))
	}
	return item, nil
}

const selectOrderColumns = `SELECT id, request_id, idempotency_key, user_id, activity_id,
	shop_id, price_cents, currency, status, payment_deadline, created_at FROM orders`

func scanOrder(row interface{ Scan(...any) error }) (Order, error) {
	var item Order
	err := row.Scan(&item.ID, &item.RequestID, &item.IdempotencyKey, &item.UserID,
		&item.ActivityID, &item.ShopID, &item.PriceCents, &item.Currency,
		&item.Status, &item.PaymentDeadline, &item.CreatedAt)
	return item, err
}

func (s *MySQLService) resolvePurchaseConflict(ctx context.Context, userID, activityID uint64, key string, cause error) (Order, error) {
	existing, err := scanOrder(s.db.QueryRowContext(ctx, selectOrderColumns+` WHERE user_id = ? AND idempotency_key = ?`, userID, key))
	if err == nil {
		if existing.ActivityID != activityID {
			return Order{}, ErrIdempotencyConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Order{}, fmt.Errorf("resolve purchase replay conflict: %w", mapPurchaseDBError(err))
	}
	var id uint64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM orders WHERE user_id = ? AND activity_id = ?`, userID, activityID).Scan(&id)
	if err == nil {
		return Order{}, ErrAlreadyPurchased
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Order{}, fmt.Errorf("resolve purchase eligibility conflict: %w", mapPurchaseDBError(err))
	}
	return Order{}, cause
}

func validatePurchaseInput(userID, activityID uint64, key string, now time.Time) error {
	now = now.UTC()
	if userID == 0 || activityID == 0 || len(key) == 0 || len(key) > 128 || now.Year() < 1000 || now.Add(PaymentWindow).Year() > 9999 {
		return ErrInvalidPurchaseInput
	}
	// Reject whitespace and non-ASCII bytes instead of silently normalizing keys.
	// In particular, trailing spaces must not alias under MySQL string comparison.
	for i := 0; i < len(key); i++ {
		if key[i] < 33 || key[i] > 126 {
			return ErrInvalidPurchaseInput
		}
	}
	return nil
}

func validatePurchaseActivity(activity purchaseActivity, now time.Time) error {
	if activity.AdmissionState != "OPEN" {
		return ErrActivityUnavailable
	}
	if now.Before(activity.StartsAt) {
		return ErrActivityNotStarted
	}
	if !now.Before(activity.EndsAt) {
		return ErrActivityEnded
	}
	if activity.AvailableStock == 0 {
		return ErrSoldOut
	}
	return nil
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func mapPurchaseDBError(err error) error {
	var dbErr *mysql.MySQLError
	if errors.As(err, &dbErr) {
		switch dbErr.Number {
		case 1062:
			return fmt.Errorf("%w: %w", ErrStorageConflict, err)
		case 1205, 1213:
			return fmt.Errorf("%w: %w", ErrTransactionRetryable, err)
		}
	}
	return err
}
