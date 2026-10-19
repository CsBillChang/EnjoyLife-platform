package coupon

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const couponCacheInvalidationKind = "COUPON_CACHE_INVALIDATE"

var ErrInvalidationLeaseLost = errors.New("coupon cache invalidation lease lost")

type couponInvalidationPayload struct{ ActivityID, ShopID, Generation uint64 }

func (p couponInvalidationPayload) marshal() ([]byte, error) { return json.Marshal(p) }

type InvalidationConfig struct {
	LeaseDuration, BackoffBase, BackoffMax time.Duration
	MaxAttempts                            uint32
}

func (c InvalidationConfig) defaults() InvalidationConfig {
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Second
	}
	if c.BackoffMax < c.BackoffBase {
		c.BackoffMax = time.Minute
	}
	if c.MaxAttempts == 0 || c.MaxAttempts > 1000 {
		c.MaxAttempts = 20
	}
	return c
}

type InvalidationTask struct {
	ID                             uint64
	ActivityID, ShopID, Generation uint64
	Attempts                       uint32
	token                          string
}
type Invalidator interface {
	Invalidate(context.Context, uint64, uint64, uint64) error
}
type InvalidationDispatcher struct {
	db          *sql.DB
	invalidator Invalidator
	config      InvalidationConfig
}

func NewInvalidationDispatcher(db *sql.DB, invalidator Invalidator, config InvalidationConfig) (*InvalidationDispatcher, error) {
	if db == nil || invalidator == nil {
		return nil, errors.New("coupon invalidation dependencies required")
	}
	return &InvalidationDispatcher{db, invalidator, config.defaults()}, nil
}

func upsertCouponInvalidation(tx *sql.Tx, activityID, generation uint64, payload []byte) error {
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := tx.Exec(`INSERT INTO recovery_tasks (kind,source_id,generation,status,attempts,available_at,lease_until,payload,last_error) VALUES (?,?,?,'PENDING',0,?,NULL,?,NULL)
		ON DUPLICATE KEY UPDATE generation=VALUES(generation),status='PENDING',attempts=0,available_at=VALUES(available_at),lease_until=NULL,payload=VALUES(payload),last_error=NULL`, couponCacheInvalidationKind, fmt.Sprint(activityID), generation, now, payload)
	if err != nil {
		return fmt.Errorf("upsert coupon cache invalidation: %w", err)
	}
	return nil
}

func (d *InvalidationDispatcher) DispatchOne(ctx context.Context, now time.Time) (bool, error) {
	task, err := d.claim(ctx, now)
	if err != nil || task == nil {
		return false, err
	}
	if err = d.invalidator.Invalidate(ctx, task.ActivityID, task.ShopID, task.Generation); err != nil {
		return true, d.fail(ctx, *task, now, err)
	}
	return true, d.succeed(ctx, *task, now)
}
func (d *InvalidationDispatcher) claim(ctx context.Context, now time.Time) (*InvalidationTask, error) {
	now = now.UTC().Truncate(time.Microsecond)
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_tasks SET status='FAILED',lease_until=NULL,last_error=COALESCE(last_error,'coupon invalidation attempt budget exhausted')
		WHERE kind=? AND attempts>=? AND (status='PENDING' OR (status='RUNNING' AND lease_until<=?))`, couponCacheInvalidationKind, d.config.MaxAttempts, now); err != nil {
		return nil, err
	}
	var id uint64
	var generation uint64
	var attempts uint32
	var payload []byte
	err = tx.QueryRowContext(ctx, `SELECT id,generation,attempts,payload FROM recovery_tasks WHERE kind=? AND attempts<? AND ((status='PENDING' AND available_at<=?) OR (status='RUNNING' AND lease_until<=?)) ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, couponCacheInvalidationKind, d.config.MaxAttempts, now, now).Scan(&id, &generation, &attempts, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p couponInvalidationPayload
	if decodeErr := json.Unmarshal(payload, &p); decodeErr != nil || !validInvalidationPayload(p, generation) {
		reason := "coupon invalidation payload does not match task generation"
		if decodeErr != nil {
			reason = "invalid coupon invalidation payload: " + decodeErr.Error()
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recovery_tasks SET status='FAILED',lease_until=NULL,last_error=? WHERE id=? AND generation=?`, reason, id, generation); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, errors.New(reason)
	}
	token, err := token()
	if err != nil {
		return nil, err
	}
	lease := now.Add(d.config.LeaseDuration)
	result, err := tx.ExecContext(ctx, `UPDATE recovery_tasks SET status='RUNNING',attempts=attempts+1,lease_until=?,payload=JSON_SET(payload,'$.lease_token',?),last_error=NULL WHERE id=? AND generation=? AND attempts=?`, lease, token, id, generation, attempts)
	if err != nil {
		return nil, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return nil, ErrInvalidationLeaseLost
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &InvalidationTask{id, p.ActivityID, p.ShopID, p.Generation, attempts + 1, token}, nil
}

func validInvalidationPayload(p couponInvalidationPayload, rowGeneration uint64) bool {
	return p.ActivityID != 0 && p.ShopID != 0 && p.Generation == rowGeneration
}
func (d *InvalidationDispatcher) succeed(ctx context.Context, t InvalidationTask, now time.Time) error {
	// Re-sample after Redis work; a lease can expire while an invalidator is
	// blocked, in which case this worker must not record a late success.
	now = time.Now().UTC().Truncate(time.Microsecond)
	r, e := d.db.ExecContext(ctx, `UPDATE recovery_tasks SET status='SUCCEEDED',lease_until=NULL,last_error=NULL WHERE id=? AND generation=? AND status='RUNNING' AND lease_until>? AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.lease_token'))=?`, t.ID, t.Generation, now.UTC(), t.token)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrInvalidationLeaseLost
	}
	return nil
}
func (d *InvalidationDispatcher) fail(ctx context.Context, t InvalidationTask, now time.Time, cause error) error {
	now = time.Now().UTC().Truncate(time.Microsecond)
	at := now.UTC().Add(backoff(t.Attempts, d.config.BackoffBase, d.config.BackoffMax))
	msg := cause.Error()
	if len(msg) > 255 {
		msg = msg[:255]
	}
	r, e := d.db.ExecContext(ctx, `UPDATE recovery_tasks SET status=IF(attempts>=?,'FAILED','PENDING'),available_at=?,lease_until=NULL,last_error=? WHERE id=? AND generation=? AND status='RUNNING' AND lease_until>? AND JSON_UNQUOTE(JSON_EXTRACT(payload,'$.lease_token'))=?`, d.config.MaxAttempts, at, msg, t.ID, t.Generation, now.UTC(), t.token)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrInvalidationLeaseLost
	}
	return nil
}
func backoff(n uint32, base, max time.Duration) time.Duration {
	for i := uint32(1); i < n && base < max; i++ {
		base *= 2
		if base >= max {
			return max
		}
	}
	return base
}
func token() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}
