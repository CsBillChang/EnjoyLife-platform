package order

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const MaxFulfillmentRecoveryBatch = 1000
const StockRestoreTaskKind = "ADMISSION_STOCK_RESTORE"

type StockRestorePayload struct {
	ActivityID        uint64 `json:"activity_id"`
	Generation        uint64 `json:"generation"`
	OrderID           uint64 `json:"order_id"`
	RequestGeneration uint64 `json:"request_generation"`
}

// The RELEASE and its Redis repair intent share the caller's transaction.
// Generation is the CURRENT locked activity epoch: an old order refunded after
// a rebuild creates new stock that was not covered by the earlier snapshot.
func enqueueStockRestore(ctx context.Context, locked *lockedFulfillment) error {
	var requestGeneration uint64
	var state ReservationState
	err := locked.tx.QueryRowContext(ctx, `SELECT generation,reservation_state FROM purchase_requests WHERE order_id = ?`, locked.order.ID).Scan(&requestGeneration, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} // Synchronous path did not reserve Redis.
	if err != nil {
		return fulfillmentDBError("read released admission generation", err)
	}
	if state == ReservationNone {
		return nil
	} // Legacy L09 never used the reservation path.
	payload, err := json.Marshal(StockRestorePayload{ActivityID: locked.order.ActivityID, Generation: locked.generation, OrderID: locked.order.ID, RequestGeneration: requestGeneration})
	if err != nil {
		return err
	}
	_, err = locked.tx.ExecContext(ctx, `INSERT INTO recovery_tasks (kind,source_id,generation,status,available_at,payload)
		VALUES (?, ?, ?, 'PENDING', CURRENT_TIMESTAMP(6), ?) ON DUPLICATE KEY UPDATE source_id = source_id`, StockRestoreTaskKind, strconv.FormatUint(locked.order.ID, 10), locked.generation, payload)
	if err != nil {
		return fulfillmentDBError("enqueue inventory admission restore", err)
	}
	return nil
}

// ListExpiredOrderIDs returns candidates only. CloseExpired re-locks and
// rechecks each order; concurrent payment, scanners and rebuild are safe.
// Keep a monotonic afterID within a pass; begin the next pass at zero.
func (s *FulfillmentService) ListExpiredOrderIDs(ctx context.Context, afterID uint64, limit int, now time.Time) ([]uint64, error) {
	if limit < 1 || limit > MaxFulfillmentRecoveryBatch || !validFulfillmentTime(now) {
		return nil, ErrInvalidFulfillmentInput
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM orders WHERE id > ? AND status = 'PENDING_PAYMENT' AND payment_deadline <= ? ORDER BY id LIMIT ?`, afterID, now.UTC(), limit)
	if err != nil {
		return nil, fulfillmentDBError("list expired orders", err)
	}
	defer rows.Close()
	ids := make([]uint64, 0, limit)
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fulfillmentDBError("scan expired order", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fulfillmentDBError("iterate expired orders", err)
	}
	return ids, nil
}

type PendingRefund struct {
	Refund
	Provider              string `json:"provider"`
	ProviderTransactionID string `json:"provider_transaction_id"`
}

// ListPendingRefunds includes NORMAL, LATE_PAYMENT and EXTRA_PAYMENT. The
// worker must query/retry the provider with Refund.RequestID as the stable
// identity, outside a DB transaction, then call ApplyRefundResult. Concurrent
// workers require provider-side idempotency; selection itself is not a lease.
func (s *FulfillmentService) ListPendingRefunds(ctx context.Context, afterID uint64, limit int) ([]PendingRefund, error) {
	if limit < 1 || limit > MaxFulfillmentRecoveryBatch {
		return nil, ErrInvalidFulfillmentInput
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.order_id,r.payment_id,r.refund_request_id,r.purpose,r.amount_cents,r.currency,r.status,p.provider,p.provider_transaction_id
		FROM refunds r JOIN payments p ON p.id = r.payment_id
		WHERE r.id > ? AND r.status IN ('PENDING','UNKNOWN') ORDER BY r.id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fulfillmentDBError("list pending refunds", err)
	}
	defer rows.Close()
	items := make([]PendingRefund, 0, limit)
	for rows.Next() {
		var item PendingRefund
		if err := rows.Scan(&item.ID, &item.OrderID, &item.PaymentID, &item.RequestID, &item.Purpose, &item.AmountCents, &item.Currency, &item.Status, &item.Provider, &item.ProviderTransactionID); err != nil {
			return nil, fulfillmentDBError("scan pending refund", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fulfillmentDBError("iterate pending refunds", err)
	}
	return items, nil
}
