// Package coupon contains the public, display-only view of coupon activities
// and the merchant maintenance boundary.  It deliberately does not contain
// purchase settlement: order transactions re-read coupon_activities from
// MySQL and never authorize a purchase from this package's cache.
package coupon

import (
	"errors"
	"time"
)

var (
	ErrInvalidActivityID = errors.New("invalid activity id")
	ErrInvalidShopID     = errors.New("invalid shop id")
	ErrInvalidPagination = errors.New("invalid pagination")
	ErrActivityNotFound  = errors.New("activity not found")
	ErrMerchantForbidden = errors.New("merchant is not assigned to this activity's shop")
	ErrInvalidUpdate     = errors.New("invalid activity update")
	ErrActivityImmutable = errors.New("activity terms cannot change after purchase activity exists")
	ErrCacheMiss         = errors.New("cache miss")
)

// Activity is the internal public-read model. AvailableStock is intentionally
// named AvailableStockHint in HTTP responses: it is a display hint only.
type Activity struct {
	ID                uint64
	ShopID            uint64
	Title             string
	PriceCents        uint64
	Currency          string
	StartsAt          time.Time
	EndsAt            time.Time
	VoucherValidUntil time.Time
	AvailableStock    uint64
	// CacheGeneration belongs to recovery_tasks and fences display invalidation;
	// Generation above remains the purchase-admission generation from MySQL.
	CacheGeneration uint64
	Generation      uint64
}

type DisplayActivity struct {
	ID                 uint64    `json:"id"`
	ShopID             uint64    `json:"shop_id"`
	Title              string    `json:"title"`
	PriceCents         uint64    `json:"price_cents"`
	Currency           string    `json:"currency"`
	StartsAt           time.Time `json:"starts_at"`
	EndsAt             time.Time `json:"ends_at"`
	VoucherValidUntil  time.Time `json:"voucher_valid_until"`
	AvailableStockHint uint64    `json:"available_stock_hint"`
}

func (a Activity) Display() DisplayActivity {
	return DisplayActivity{ID: a.ID, ShopID: a.ShopID, Title: a.Title, PriceCents: a.PriceCents,
		Currency: a.Currency, StartsAt: a.StartsAt, EndsAt: a.EndsAt,
		VoucherValidUntil: a.VoucherValidUntil, AvailableStockHint: a.AvailableStock}
}

// UpdateInput uses pointers so an omitted protected field can be distinguished
// from a zero value. Shop, stock, and generation are intentionally absent.
type UpdateInput struct {
	Title             *string
	PriceCents        *uint64
	StartsAt          *time.Time
	EndsAt            *time.Time
	VoucherValidUntil *time.Time
}

func (u UpdateInput) changesTerms() bool {
	return u.PriceCents != nil || u.StartsAt != nil || u.EndsAt != nil || u.VoucherValidUntil != nil
}
