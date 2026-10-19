package shop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

const shopCacheInvalidationKind = "SHOP_CACHE_INVALIDATE"

var ErrInvalidationLeaseLost = errors.New("shop cache invalidation lease is no longer owned")

type shopCacheInvalidationPayload struct {
	ShopID     int64  `json:"shop_id"`
	Generation uint64 `json:"generation"`
}

// InvalidationTask is a leased durable instruction. The lease token fences a
// worker which paused past its lease; generation additionally fences an older
// shop update which was superseded by a later update.
type InvalidationTask struct {
	ID, ShopID int64
	Generation uint64
	Attempts   uint32
	LeaseUntil time.Time
	leaseToken string
}

type InvalidationConfig struct {
	LeaseDuration time.Duration
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	MaxAttempts   uint32
}

func (c InvalidationConfig) withDefaults() InvalidationConfig {
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = time.Minute
	}
	if c.BackoffMax < c.BackoffBase {
		c.BackoffMax = c.BackoffBase
	}
	if c.MaxAttempts == 0 || c.MaxAttempts > 1000 {
		c.MaxAttempts = 20
	}
	return c
}

// InvalidationDispatcher is intentionally a single-process dispatcher. It
// invalidates this API process's L1 and its shared Redis L2; it is not a
// multi-instance L1 broadcast mechanism.
type InvalidationDispatcher struct {
	db          *gorm.DB
	invalidator interface {
		Invalidate(context.Context, int64) error
	}
	config InvalidationConfig
}

func NewInvalidationDispatcher(db *gorm.DB, invalidator interface {
	Invalidate(context.Context, int64) error
}, config InvalidationConfig) (*InvalidationDispatcher, error) {
	if db == nil {
		return nil, errors.New("shop invalidation database is required")
	}
	if invalidator == nil {
		return nil, errors.New("shop invalidator is required")
	}
	return &InvalidationDispatcher{db: db, invalidator: invalidator, config: config.withDefaults()}, nil
}

func upsertShopCacheInvalidation(tx *gorm.DB, shopID int64, generation uint64, payload []byte) error {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := tx.Exec(`INSERT INTO recovery_tasks
		(kind, source_id, generation, status, attempts, available_at, lease_until, payload, last_error)
		VALUES (?, ?, ?, 'PENDING', 0, ?, NULL, ?, NULL)
		ON DUPLICATE KEY UPDATE generation = VALUES(generation), status = 'PENDING', attempts = 0,
		available_at = VALUES(available_at), lease_until = NULL, payload = VALUES(payload), last_error = NULL`,
		shopCacheInvalidationKind, fmt.Sprint(shopID), generation, now, payload).Error; err != nil {
		return fmt.Errorf("upsert shop cache invalidation: %w", err)
	}
	return nil
}

// ClaimOne atomically leases at most one task. An expired RUNNING task is
// eligible again, so a process crash cannot strand invalidation forever.
func (d *InvalidationDispatcher) ClaimOne(ctx context.Context, now time.Time) (*InvalidationTask, error) {
	now = now.UTC().Truncate(time.Microsecond)
	leaseUntil := now.Add(d.config.LeaseDuration)
	var task *InvalidationTask
	var invalidPayload error
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE recovery_tasks SET status = 'FAILED', lease_until = NULL,
			last_error = COALESCE(last_error, 'invalidation attempt budget exhausted')
			WHERE kind = ? AND attempts >= ? AND
			(status = 'PENDING' OR (status = 'RUNNING' AND lease_until <= ?))`,
			shopCacheInvalidationKind, d.config.MaxAttempts, now).Error; err != nil {
			return fmt.Errorf("retire exhausted invalidations: %w", err)
		}
		var row struct {
			ID         int64
			SourceID   string
			Generation uint64
			Attempts   uint32
			Payload    []byte
		}
		err := tx.Raw(`SELECT id, source_id, generation, attempts, payload FROM recovery_tasks
			WHERE kind = ? AND attempts < ? AND ((status = 'PENDING' AND available_at <= ?) OR (status = 'RUNNING' AND lease_until <= ?))
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, shopCacheInvalidationKind, d.config.MaxAttempts, now, now).Scan(&row).Error
		if err != nil {
			return fmt.Errorf("select shop invalidation: %w", err)
		}
		if row.ID == 0 {
			return nil
		}
		var payload shopCacheInvalidationPayload
		if decodeErr := json.Unmarshal(row.Payload, &payload); decodeErr != nil || payload.ShopID <= 0 || payload.Generation == 0 || payload.Generation != row.Generation {
			invalidPayload = fmt.Errorf("invalid shop invalidation payload for task %d", row.ID)
			result := tx.Exec(`UPDATE recovery_tasks SET status = 'FAILED', lease_until = NULL,
				last_error = ? WHERE id = ? AND generation = ?`, invalidPayload.Error(), row.ID, row.Generation)
			if result.Error != nil {
				return fmt.Errorf("isolate invalid shop task: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return ErrInvalidationLeaseLost
			}
			// Returning the payload error here would roll back its isolation and
			// select this same row forever. Report it only after the commit.
			return nil
		}
		token, err := invalidationLeaseToken()
		if err != nil {
			return err
		}
		result := tx.Exec(`UPDATE recovery_tasks SET status = 'RUNNING', attempts = attempts + 1, lease_until = ?,
			payload = JSON_SET(payload, '$.lease_token', ?), last_error = NULL
			WHERE id = ? AND generation = ? AND attempts = ?`, leaseUntil, token, row.ID, row.Generation, row.Attempts)
		if result.Error != nil {
			return fmt.Errorf("lease shop invalidation: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrInvalidationLeaseLost
		}
		task = &InvalidationTask{ID: row.ID, ShopID: payload.ShopID, Generation: row.Generation, Attempts: row.Attempts + 1, LeaseUntil: leaseUntil, leaseToken: token}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return task, invalidPayload
}

// DispatchOne performs no database transaction while touching Redis. Only the
// currently leased generation/token may record its outcome.
func (d *InvalidationDispatcher) DispatchOne(ctx context.Context, now time.Time) (bool, error) {
	task, err := d.ClaimOne(ctx, now)
	if err != nil || task == nil {
		return false, err
	}
	if err := d.invalidator.Invalidate(ctx, task.ShopID); err != nil {
		return true, d.releaseFailure(ctx, *task, now, err)
	}
	return true, d.markSucceeded(ctx, *task, now)
}

func (d *InvalidationDispatcher) markSucceeded(ctx context.Context, task InvalidationTask, now time.Time) error {
	now = now.UTC().Truncate(time.Microsecond)
	result := d.db.WithContext(ctx).Exec(`UPDATE recovery_tasks SET status = 'SUCCEEDED', lease_until = NULL, last_error = NULL
		WHERE id = ? AND generation = ? AND status = 'RUNNING' AND lease_until > ?
		AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.lease_token')) = ?`, task.ID, task.Generation, now, task.leaseToken)
	if result.Error != nil {
		return fmt.Errorf("mark shop invalidation succeeded: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrInvalidationLeaseLost
	}
	return nil
}

func (d *InvalidationDispatcher) releaseFailure(ctx context.Context, task InvalidationTask, now time.Time, cause error) error {
	now = now.UTC().Truncate(time.Microsecond)
	availableAt := now.Add(invalidationBackoff(task.Attempts, d.config.BackoffBase, d.config.BackoffMax))
	message := cause.Error()
	if len(message) > 255 {
		message = message[:255]
	}
	result := d.db.WithContext(ctx).Exec(`UPDATE recovery_tasks SET status = IF(attempts >= ?, 'FAILED', 'PENDING'),
		available_at = ?, lease_until = NULL, last_error = ?
		WHERE id = ? AND generation = ? AND status = 'RUNNING' AND lease_until > ?
		AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.lease_token')) = ?`, d.config.MaxAttempts, availableAt, message,
		task.ID, task.Generation, now, task.leaseToken)
	if result.Error != nil {
		return fmt.Errorf("release shop invalidation failure: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrInvalidationLeaseLost
	}
	return nil
}

func invalidationBackoff(attempt uint32, base, max time.Duration) time.Duration {
	if attempt <= 1 {
		return base
	}
	for i := uint32(1); i < attempt && base < max; i++ {
		if base > max/2 {
			return max
		}
		base *= 2
	}
	if base > max {
		return max
	}
	return base
}

func invalidationLeaseToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate invalidation lease token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
