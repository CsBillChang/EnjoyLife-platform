package order

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidFulfillmentInput = errors.New("invalid fulfillment input")
	ErrPaymentMismatch         = errors.New("payment amount or currency does not match order")
	ErrPaymentConflict         = errors.New("payment identity conflicts with a recorded transaction")
	ErrOrderStateConflict      = errors.New("order cannot perform this operation in its current state")
	ErrOrderNotExpired         = errors.New("order payment deadline has not elapsed")
	ErrVoucherNotFound         = errors.New("voucher not found")
	ErrVoucherUnavailable      = errors.New("voucher is not active or has expired")
	ErrMerchantForbidden       = errors.New("merchant does not own the voucher shop")
	ErrRefundNotFound          = errors.New("refund not found")
	ErrRefundConflict          = errors.New("refund identity or result conflicts with recorded facts")
	ErrInventoryInvariant      = errors.New("inventory release invariant failed")
)

const RefundPending = "PENDING"

type SuccessfulPayment struct {
	UserID        uint64
	OrderID       uint64
	AttemptID     string
	Provider      string
	TransactionID string
	AmountCents   uint64
	Currency      string
	Now           time.Time
}

type PaymentResult struct {
	Order       Order          `json:"order"`
	PaymentID   uint64         `json:"payment_id"`
	Outcome     PaymentOutcome `json:"outcome"`
	VoucherCode string         `json:"voucher_code,omitempty"`
	Refund      *Refund        `json:"refund,omitempty"`
}

type Refund struct {
	ID          uint64        `json:"id"`
	OrderID     uint64        `json:"order_id"`
	PaymentID   uint64        `json:"payment_id"`
	RequestID   string        `json:"request_id"`
	Purpose     RefundPurpose `json:"purpose"`
	AmountCents uint64        `json:"amount_cents"`
	Currency    string        `json:"currency"`
	Status      string        `json:"status"`
}

type FulfillmentService struct {
	db    *sql.DB
	clock func() time.Time
}

func NewFulfillmentService(db *sql.DB) *FulfillmentService {
	return &FulfillmentService{db: db, clock: time.Now}
}

type lockedFulfillment struct {
	tx                *sql.Tx
	order             Order
	admissionState    string
	generation        uint64
	voucherValidUntil time.Time
}

// lockOrder first discovers immutable routing information without locking, then
// locks activity -> order and rechecks the owner. userID == 0 is reserved for
// trusted workers (closure/refund results) and merchant redemption.
func (s *FulfillmentService) lockOrder(ctx context.Context, orderID, userID uint64) (*lockedFulfillment, error) {
	var activityID uint64
	query := `SELECT activity_id FROM orders WHERE id = ?`
	args := []any{orderID}
	if userID != 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&activityID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, fulfillmentDBError("locate order", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fulfillmentDBError("begin fulfillment", err)
	}
	locked := &lockedFulfillment{tx: tx}
	if err := tx.QueryRowContext(ctx, `SELECT admission_state, voucher_valid_until, generation
		FROM coupon_activities WHERE id = ? FOR UPDATE`, activityID).
		Scan(&locked.admissionState, &locked.voucherValidUntil, &locked.generation); err != nil {
		tx.Rollback()
		return nil, fulfillmentDBError("lock activity", err)
	}
	locked.order, err = scanOrder(tx.QueryRowContext(ctx, selectOrderColumns+` WHERE id = ? FOR UPDATE`, orderID))
	if err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, fulfillmentDBError("lock order", err)
	}
	if locked.order.ActivityID != activityID || (userID != 0 && locked.order.UserID != userID) {
		tx.Rollback()
		return nil, ErrOrderNotFound
	}
	return locked, nil
}

// ApplySuccessfulPayment accepts an already authenticated, verified successful
// provider result. It makes no external calls. The row-lock winner determines
// whether payment fulfills the order or compensates a previously closed order.
// Voucher plaintext is returned only for a newly committed voucher and cannot
// be recovered from its database hash on a replay.
func (s *FulfillmentService) ApplySuccessfulPayment(ctx context.Context, input SuccessfulPayment) (PaymentResult, error) {
	if err := validateSuccessfulPayment(input); err != nil {
		return PaymentResult{}, err
	}
	input.Now = input.Now.UTC().Truncate(time.Microsecond)
	locked, err := s.lockOrder(ctx, input.OrderID, input.UserID)
	if err != nil {
		return PaymentResult{}, err
	}
	defer locked.tx.Rollback()
	if err := validatePaymentAmount(locked.order, input); err != nil {
		return PaymentResult{}, err
	}

	previous, found, err := findRecordedPayment(ctx, locked.tx, input)
	if err != nil {
		return PaymentResult{}, err
	}
	if found {
		result := PaymentResult{Order: locked.order, PaymentID: previous.ID, Outcome: PaymentDuplicate}
		if previous.Settlement == PaymentLate || previous.Settlement == PaymentExtra {
			refund, err := scanRefund(locked.tx.QueryRowContext(ctx, selectRefundColumns+` WHERE refund_request_id = ?`, compensationRefundID(previous.ID)))
			if err != nil {
				return PaymentResult{}, fulfillmentDBError("read payment compensation", err)
			}
			result.Refund = &refund
		}
		return result, nil
	}
	decision, err := DecidePayment(locked.order.Status, false)
	if err != nil {
		return PaymentResult{}, err
	}
	insert, err := locked.tx.ExecContext(ctx, `INSERT INTO payments
		(order_id, attempt_id, provider, provider_transaction_id, amount_cents, currency, status, settlement, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'SUCCEEDED', ?, ?)`, input.OrderID, input.AttemptID, input.Provider,
		input.TransactionID, input.AmountCents, input.Currency, decision.Outcome, input.Now)
	if err != nil {
		return PaymentResult{}, paymentDBError(err)
	}
	paymentID, err := insertedID(insert)
	if err != nil {
		return PaymentResult{}, err
	}
	result := PaymentResult{Order: locked.order, PaymentID: paymentID, Outcome: decision.Outcome}
	if decision.IssueVoucher {
		code, hash, err := newVoucherCode()
		if err != nil {
			return PaymentResult{}, fmt.Errorf("generate voucher: %w", err)
		}
		if err := transitionLockedOrder(ctx, locked.tx, input.OrderID, locked.order.Status, decision.NextState); err != nil {
			return PaymentResult{}, err
		}
		if _, err := locked.tx.ExecContext(ctx, `INSERT INTO vouchers
			(order_id, code_hash, status, expires_at, created_at) VALUES (?, ?, 'ACTIVE', ?, ?)`,
			input.OrderID, hash[:], locked.voucherValidUntil, input.Now); err != nil {
			return PaymentResult{}, fulfillmentDBError("create voucher", err)
		}
		result.Order.Status = decision.NextState
		result.VoucherCode = code
	} else {
		refund := Refund{OrderID: input.OrderID, PaymentID: paymentID,
			RequestID: compensationRefundID(paymentID), Purpose: decision.RefundPurpose,
			AmountCents: input.AmountCents, Currency: input.Currency, Status: RefundPending}
		if err := insertRefund(ctx, locked.tx, &refund, input.Now); err != nil {
			return PaymentResult{}, err
		}
		result.Refund = &refund
	}
	if err := locked.tx.Commit(); err != nil {
		return PaymentResult{}, fulfillmentDBError("commit payment", err)
	}
	return result, nil
}

type recordedPayment struct {
	ID            uint64
	OrderID       uint64
	AttemptID     string
	Provider      string
	TransactionID string
	AmountCents   uint64
	Currency      string
	Settlement    PaymentOutcome
}

func findRecordedPayment(ctx context.Context, tx *sql.Tx, input SuccessfulPayment) (recordedPayment, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, order_id, attempt_id, provider,
		provider_transaction_id, amount_cents, currency, settlement FROM payments
		WHERE (provider = ? AND provider_transaction_id = ?) OR attempt_id = ?`, input.Provider, input.TransactionID, input.AttemptID)
	if err != nil {
		return recordedPayment{}, false, fulfillmentDBError("find recorded payment", err)
	}
	defer rows.Close()
	var found recordedPayment
	count := 0
	for rows.Next() {
		if err := rows.Scan(&found.ID, &found.OrderID, &found.AttemptID, &found.Provider,
			&found.TransactionID, &found.AmountCents, &found.Currency, &found.Settlement); err != nil {
			return recordedPayment{}, false, fulfillmentDBError("scan recorded payment", err)
		}
		count++
		if count > 1 || !paymentMatches(found, input) {
			return recordedPayment{}, false, ErrPaymentConflict
		}
	}
	if err := rows.Err(); err != nil {
		return recordedPayment{}, false, fulfillmentDBError("iterate recorded payment", err)
	}
	return found, count == 1, nil
}

// CloseExpired is a trusted worker operation. CLOSED replays are no-ops;
// other non-pending states cannot be closed. Inventory mutations pause while
// admission_state is not OPEN, so rebuilds have a stable inventory snapshot.
func (s *FulfillmentService) CloseExpired(ctx context.Context, orderID uint64, now time.Time) (Order, error) {
	if orderID == 0 || !validFulfillmentTime(now) {
		return Order{}, ErrInvalidFulfillmentInput
	}
	locked, err := s.lockOrder(ctx, orderID, 0)
	if err != nil {
		return Order{}, err
	}
	defer locked.tx.Rollback()
	if locked.order.Status == Closed {
		return locked.order, nil
	}
	if locked.order.Status != PendingPayment {
		return Order{}, ErrOrderStateConflict
	}
	if now.Before(locked.order.PaymentDeadline) {
		return Order{}, ErrOrderNotExpired
	}
	if err := releaseLockedInventory(ctx, locked, now); err != nil {
		return Order{}, err
	}
	if err := transitionLockedOrder(ctx, locked.tx, orderID, PendingPayment, Closed); err != nil {
		return Order{}, err
	}
	if err := locked.tx.Commit(); err != nil {
		return Order{}, fulfillmentDBError("commit closure", err)
	}
	locked.order.Status = Closed
	return locked.order, nil
}

// Redeem authenticates shop membership again inside the transaction, in addition
// to any HTTP role check. A refund-in-progress blocks redemption via order state.
func (s *FulfillmentService) Redeem(ctx context.Context, merchantID uint64, code string, now time.Time) (Order, error) {
	if merchantID == 0 || !validVoucherCode(code) || !validFulfillmentTime(now) {
		return Order{}, ErrInvalidFulfillmentInput
	}
	hash := sha256.Sum256([]byte(code))
	var orderID uint64
	if err := s.db.QueryRowContext(ctx, `SELECT order_id FROM vouchers WHERE code_hash = ?`, hash[:]).Scan(&orderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Order{}, ErrVoucherNotFound
		}
		return Order{}, fulfillmentDBError("locate voucher", err)
	}
	locked, err := s.lockOrder(ctx, orderID, 0)
	if err != nil {
		return Order{}, err
	}
	defer locked.tx.Rollback()
	var voucherID uint64
	var voucherStatus string
	var expires time.Time
	err = locked.tx.QueryRowContext(ctx, `SELECT id, status, expires_at FROM vouchers
		WHERE code_hash = ? AND order_id = ? FOR UPDATE`, hash[:], orderID).Scan(&voucherID, &voucherStatus, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, ErrVoucherNotFound
	}
	if err != nil {
		return Order{}, fulfillmentDBError("lock voucher", err)
	}
	var authorizedID uint64
	err = locked.tx.QueryRowContext(ctx, `SELECT m.user_id FROM merchant_shop_members m
		JOIN users u ON u.id = m.user_id WHERE m.user_id = ? AND m.shop_id = ?
		AND u.role = 'MERCHANT' FOR SHARE`, merchantID, locked.order.ShopID).Scan(&authorizedID)
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, ErrMerchantForbidden
	}
	if err != nil {
		return Order{}, fulfillmentDBError("authorize voucher merchant", err)
	}
	// Waiting for activity/order/voucher or authorization locks must not let a
	// voucher cross its expiration boundary with a stale request timestamp.
	now = s.clock().UTC().Truncate(time.Microsecond)
	if !validFulfillmentTime(now) {
		return Order{}, ErrInvalidFulfillmentInput
	}
	if err := validateRedemption(locked.order.Status, voucherStatus, expires, now); err != nil {
		return Order{}, err
	}
	if err := transitionLockedOrder(ctx, locked.tx, orderID, Paid, Redeemed); err != nil {
		return Order{}, err
	}
	update, err := locked.tx.ExecContext(ctx, `UPDATE vouchers SET status = 'REDEEMED' WHERE id = ? AND status = 'ACTIVE'`, voucherID)
	if err != nil {
		return Order{}, fulfillmentDBError("redeem voucher", err)
	}
	if err := requireOneChanged(update, ErrVoucherUnavailable); err != nil {
		return Order{}, err
	}
	if _, err := locked.tx.ExecContext(ctx, `INSERT INTO redemptions
		(voucher_id, order_id, merchant_user_id, redeemed_at) VALUES (?, ?, ?, ?)`,
		voucherID, orderID, merchantID, now.UTC().Truncate(time.Microsecond)); err != nil {
		return Order{}, fulfillmentDBError("record redemption", err)
	}
	if err := locked.tx.Commit(); err != nil {
		return Order{}, fulfillmentDBError("commit redemption", err)
	}
	locked.order.Status = Redeemed
	return locked.order, nil
}

// RequestNormalRefund commits the frozen order and stable refund identity before
// a simulator/provider is called. Reusing a key for another order conflicts;
// reusing the same key returns its recorded result, including terminal failure.
func (s *FulfillmentService) RequestNormalRefund(ctx context.Context, userID, orderID uint64, key string, now time.Time) (Refund, error) {
	if userID == 0 || orderID == 0 || ValidateOperationKey(key) != nil || !validFulfillmentTime(now) {
		return Refund{}, ErrInvalidFulfillmentInput
	}
	locked, err := s.lockOrder(ctx, orderID, userID)
	if err != nil {
		return Refund{}, err
	}
	defer locked.tx.Rollback()
	requestID := normalRefundID(userID, key)
	previous, err := scanRefund(locked.tx.QueryRowContext(ctx, selectRefundColumns+` WHERE refund_request_id = ?`, requestID))
	if err == nil {
		if previous.OrderID != orderID || previous.Purpose != NormalRefund {
			return Refund{}, ErrRefundConflict
		}
		return previous, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Refund{}, fulfillmentDBError("read refund replay", err)
	}
	if locked.order.Status != Paid {
		return Refund{}, ErrOrderStateConflict
	}
	var paymentID uint64
	if err := locked.tx.QueryRowContext(ctx, `SELECT id FROM payments WHERE order_id = ?
		AND settlement = 'APPLIED' AND status = 'SUCCEEDED' ORDER BY id LIMIT 1`, orderID).Scan(&paymentID); err != nil {
		return Refund{}, fulfillmentDBError("find refundable payment", err)
	}
	refund := Refund{OrderID: orderID, PaymentID: paymentID, RequestID: requestID,
		Purpose: NormalRefund, AmountCents: locked.order.PriceCents, Currency: locked.order.Currency, Status: RefundPending}
	if err := insertRefund(ctx, locked.tx, &refund, now); err != nil {
		return Refund{}, err
	}
	if err := transitionLockedOrder(ctx, locked.tx, orderID, Paid, Refunding); err != nil {
		return Refund{}, err
	}
	if err := locked.tx.Commit(); err != nil {
		return Refund{}, fulfillmentDBError("commit refund request", err)
	}
	return refund, nil
}

// ApplyRefundResult is a trusted adapter/worker boundary: authentication and
// provider amount/identity verification precede this call. Unknown results never
// unfreeze an order. A terminal result cannot be reversed by an older callback.
func (s *FulfillmentService) ApplyRefundResult(ctx context.Context, requestID string, result RefundResult, now time.Time) (Refund, error) {
	if !validFulfillmentToken(requestID, 128) || !validFulfillmentTime(now) || !validRefundResult(result) {
		return Refund{}, ErrInvalidFulfillmentInput
	}
	var orderID uint64
	if err := s.db.QueryRowContext(ctx, `SELECT order_id FROM refunds WHERE refund_request_id = ?`, requestID).Scan(&orderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Refund{}, ErrRefundNotFound
		}
		return Refund{}, fulfillmentDBError("locate refund", err)
	}
	locked, err := s.lockOrder(ctx, orderID, 0)
	if err != nil {
		return Refund{}, err
	}
	defer locked.tx.Rollback()
	refund, err := scanRefund(locked.tx.QueryRowContext(ctx, selectRefundColumns+` WHERE refund_request_id = ? AND order_id = ? FOR UPDATE`, requestID, orderID))
	if err != nil {
		return Refund{}, fulfillmentDBError("lock refund", err)
	}
	apply, err := shouldApplyRefundResult(refund.Status, result)
	if err != nil {
		return Refund{}, err
	}
	if !apply {
		return refund, nil
	}
	decision, err := DecideRefund(locked.order.Status, refund.Purpose, result)
	if err != nil {
		return Refund{}, err
	}
	if refund.Purpose == NormalRefund {
		// Prevent a stale normal refund operation from changing a newer attempt.
		var currentRefundID uint64
		if err := locked.tx.QueryRowContext(ctx, `SELECT id FROM refunds WHERE order_id = ?
			AND purpose = 'NORMAL' ORDER BY id DESC LIMIT 1`, orderID).Scan(&currentRefundID); err != nil {
			return Refund{}, fulfillmentDBError("find current normal refund", err)
		}
		if currentRefundID != refund.ID {
			return Refund{}, ErrRefundConflict
		}
		if decision.ReleaseInventory {
			if err := releaseLockedInventory(ctx, locked, now); err != nil {
				return Refund{}, err
			}
			update, err := locked.tx.ExecContext(ctx, `UPDATE vouchers SET status = 'INVALID'
				WHERE order_id = ? AND status = 'ACTIVE'`, orderID)
			if err != nil {
				return Refund{}, fulfillmentDBError("invalidate refunded voucher", err)
			}
			if err := requireOneChanged(update, ErrVoucherUnavailable); err != nil {
				return Refund{}, err
			}
		}
		if decision.NextState != locked.order.Status {
			if err := transitionLockedOrder(ctx, locked.tx, orderID, locked.order.Status, decision.NextState); err != nil {
				return Refund{}, err
			}
		}
	}
	update, err := locked.tx.ExecContext(ctx, `UPDATE refunds SET status = ? WHERE id = ? AND status = ?`, string(result), refund.ID, refund.Status)
	if err != nil {
		return Refund{}, fulfillmentDBError("record refund result", err)
	}
	if err := requireOneChanged(update, ErrRefundConflict); err != nil {
		return Refund{}, err
	}
	if err := locked.tx.Commit(); err != nil {
		return Refund{}, fulfillmentDBError("commit refund result", err)
	}
	refund.Status = string(result)
	return refund, nil
}

func releaseLockedInventory(ctx context.Context, locked *lockedFulfillment, now time.Time) error {
	if locked.admissionState != "OPEN" {
		return ErrActivityUnavailable
	}
	// Require the matching committed deduction, then rely on (order_id, kind)
	// uniqueness. A duplicate RELEASE is an invariant error, never a second +1.
	insert, err := locked.tx.ExecContext(ctx, `INSERT INTO inventory_movements
		(order_id, activity_id, kind, delta, created_at)
		SELECT ?, ?, 'RELEASE', 1, ? FROM inventory_movements
		WHERE order_id = ? AND activity_id = ? AND kind = 'DEDUCT'`,
		locked.order.ID, locked.order.ActivityID, now.UTC().Truncate(time.Microsecond), locked.order.ID, locked.order.ActivityID)
	if err != nil {
		mapped := mapPurchaseDBError(err)
		if errors.Is(mapped, ErrStorageConflict) {
			return fmt.Errorf("%w: %w", ErrInventoryInvariant, err)
		}
		return fulfillmentDBError("record inventory release", err)
	}
	if err := requireOneChanged(insert, ErrInventoryInvariant); err != nil {
		return err
	}
	update, err := locked.tx.ExecContext(ctx, `UPDATE coupon_activities
		SET available_stock = available_stock + 1
		WHERE id = ? AND admission_state = 'OPEN' AND available_stock < total_stock`, locked.order.ActivityID)
	if err != nil {
		return fulfillmentDBError("release inventory", err)
	}
	if err := requireOneChanged(update, ErrInventoryInvariant); err != nil {
		return err
	}
	return enqueueStockRestore(ctx, locked)
}

func transitionLockedOrder(ctx context.Context, tx *sql.Tx, orderID uint64, from, to State) error {
	if err := ValidateTransition(from, to); err != nil {
		return err
	}
	update, err := tx.ExecContext(ctx, `UPDATE orders SET status = ? WHERE id = ? AND status = ?`, to, orderID, from)
	if err != nil {
		return fulfillmentDBError("change order state", err)
	}
	return requireOneChanged(update, ErrOrderStateConflict)
}

const selectRefundColumns = `SELECT id, order_id, payment_id, refund_request_id, purpose,
	amount_cents, currency, status FROM refunds`

func scanRefund(row interface{ Scan(...any) error }) (Refund, error) {
	var refund Refund
	var paymentID sql.NullInt64
	err := row.Scan(&refund.ID, &refund.OrderID, &paymentID, &refund.RequestID,
		&refund.Purpose, &refund.AmountCents, &refund.Currency, &refund.Status)
	if paymentID.Valid && paymentID.Int64 > 0 {
		refund.PaymentID = uint64(paymentID.Int64)
	}
	return refund, err
}

func insertRefund(ctx context.Context, tx *sql.Tx, refund *Refund, now time.Time) error {
	insert, err := tx.ExecContext(ctx, `INSERT INTO refunds
		(order_id, payment_id, refund_request_id, purpose, amount_cents, currency, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'PENDING', ?)`, refund.OrderID, refund.PaymentID, refund.RequestID,
		refund.Purpose, refund.AmountCents, refund.Currency, now.UTC().Truncate(time.Microsecond))
	if err != nil {
		if errors.Is(mapPurchaseDBError(err), ErrStorageConflict) {
			return fmt.Errorf("%w: %w", ErrRefundConflict, err)
		}
		return fulfillmentDBError("create refund", err)
	}
	refund.ID, err = insertedID(insert)
	return err
}

func requireOneChanged(result sql.Result, conflict error) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read mutation result: %w", err)
	}
	if count != 1 {
		return conflict
	}
	return nil
}

func insertedID(result sql.Result) (uint64, error) {
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read inserted ID: %w", err)
	}
	if id <= 0 {
		return 0, ErrStorageConflict
	}
	return uint64(id), nil
}

func fulfillmentDBError(operation string, err error) error {
	return fmt.Errorf("%s: %w", operation, mapPurchaseDBError(err))
}

func paymentDBError(err error) error {
	if errors.Is(mapPurchaseDBError(err), ErrStorageConflict) {
		return fmt.Errorf("%w: %w", ErrPaymentConflict, err)
	}
	return fulfillmentDBError("record successful payment", err)
}

func validFulfillmentTime(now time.Time) bool {
	year := now.UTC().Year()
	return year >= 1000 && year <= 9999
}

func validFulfillmentToken(value string, max int) bool {
	if len(value) == 0 || len(value) > max {
		return false
	}
	for i := range len(value) {
		if value[i] < 33 || value[i] > 126 {
			return false
		}
	}
	return true
}

// ValidateOperationKey is the shared HTTP/domain rule for payment and refund
// identities. Validate before any provider operation; do not trim or normalize.
func ValidateOperationKey(key string) error {
	if !validFulfillmentToken(key, 128) {
		return ErrInvalidFulfillmentInput
	}
	return nil
}

func validateSuccessfulPayment(input SuccessfulPayment) error {
	if input.UserID == 0 || input.OrderID == 0 || !validFulfillmentTime(input.Now) ||
		ValidateOperationKey(input.AttemptID) != nil || !validFulfillmentToken(input.Provider, 32) ||
		!validFulfillmentToken(input.TransactionID, 128) || len(input.Currency) != 3 {
		return ErrInvalidFulfillmentInput
	}
	for i := range len(input.Currency) {
		if input.Currency[i] < 'A' || input.Currency[i] > 'Z' {
			return ErrInvalidFulfillmentInput
		}
	}
	return nil
}

func validatePaymentAmount(item Order, input SuccessfulPayment) error {
	if item.PriceCents != input.AmountCents || item.Currency != input.Currency {
		return ErrPaymentMismatch
	}
	return nil
}

func paymentMatches(previous recordedPayment, input SuccessfulPayment) bool {
	return previous.OrderID == input.OrderID && previous.AttemptID == input.AttemptID &&
		previous.Provider == input.Provider && previous.TransactionID == input.TransactionID &&
		previous.AmountCents == input.AmountCents && previous.Currency == input.Currency
}

func validateRedemption(state State, voucherStatus string, expires, now time.Time) error {
	if state != Paid {
		return ErrOrderStateConflict
	}
	if voucherStatus != "ACTIVE" || !now.Before(expires) {
		return ErrVoucherUnavailable
	}
	return nil
}

func validRefundResult(result RefundResult) bool {
	return result == RefundUnknown || result == RefundSucceeded || result == RefundFailedFinal
}

func shouldApplyRefundResult(current string, result RefundResult) (bool, error) {
	if !validRefundResult(result) {
		return false, ErrInvalidFulfillmentInput
	}
	switch current {
	case RefundPending:
		return true, nil
	case string(RefundUnknown):
		return result != RefundUnknown, nil
	case string(RefundSucceeded), string(RefundFailedFinal):
		if result == RefundUnknown || string(result) == current {
			return false, nil
		}
		return false, ErrRefundConflict
	default:
		return false, ErrRefundConflict
	}
}

func normalRefundID(userID uint64, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", userID, key)))
	return "normal-" + hex.EncodeToString(sum[:])
}

func compensationRefundID(paymentID uint64) string {
	return fmt.Sprintf("comp-%d", paymentID)
}

func newVoucherCode() (string, [32]byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", [32]byte{}, err
	}
	code := hex.EncodeToString(random[:])
	return code, sha256.Sum256([]byte(code)), nil
}

func validVoucherCode(code string) bool {
	if len(code) != 64 {
		return false
	}
	for i := range len(code) {
		if !(code[i] >= '0' && code[i] <= '9') && !(code[i] >= 'a' && code[i] <= 'f') {
			return false
		}
	}
	return true
}
