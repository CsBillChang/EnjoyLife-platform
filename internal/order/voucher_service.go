package order

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type VoucherResult struct {
	OrderID     uint64    `json:"order_id"`
	VoucherCode string    `json:"voucher_code"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ReissueOwnedVoucher repairs a lost one-time payment response without storing
// plaintext. It rotates the one existing voucher's hash; old codes immediately
// stop working. A lost reissue response is repaired by another authorized
// reissue, not by issuing a second voucher. Redemption/refund share the same
// activity -> order -> voucher lock order and therefore cannot race eligibility.
func (s *FulfillmentService) ReissueOwnedVoucher(ctx context.Context, userID, orderID uint64, now time.Time) (VoucherResult, error) {
	if userID == 0 || orderID == 0 || !validFulfillmentTime(now) {
		return VoucherResult{}, ErrInvalidFulfillmentInput
	}
	locked, err := s.lockOrder(ctx, orderID, userID)
	if err != nil {
		return VoucherResult{}, err
	}
	defer locked.tx.Rollback()
	if locked.order.Status != Paid {
		return VoucherResult{}, ErrOrderStateConflict
	}
	var id uint64
	var status string
	var expires time.Time
	err = locked.tx.QueryRowContext(ctx, `SELECT id,status,expires_at FROM vouchers WHERE order_id = ? FOR UPDATE`, orderID).Scan(&id, &status, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return VoucherResult{}, ErrVoucherNotFound
	}
	if err != nil {
		return VoucherResult{}, fulfillmentDBError("lock owned voucher", err)
	}
	now = s.clock().UTC().Truncate(time.Microsecond)
	if !validFulfillmentTime(now) {
		return VoucherResult{}, ErrInvalidFulfillmentInput
	}
	if err := validateRedemption(locked.order.Status, status, expires, now); err != nil {
		return VoucherResult{}, err
	}
	code, hash, err := newVoucherCode()
	if err != nil {
		return VoucherResult{}, err
	}
	update, err := locked.tx.ExecContext(ctx, `UPDATE vouchers SET code_hash = ? WHERE id = ? AND status = 'ACTIVE'`, hash[:], id)
	if err != nil {
		return VoucherResult{}, fulfillmentDBError("rotate voucher code", err)
	}
	if err := requireOneChanged(update, ErrVoucherUnavailable); err != nil {
		return VoucherResult{}, err
	}
	if err := locked.tx.Commit(); err != nil {
		return VoucherResult{}, fulfillmentDBError("commit voucher reissue", err)
	}
	return VoucherResult{OrderID: orderID, VoucherCode: code, ExpiresAt: expires}, nil
}
