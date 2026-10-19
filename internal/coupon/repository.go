package coupon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Repository interface {
	ListByShop(context.Context, uint64, int, int) ([]Activity, int, error)
	FindByID(context.Context, uint64) (Activity, error)
}

type MySQLRepository struct{ db *sql.DB }

func NewMySQLRepository(db *sql.DB) *MySQLRepository { return &MySQLRepository{db: db} }

func (r *MySQLRepository) ListByShop(ctx context.Context, shopID uint64, offset, limit int) ([]Activity, int, error) {
	if shopID == 0 {
		return nil, 0, ErrInvalidShopID
	}
	if offset < 0 || limit < 1 {
		return nil, 0, ErrInvalidPagination
	}
	// Public discovery includes scheduled and active OPEN activities, but never
	// ended ones. Purchase eligibility is still decided by order transactions.
	now := time.Now().UTC()
	var total int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM coupon_activities a JOIN shops s ON s.id=a.shop_id
		WHERE a.shop_id=? AND s.status='OPEN' AND a.admission_state='OPEN' AND a.ends_at>?`, shopID, now).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count activities: %w", err)
	}
	rows, err := r.db.QueryContext(ctx, activityColumns+` FROM coupon_activities a JOIN shops s ON s.id=a.shop_id
		WHERE a.shop_id=? AND s.status='OPEN' AND a.admission_state='OPEN' AND a.ends_at>? ORDER BY a.starts_at, a.id LIMIT ? OFFSET ?`, shopID, now, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list activities: %w", err)
	}
	defer rows.Close()
	items := make([]Activity, 0)
	for rows.Next() {
		item, err := scanActivity(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate activities: %w", err)
	}
	return items, total, nil
}

func (r *MySQLRepository) FindByID(ctx context.Context, id uint64) (Activity, error) {
	if id == 0 {
		return Activity{}, ErrInvalidActivityID
	}
	item, err := scanActivity(r.db.QueryRowContext(ctx, activityColumns+` FROM coupon_activities a JOIN shops s ON s.id=a.shop_id
		WHERE a.id=? AND s.status='OPEN' AND a.admission_state='OPEN' AND a.ends_at>?`, id, time.Now().UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return Activity{}, ErrActivityNotFound
	}
	if err != nil {
		return Activity{}, fmt.Errorf("find activity: %w", err)
	}
	return item, nil
}

const activityColumns = `SELECT a.id, a.shop_id, a.title, a.price_cents, a.currency, a.starts_at, a.ends_at,
	a.voucher_valid_until, a.available_stock, a.generation`

func scanActivity(row interface{ Scan(...any) error }) (Activity, error) {
	var a Activity
	err := row.Scan(&a.ID, &a.ShopID, &a.Title, &a.PriceCents, &a.Currency, &a.StartsAt, &a.EndsAt,
		&a.VoucherValidUntil, &a.AvailableStock, &a.Generation)
	return a, err
}

// UpdateOwned keeps membership in the transaction and authorizes it with a
// predicate tied to the activity's actual shop, not a caller-supplied shop ID.
func (r *MySQLRepository) UpdateOwned(ctx context.Context, merchantID, activityID uint64, input UpdateInput) (Activity, error) {
	if merchantID == 0 || activityID == 0 || !validUpdateInput(input) {
		return Activity{}, ErrInvalidUpdate
	}
	// Admission Lua keeps activity timing and price metadata in Redis. Updating
	// those terms without an atomic cross-store rebuild could create a window in
	// which Redis admits by old terms while MySQL records new ones. Merchant
	// maintenance is therefore deliberately title-only in this API.
	if input.changesTerms() {
		return Activity{}, ErrActivityImmutable
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Activity{}, fmt.Errorf("begin activity update: %w", err)
	}
	defer tx.Rollback()
	a, err := scanActivity(tx.QueryRowContext(ctx, activityColumns+` FROM coupon_activities a WHERE a.id=? FOR UPDATE`, activityID))
	if errors.Is(err, sql.ErrNoRows) {
		return Activity{}, ErrActivityNotFound
	}
	if err != nil {
		return Activity{}, fmt.Errorf("lock activity update: %w", err)
	}
	var member uint64
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM merchant_shop_members WHERE user_id=? AND shop_id=? FOR SHARE`, merchantID, a.ShopID).Scan(&member)
	if errors.Is(err, sql.ErrNoRows) {
		return Activity{}, ErrMerchantForbidden
	}
	if err != nil {
		return Activity{}, fmt.Errorf("authorize activity update: %w", err)
	}
	if input.Title != nil {
		a.Title = strings.TrimSpace(*input.Title)
	}
	if a.Title == "" {
		return Activity{}, ErrInvalidUpdate
	}
	_, err = tx.ExecContext(ctx, `UPDATE coupon_activities SET title=? WHERE id=?`, a.Title, activityID)
	if err != nil {
		return Activity{}, fmt.Errorf("update activity: %w", err)
	}
	// coupon_activities.generation fences purchase admission. Do not advance it
	// for a display-only title edit, or an already admitted request could fail.
	// recovery_tasks has its own fencing generation for cache work.
	cacheGeneration := uint64(time.Now().UTC().UnixMicro())
	a.CacheGeneration = cacheGeneration
	payload, err := couponInvalidationPayload{ActivityID: a.ID, ShopID: a.ShopID, Generation: cacheGeneration}.marshal()
	if err != nil {
		return Activity{}, err
	}
	if err := upsertCouponInvalidation(tx, a.ID, cacheGeneration, payload); err != nil {
		return Activity{}, err
	}
	if err := tx.Commit(); err != nil {
		return Activity{}, fmt.Errorf("commit activity update: %w", err)
	}
	return a, nil
}

func validUpdateInput(in UpdateInput) bool {
	if in.Title == nil && !in.changesTerms() {
		return false
	}
	if in.Title != nil && strings.TrimSpace(*in.Title) == "" {
		return false
	}
	return true
}
