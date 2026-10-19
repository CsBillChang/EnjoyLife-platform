package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	DefaultRecordPageSize = 20
	MaxRecordPageSize     = 100
)

var (
	ErrInvalidRecordPage    = errors.New("invalid record page")
	ErrMerchantShopNotFound = errors.New("merchant shop not found")
)

// RecordPage is the validated, decoded pagination input. Its cursor is kept
// unexported so callers cannot accidentally build an SQL cursor from request
// text without validation.
type RecordPage struct {
	Limit  int
	cursor *recordCursor
}

type recordCursor struct {
	At time.Time
	ID uint64
}

// PaymentRecord is deliberately a display-safe projection. In particular, it
// excludes provider_transaction_id: that identifier is only needed by the
// local-provider and reconciliation boundaries.
type PaymentRecord struct {
	PaymentID   uint64    `json:"payment_id"`
	OrderID     uint64    `json:"order_id"`
	ShopID      uint64    `json:"shop_id"`
	AmountCents uint64    `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	Settlement  string    `json:"settlement"`
	CreatedAt   time.Time `json:"created_at"`
}

// RedemptionRecord similarly never includes a voucher plaintext or hash.
type RedemptionRecord struct {
	RedemptionID   uint64    `json:"redemption_id"`
	OrderID        uint64    `json:"order_id"`
	MerchantUserID uint64    `json:"merchant_user_id"`
	RedeemedAt     time.Time `json:"redeemed_at"`
}

type PaymentRecordPage struct {
	Items      []PaymentRecord `json:"items"`
	NextCursor string          `json:"next_cursor"`
}

type RedemptionRecordPage struct {
	Items      []RedemptionRecord `json:"items"`
	NextCursor string             `json:"next_cursor"`
}

// RecordQueryService owns only read projections over transaction facts. Write
// paths and the local payment-provider boundary intentionally remain elsewhere.
type RecordQueryService struct {
	db *sql.DB
}

func NewRecordQueryService(db *sql.DB) *RecordQueryService {
	return &RecordQueryService{db: db}
}

func (s *RecordQueryService) ListPaymentRecords(ctx context.Context, userID uint64, page RecordPage) (PaymentRecordPage, error) {
	if userID == 0 || !validRecordPage(page) {
		return PaymentRecordPage{}, ErrInvalidRecordPage
	}
	query := `SELECT p.id, p.order_id, o.shop_id, p.amount_cents, p.currency, p.status, p.settlement, p.created_at
		FROM payments p JOIN orders o ON o.id = p.order_id
		WHERE o.user_id = ?`
	args := []any{userID}
	if page.cursor != nil {
		query += ` AND (p.created_at < ? OR (p.created_at = ? AND p.id < ?))`
		args = append(args, page.cursor.At, page.cursor.At, page.cursor.ID)
	}
	query += ` ORDER BY p.created_at DESC, p.id DESC LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return PaymentRecordPage{}, fmt.Errorf("list payment records: %w", err)
	}
	defer rows.Close()
	items := make([]PaymentRecord, 0, page.Limit)
	for rows.Next() {
		var item PaymentRecord
		if err := rows.Scan(&item.PaymentID, &item.OrderID, &item.ShopID, &item.AmountCents,
			&item.Currency, &item.Status, &item.Settlement, &item.CreatedAt); err != nil {
			return PaymentRecordPage{}, fmt.Errorf("scan payment record: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return PaymentRecordPage{}, fmt.Errorf("iterate payment records: %w", err)
	}
	return paymentPage(items, page.Limit), nil
}

func (s *RecordQueryService) ListMerchantRedemptionRecords(ctx context.Context, merchantID, shopID uint64, page RecordPage) (RedemptionRecordPage, error) {
	if merchantID == 0 || shopID == 0 || !validRecordPage(page) {
		return RedemptionRecordPage{}, ErrInvalidRecordPage
	}
	// Keep the membership test explicit so a caller cannot distinguish a shop
	// that does not exist from one it is not allowed to inspect. The listing SQL
	// repeats the membership join to remain safe if membership changes after this
	// check but before the record query.
	var member uint64
	err := s.db.QueryRowContext(ctx, `SELECT shop_id FROM merchant_shop_members
		WHERE user_id = ? AND shop_id = ?`, merchantID, shopID).Scan(&member)
	if errors.Is(err, sql.ErrNoRows) {
		return RedemptionRecordPage{}, ErrMerchantShopNotFound
	}
	if err != nil {
		return RedemptionRecordPage{}, fmt.Errorf("check merchant shop membership: %w", err)
	}

	query := `SELECT r.id, r.order_id, r.merchant_user_id, r.redeemed_at
		FROM redemptions r
		JOIN orders o ON o.id = r.order_id
		JOIN merchant_shop_members m ON m.shop_id = o.shop_id AND m.user_id = ?
		WHERE o.shop_id = ?`
	args := []any{merchantID, shopID}
	if page.cursor != nil {
		query += ` AND (r.redeemed_at < ? OR (r.redeemed_at = ? AND r.id < ?))`
		args = append(args, page.cursor.At, page.cursor.At, page.cursor.ID)
	}
	query += ` ORDER BY r.redeemed_at DESC, r.id DESC LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return RedemptionRecordPage{}, fmt.Errorf("list merchant redemption records: %w", err)
	}
	defer rows.Close()
	items := make([]RedemptionRecord, 0, page.Limit)
	for rows.Next() {
		var item RedemptionRecord
		if err := rows.Scan(&item.RedemptionID, &item.OrderID, &item.MerchantUserID, &item.RedeemedAt); err != nil {
			return RedemptionRecordPage{}, fmt.Errorf("scan redemption record: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return RedemptionRecordPage{}, fmt.Errorf("iterate merchant redemption records: %w", err)
	}
	return redemptionPage(items, page.Limit), nil
}

func validRecordPage(page RecordPage) bool {
	return page.Limit >= 1 && page.Limit <= MaxRecordPageSize &&
		(page.cursor == nil || (!page.cursor.At.IsZero() && page.cursor.ID > 0))
}

func paymentPage(items []PaymentRecord, limit int) PaymentRecordPage {
	result := PaymentRecordPage{Items: items}
	if len(items) <= limit {
		return result
	}
	last := items[limit-1]
	result.Items = items[:limit]
	result.NextCursor = encodeRecordCursor(recordCursor{At: last.CreatedAt, ID: last.PaymentID})
	return result
}

func redemptionPage(items []RedemptionRecord, limit int) RedemptionRecordPage {
	result := RedemptionRecordPage{Items: items}
	if len(items) <= limit {
		return result
	}
	last := items[limit-1]
	result.Items = items[:limit]
	result.NextCursor = encodeRecordCursor(recordCursor{At: last.RedeemedAt, ID: last.RedemptionID})
	return result
}
