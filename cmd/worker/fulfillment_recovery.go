package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/payment"
)

const fulfillmentRecoveryInterval = 2 * time.Second

var errStockRestoreLeaseLost = errors.New("stock restore task lease is no longer owned")

type fulfillmentRecoveryService interface {
	ListExpiredOrderIDs(context.Context, uint64, int, time.Time) ([]uint64, error)
	CloseExpired(context.Context, uint64, time.Time) (order.Order, error)
	ListPendingRefunds(context.Context, uint64, int) ([]order.PendingRefund, error)
	ApplyRefundResult(context.Context, string, order.RefundResult, time.Time) (order.Refund, error)
}

type refundRecoveryProvider interface {
	Refund(payment.RefundRequest) (payment.RefundResult, error)
}

type committedStockRestorer interface {
	RestoreCommittedStock(context.Context, uint64, uint64, uint64) (admission.Result, error)
}

func runFulfillmentRecovery(ctx context.Context, db *sql.DB, service fulfillmentRecoveryService, provider refundRecoveryProvider, restorer committedStockRestorer) {
	repository := stockRestoreRepository{db: db}
	ticker := time.NewTicker(fulfillmentRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := recoverExpiredOrders(ctx, service, recoveryBatchSize, now); err != nil && ctx.Err() == nil {
				slog.Warn("recover expired orders", "error", err)
			}
			if err := recoverPendingRefunds(ctx, service, provider, recoveryBatchSize, now); err != nil && ctx.Err() == nil {
				slog.Warn("recover pending refunds", "error", err)
			}
			if err := recoverCommittedStock(ctx, repository, restorer, recoveryBatchSize, now); err != nil && ctx.Err() == nil {
				slog.Warn("recover admission stock", "error", err)
			}
		}
	}
}

func recoverExpiredOrders(ctx context.Context, service fulfillmentRecoveryService, limit int, now time.Time) error {
	if limit <= 0 || limit > order.MaxFulfillmentRecoveryBatch {
		limit = recoveryBatchSize
	}
	var afterID uint64
	var failures []error
	for {
		ids, err := service.ListExpiredOrderIDs(ctx, afterID, limit, now)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		for _, id := range ids {
			afterID = id
			_, err := service.CloseExpired(ctx, id, time.Now())
			if err == nil || errors.Is(err, order.ErrOrderNotFound) || errors.Is(err, order.ErrOrderNotExpired) || errors.Is(err, order.ErrOrderStateConflict) {
				continue
			}
			failures = append(failures, fmt.Errorf("order %d: %w", id, err))
		}
		if len(ids) < limit {
			return errors.Join(failures...)
		}
	}
}

func recoverPendingRefunds(ctx context.Context, service fulfillmentRecoveryService, provider refundRecoveryProvider, limit int, now time.Time) error {
	if limit <= 0 || limit > order.MaxFulfillmentRecoveryBatch {
		limit = recoveryBatchSize
	}
	var afterID uint64
	var failures []error
	for {
		items, err := service.ListPendingRefunds(ctx, afterID, limit)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		for _, item := range items {
			afterID = item.ID
			if item.AmountCents > math.MaxInt64 {
				failures = append(failures, fmt.Errorf("refund %d amount exceeds provider range", item.ID))
				continue
			}
			providerResult, err := provider.Refund(payment.RefundRequest{
				RefundRequestID:       item.RequestID,
				ProviderTransactionID: fmt.Sprintf("payment-%d", item.PaymentID),
				Amount:                int64(item.AmountCents),
				Currency:              item.Currency,
				InitialStatus:         payment.StatusSuccess,
			})
			if err != nil {
				failures = append(failures, fmt.Errorf("refund %d provider: %w", item.ID, err))
				continue
			}
			result := order.RefundFailedFinal
			switch providerResult.Status {
			case payment.StatusSuccess:
				result = order.RefundSucceeded
			case payment.StatusUnknown:
				result = order.RefundUnknown
			}
			if _, err := service.ApplyRefundResult(ctx, item.RequestID, result, time.Now()); err != nil && !errors.Is(err, order.ErrRefundConflict) {
				failures = append(failures, fmt.Errorf("refund %d apply: %w", item.ID, err))
			}
		}
		if len(items) < limit {
			return errors.Join(failures...)
		}
	}
}

type stockRestoreTask struct {
	ID         uint64
	Generation uint64
	Attempts   uint64
	Payload    order.StockRestorePayload
	leaseToken string
}

type stockRestoreTaskRepository interface {
	Claim(context.Context, time.Time) (stockRestoreTask, bool, error)
	CurrentGeneration(context.Context, uint64) (uint64, error)
	Succeed(context.Context, stockRestoreTask) error
	Retry(context.Context, stockRestoreTask, time.Time, error) error
}

type stockRestoreRepository struct{ db *sql.DB }

func (r stockRestoreRepository) Claim(ctx context.Context, _ time.Time) (stockRestoreTask, bool, error) {
	var task stockRestoreTask
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return task, false, err
	}
	defer tx.Rollback()
	var payload []byte
	// The scan timestamp belongs to the application clock. Persisted scheduling
	// and leases use the database clock for both creation and eligibility.
	err = tx.QueryRowContext(ctx, `SELECT id,generation,attempts,payload FROM recovery_tasks
		WHERE kind = ? AND available_at <= CURRENT_TIMESTAMP(6)
		AND (status = 'PENDING' OR (status = 'RUNNING' AND lease_until <= CURRENT_TIMESTAMP(6)))
		ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`, order.StockRestoreTaskKind).Scan(&task.ID, &task.Generation, &task.Attempts, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return task, false, nil
	}
	if err != nil {
		return task, false, err
	}
	if decodeErr := json.Unmarshal(payload, &task.Payload); decodeErr != nil || task.Payload.ActivityID == 0 || task.Payload.OrderID == 0 || task.Generation == 0 || task.Payload.Generation != task.Generation {
		reason := fmt.Errorf("stock restore task %d has invalid payload or generation", task.ID)
		if _, err := tx.ExecContext(ctx, `UPDATE recovery_tasks SET status = 'FAILED', lease_until = NULL,
			last_error = ? WHERE id = ? AND kind = ? AND generation = ?`, reason.Error(), task.ID, order.StockRestoreTaskKind, task.Generation); err != nil {
			return task, false, err
		}
		if err := tx.Commit(); err != nil {
			return task, false, err
		}
		return task, false, reason
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return task, false, fmt.Errorf("create stock restore lease token: %w", err)
	}
	task.leaseToken = hex.EncodeToString(token[:])
	// Use a fresh database timestamp after the row lock, not the scan's timestamp.
	// The token lives alongside the payload, so existing task tables need no DDL.
	result, err := tx.ExecContext(ctx, `UPDATE recovery_tasks SET status = 'RUNNING', attempts = attempts + 1,
		lease_until = CURRENT_TIMESTAMP(6) + INTERVAL 10 SECOND,
		payload = JSON_SET(payload, '$.lease_token', ?), last_error = NULL
		WHERE id = ? AND kind = ? AND generation = ? AND attempts = ?`,
		task.leaseToken, task.ID, order.StockRestoreTaskKind, task.Generation, task.Attempts)
	if err != nil {
		return task, false, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return task, false, err
		}
		return task, false, errStockRestoreLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return task, false, err
	}
	task.Attempts++
	return task, true, nil
}

func (r stockRestoreRepository) CurrentGeneration(ctx context.Context, activityID uint64) (uint64, error) {
	var generation uint64
	err := r.db.QueryRowContext(ctx, `SELECT generation FROM coupon_activities WHERE id = ?`, activityID).Scan(&generation)
	return generation, err
}

func (r stockRestoreRepository) Succeed(ctx context.Context, task stockRestoreTask) error {
	if task.leaseToken == "" || task.Attempts == 0 {
		return errStockRestoreLeaseLost
	}
	return r.updateOwnedLease(ctx, task, `UPDATE recovery_tasks SET status = 'SUCCEEDED', lease_until = NULL,
		last_error = NULL WHERE id = ? AND kind = ? AND generation = ? AND attempts = ?
		AND status = 'RUNNING' AND lease_until > CURRENT_TIMESTAMP(6)
		AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.lease_token')) = ?`,
		task.ID, order.StockRestoreTaskKind, task.Generation, task.Attempts, task.leaseToken)
}

func (r stockRestoreRepository) Retry(ctx context.Context, task stockRestoreTask, _ time.Time, cause error) error {
	if task.leaseToken == "" || task.Attempts == 0 {
		return errStockRestoreLeaseLost
	}
	delaySeconds := min(task.Attempts, uint64(30))
	if delaySeconds < 1 {
		delaySeconds = 1
	}
	message := cause.Error()
	if len(message) > 255 {
		message = message[:255]
	}
	return r.updateOwnedLease(ctx, task, `UPDATE recovery_tasks SET status = 'PENDING', lease_until = NULL,
		available_at = TIMESTAMPADD(SECOND, ?, CURRENT_TIMESTAMP(6)), last_error = ?
		WHERE id = ? AND kind = ? AND generation = ? AND attempts = ?
		AND status = 'RUNNING' AND lease_until > CURRENT_TIMESTAMP(6)
		AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.lease_token')) = ?`,
		delaySeconds, message, task.ID, order.StockRestoreTaskKind, task.Generation, task.Attempts, task.leaseToken)
}

func (r stockRestoreRepository) updateOwnedLease(ctx context.Context, task stockRestoreTask, query string, args ...any) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id uint64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM recovery_tasks WHERE id=? AND kind=? FOR UPDATE`, task.ID, order.StockRestoreTaskKind).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errStockRestoreLeaseLost
		}
		return err
	}
	// CURRENT_TIMESTAMP is fixed at statement start. Lock in a separate statement
	// so the fenced UPDATE samples time after any row-lock wait, while we own it.
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return errStockRestoreLeaseLost
	}
	return tx.Commit()
}

func recoverCommittedStock(ctx context.Context, repository stockRestoreTaskRepository, restorer committedStockRestorer, limit int, now time.Time) error {
	if limit <= 0 {
		limit = recoveryBatchSize
	}
	var failures []error
	for range limit {
		task, found, err := repository.Claim(ctx, now)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if !found {
			break
		}
		current, err := repository.CurrentGeneration(ctx, task.Payload.ActivityID)
		if err == nil {
			switch {
			case current > task.Generation:
				// The fenced rebuild snapshots MySQL after RELEASE committed.
				err = nil
			case current == task.Generation:
				_, err = restorer.RestoreCommittedStock(ctx, task.Payload.ActivityID, task.Generation, task.Payload.OrderID)
			default:
				err = fmt.Errorf("activity generation %d is behind task generation %d", current, task.Generation)
			}
		}
		if err == nil {
			err = repository.Succeed(ctx, task)
		}
		if err != nil {
			if retryErr := repository.Retry(ctx, task, time.Now(), err); retryErr != nil {
				err = errors.Join(err, retryErr)
			}
			failures = append(failures, fmt.Errorf("task %d order %s: %w", task.ID, strconv.FormatUint(task.Payload.OrderID, 10), err))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%d stock restore task(s) deferred; first: %w", len(failures), failures[0])
}
