// Package mq contains the durable message-delivery boundary for asynchronous work.
package mq

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

var ErrLeaseLost = errors.New("outbox event lease is no longer owned")

const maxOutboxErrorBytes = 255

// OutboxEvent is a claimed, durable event. Attempts is incremented as part of
// ClaimBatch's transaction, before any broker interaction occurs.
type OutboxEvent struct {
	ID            uint64
	EventID       string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	Attempts      uint32
	AvailableAt   time.Time
	LeaseUntil    time.Time
	OwnerToken    string
}

type OutboxConfig struct {
	LeaseDuration time.Duration
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	MaxBatchSize  int
	MaxAttempts   uint32
}

func (c OutboxConfig) withDefaults() OutboxConfig {
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
	// A serialized publisher must not reserve work that sits behind other
	// publications until its lease expires. Scale using independent claimers.
	c.MaxBatchSize = 1
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 20
	}
	if c.MaxAttempts > 1000 {
		c.MaxAttempts = 1000
	}
	return c
}

// OutboxRepository claims events in short database transactions and never holds
// a transaction while publishing to RabbitMQ.
type OutboxRepository struct {
	db     *sql.DB
	config OutboxConfig
}

func NewOutboxRepository(db *sql.DB, config OutboxConfig) *OutboxRepository {
	return &OutboxRepository{db: db, config: config.withDefaults()}
}

// ClaimBatch returns at most one leased event, even when limit is larger. Each
// event therefore gets its own publication interval instead of aging in a batch.
// SKIP LOCKED permits independent publishers; expired exhausted leases become
// DEAD, preserving the error without poisoning later claims or overflowing SQL.
func (r *OutboxRepository) ClaimBatch(ctx context.Context, limit int, now time.Time) ([]OutboxEvent, error) {
	if r.db == nil {
		return nil, errors.New("outbox database is required")
	}
	if limit <= 0 {
		return []OutboxEvent{}, nil
	}
	if limit > r.config.MaxBatchSize {
		limit = r.config.MaxBatchSize
	}
	now = now.UTC().Truncate(time.Microsecond)
	leaseUntil := now.Add(r.config.LeaseDuration)

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_events
		SET status = 'DEAD', dead_at = ?, lease_until = NULL, lease_owner = NULL,
		last_error = COALESCE(last_error, 'publication attempt budget exhausted')
		WHERE status = 'PENDING' AND attempts >= ?
		AND (lease_until IS NULL OR lease_until <= ?) ORDER BY id LIMIT 100`,
		now, r.config.MaxAttempts, now); err != nil {
		return nil, fmt.Errorf("retire exhausted outbox events: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, event_id, aggregate_type, aggregate_id, event_type, payload,
		attempts, available_at
		FROM outbox_events
		WHERE status = 'PENDING' AND available_at <= ?
		  AND attempts < ?
		  AND (lease_until IS NULL OR lease_until <= ?)
		ORDER BY id
		LIMIT ? FOR UPDATE SKIP LOCKED`, now, r.config.MaxAttempts, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select outbox claim candidates: %w", err)
	}
	defer rows.Close()

	events := make([]OutboxEvent, 0, limit)
	for rows.Next() {
		var event OutboxEvent
		var attempts uint32
		if err := rows.Scan(&event.ID, &event.EventID, &event.AggregateType, &event.AggregateID,
			&event.EventType, &event.Payload, &attempts, &event.AvailableAt); err != nil {
			return nil, fmt.Errorf("scan outbox claim candidate: %w", err)
		}
		event.Attempts = attempts + 1
		event.LeaseUntil = leaseUntil
		event.OwnerToken, err = newLeaseOwner()
		if err != nil {
			return nil, fmt.Errorf("generate outbox lease owner: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox claim candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close outbox claim candidates: %w", err)
	}

	for _, event := range events {
		result, err := tx.ExecContext(ctx, `UPDATE outbox_events
			SET attempts = attempts + 1, lease_until = ?, lease_owner = ?
			WHERE id = ? AND status = 'PENDING' AND attempts < ?`, leaseUntil, event.OwnerToken, event.ID, r.config.MaxAttempts)
		if err != nil {
			return nil, fmt.Errorf("lease outbox event: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return nil, fmt.Errorf("read outbox lease result: %w", err)
			}
			return nil, ErrLeaseLost
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit outbox claim: %w", err)
	}
	return events, nil
}

// MarkSent records broker acceptance. It is deliberately distinct from a
// consumer acknowledgement: consumers may still receive the event repeatedly.
func (r *OutboxRepository) MarkSent(ctx context.Context, event OutboxEvent, now time.Time) error {
	if r.db == nil {
		return errors.New("outbox database is required")
	}
	if event.OwnerToken == "" || event.Attempts == 0 {
		return ErrLeaseLost
	}
	now = now.UTC().Truncate(time.Microsecond)
	result, err := r.db.ExecContext(ctx, `UPDATE outbox_events
		SET status = 'SENT', sent_at = ?, lease_until = NULL, lease_owner = NULL, last_error = NULL
		WHERE event_id = ? AND status = 'PENDING' AND lease_until > ?
		AND lease_owner = ? AND attempts = ?`, now, event.EventID, now, event.OwnerToken, event.Attempts)
	if err != nil {
		return fmt.Errorf("mark outbox sent: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read mark outbox sent result: %w", err)
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	return nil
}

// ReleaseFailure only changes the caller's unexpired lease. Exhaustion becomes
// DEAD; otherwise capped backoff permits another attempt. A crashed publisher's
// expired lease is recovered by ClaimBatch, never by an obsolete owner.
func (r *OutboxRepository) ReleaseFailure(ctx context.Context, event OutboxEvent, now time.Time, cause error) error {
	if r.db == nil {
		return errors.New("outbox database is required")
	}
	if event.OwnerToken == "" || event.Attempts == 0 {
		return ErrLeaseLost
	}
	now = now.UTC().Truncate(time.Microsecond)
	availableAt := now.Add(BackoffForAttempt(event.Attempts, r.config.BackoffBase, r.config.BackoffMax))
	result, err := r.db.ExecContext(ctx, `UPDATE outbox_events
		SET status = IF(attempts >= ?, 'DEAD', 'PENDING'),
		dead_at = IF(attempts >= ?, ?, NULL),
		available_at = ?, lease_until = NULL, lease_owner = NULL, last_error = ?
		WHERE event_id = ? AND status = 'PENDING' AND lease_until > ?
		AND lease_owner = ? AND attempts = ?`, r.config.MaxAttempts, r.config.MaxAttempts, now,
		availableAt, truncateError(cause, maxOutboxErrorBytes), event.EventID, now, event.OwnerToken, event.Attempts)
	if err != nil {
		return fmt.Errorf("release outbox failure: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read release outbox failure result: %w", err)
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	return nil
}

func newLeaseOwner() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

// BackoffForAttempt is capped exponential backoff. Attempt one waits base;
// subsequent attempts double until max, avoiding overflow and unbounded waits.
func BackoffForAttempt(attempt uint32, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
	if attempt <= 1 {
		return base
	}
	delay := base
	for i := uint32(1); i < attempt && delay < max; i++ {
		if delay > max/2 {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}

func truncateError(err error, maxBytes int) string {
	if err == nil || maxBytes <= 0 {
		return ""
	}
	value := err.Error()
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}
