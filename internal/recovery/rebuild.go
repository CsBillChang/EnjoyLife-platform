// Package recovery implements administrative, fail-closed admission rebuilds.
package recovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
)

var (
	ErrInvalidRebuild    = errors.New("invalid rebuild input")
	ErrActivityNotFound  = errors.New("rebuild activity not found")
	ErrRebuildBusy       = errors.New("another rebuilder owns this activity")
	ErrRebuildFence      = errors.New("rebuild task no longer owns the frozen activity generation")
	ErrEvidenceMismatch  = errors.New("MySQL order/request evidence is inconsistent")
	ErrInventoryMismatch = errors.New("MySQL stock does not balance with inventory movements")
)

const TaskKind = "ADMISSION_REBUILD"

type Phase string

const (
	PhaseFrozen     Phase = "FROZEN"
	PhaseNormalized Phase = "NORMALIZED"
	PhaseInstalled  Phase = "INSTALLED"
	PhaseVerified   Phase = "VERIFIED"
	PhaseRedisOpen  Phase = "REDIS_OPEN"
	PhaseDone       Phase = "DONE"
)

type Task struct {
	ID         uint64 `json:"task_id"`
	ActivityID uint64 `json:"activity_id"`
	Generation uint64 `json:"generation"`
	Phase      Phase  `json:"phase"`
	Status     string `json:"status"`
	Stock      uint64 `json:"stock"`
	Owners     int    `json:"owners"`
}

type RebuildStore interface {
	SetOpen(context.Context, uint64, uint64, bool) error
	InstallRebuild(context.Context, admission.RebuildSnapshot, string) error
	VerifyRebuild(context.Context, admission.RebuildSnapshot, string) error
	PublishRebuild(context.Context, admission.RebuildSnapshot, string) error
}

type Service struct {
	db        *sql.DB
	store     RebuildStore
	failpoint func(Phase) error
}

// failpoint is optional and runs after a durable stage. An injected error leaves
// the task RUNNING and MySQL non-OPEN so Resume can finish the same generation.
func NewService(db *sql.DB, store RebuildStore, failpoint func(Phase) error) *Service {
	return &Service{db: db, store: store, failpoint: failpoint}
}

// Start durably freezes an OPEN activity and advances its generation exactly
// once. A RUNNING task for the current generation is returned unchanged.
func (s *Service) Start(ctx context.Context, activityID uint64) (Task, error) {
	return s.withLock(ctx, activityID, func(conn *sql.Conn) (Task, error) { return startTask(ctx, conn, activityID) })
}

// Rebuild starts or resumes the current RUNNING task. Use Resume(taskID) for an
// idempotent retry after a possibly successful final response was lost: a new
// Rebuild on an already OPEN, completed activity intentionally starts a new job.
func (s *Service) Rebuild(ctx context.Context, activityID uint64) (Task, error) {
	return s.withLock(ctx, activityID, func(conn *sql.Conn) (Task, error) {
		task, err := startTask(ctx, conn, activityID)
		if err != nil {
			return task, err
		}
		return s.run(ctx, conn, task)
	})
}

func (s *Service) Resume(ctx context.Context, taskID uint64) (Task, error) {
	if taskID == 0 || s.db == nil || s.store == nil {
		return Task{}, ErrInvalidRebuild
	}
	task, err := readTask(ctx, s.db, taskID)
	if err != nil {
		return task, err
	}
	return s.withLock(ctx, task.ActivityID, func(conn *sql.Conn) (Task, error) {
		task, err := readTask(ctx, conn, taskID)
		if err != nil {
			return task, err
		}
		if task.Status == "SUCCEEDED" && task.Phase == PhaseDone {
			return task, nil
		}
		return s.run(ctx, conn, task)
	})
}

func (s *Service) withLock(ctx context.Context, activityID uint64, run func(*sql.Conn) (Task, error)) (Task, error) {
	if activityID == 0 || s.db == nil || s.store == nil {
		return Task{}, ErrInvalidRebuild
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Task{}, fmt.Errorf("acquire rebuild connection: %w", err)
	}
	defer conn.Close()
	var database string
	if err := conn.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database); err != nil {
		return Task{}, err
	}
	digest := sha256.Sum256([]byte(database))
	name := fmt.Sprintf("ll:rebuild:%x:%d", digest[:8], activityID)
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 0)`, name).Scan(&acquired); err != nil {
		return Task{}, fmt.Errorf("lock rebuild session: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return Task{}, ErrRebuildBusy
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		var released sql.NullInt64
		if err := conn.QueryRowContext(cleanup, `SELECT RELEASE_LOCK(?)`, name).Scan(&released); err != nil || !released.Valid || released.Int64 != 1 {
			// Do not return an ambiguously locked session to the connection pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return run(conn)
}

func (s *Service) run(ctx context.Context, conn *sql.Conn, task Task) (Task, error) {
	if task.Status != "RUNNING" || phaseRank(task.Phase) == 0 {
		return task, ErrRebuildFence
	}
	if task.Phase == PhaseFrozen {
		if err := s.trip(PhaseFrozen); err != nil {
			return task, err
		}
	}
	// Old Redis is a retired namespace. Missing or partial old data need not be
	// repaired: no old request can pass the new MySQL generation fence.
	if task.Generation > 1 {
		err := s.store.SetOpen(ctx, task.ActivityID, task.Generation-1, false)
		if err != nil && !errors.Is(err, admission.ErrStateMissing) {
			return task, err
		}
	}
	snapshot, err := normalizeAndSnapshot(ctx, conn, task)
	if err != nil {
		return task, err
	}
	task.Stock, task.Owners = snapshot.StockHint, len(snapshot.Owners)
	if task, err = s.checkpoint(ctx, conn, task, PhaseNormalized); err != nil {
		return task, err
	}
	task, err = s.publish(ctx, task, snapshot, func(current Task, phase Phase) (Task, error) {
		return s.checkpoint(ctx, conn, current, phase)
	})
	if err != nil {
		return task, err
	}
	return finishTask(ctx, conn, task)
}

func (s *Service) publish(ctx context.Context, task Task, snapshot admission.RebuildSnapshot, checkpoint func(Task, Phase) (Task, error)) (Task, error) {
	var err error
	token := rebuildToken(task)
	if err := s.store.InstallRebuild(ctx, snapshot, token); err != nil {
		return task, err
	}
	if task, err = checkpoint(task, PhaseInstalled); err != nil {
		return task, err
	}
	if err := s.store.VerifyRebuild(ctx, snapshot, token); err != nil {
		return task, err
	}
	if task, err = checkpoint(task, PhaseVerified); err != nil {
		return task, err
	}
	// Redis is complete before it opens. MySQL is still VERIFIED, so Receive,
	// Process and stock RELEASE cannot commit. No request in the new generation
	// exists yet; old request IDs use only old Redis keys. Thus this interval is
	// closed to normal traffic even though the Redis gate is already open.
	if err := s.store.PublishRebuild(ctx, snapshot, token); err != nil {
		return task, err
	}
	if task, err = checkpoint(task, PhaseRedisOpen); err != nil {
		return task, err
	}
	return task, nil
}

func (s *Service) checkpoint(ctx context.Context, conn *sql.Conn, task Task, phase Phase) (Task, error) {
	if phaseRank(phase) <= phaseRank(task.Phase) {
		return task, nil
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return task, err
	}
	defer tx.Rollback()
	if _, _, err := lockFence(ctx, tx, task); err != nil {
		return task, err
	}
	task.Phase = phase
	payload, err := json.Marshal(task)
	if err != nil {
		return task, err
	}
	state := "BUILDING"
	if phaseRank(phase) >= phaseRank(PhaseVerified) {
		state = "VERIFIED"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE coupon_activities SET admission_state = ? WHERE id = ? AND generation = ?`, state, task.ActivityID, task.Generation); err != nil {
		return task, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_tasks SET payload = ?, last_error = NULL WHERE id = ? AND status = 'RUNNING' AND generation = ?`, payload, task.ID, task.Generation); err != nil {
		return task, err
	}
	if err := tx.Commit(); err != nil {
		return task, err
	}
	return task, s.trip(phase)
}

func (s *Service) trip(phase Phase) error {
	if s.failpoint != nil {
		return s.failpoint(phase)
	}
	return nil
}
func rebuildToken(task Task) string { return fmt.Sprintf("rebuild:%d:%d", task.ID, task.Generation) }
func phaseRank(phase Phase) int {
	switch phase {
	case PhaseFrozen:
		return 1
	case PhaseNormalized:
		return 2
	case PhaseInstalled:
		return 3
	case PhaseVerified:
		return 4
	case PhaseRedisOpen:
		return 5
	case PhaseDone:
		return 6
	}
	return 0
}

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readTask(ctx context.Context, db rowReader, id uint64) (Task, error) {
	var task Task
	var payload []byte
	var generation uint64
	var status string
	var source string
	if err := db.QueryRowContext(ctx, `SELECT generation, status, payload, source_id FROM recovery_tasks WHERE id = ? AND kind = ?`, id, TaskKind).Scan(&generation, &status, &payload, &source); err != nil {
		return task, err
	}
	if err := json.Unmarshal(payload, &task); err != nil {
		return task, fmt.Errorf("decode rebuild task: %w", err)
	}
	if task.ActivityID == 0 || task.Generation != generation || source != taskSource(task.ActivityID, generation) || phaseRank(task.Phase) == 0 {
		return task, ErrRebuildFence
	}
	task.ID, task.Status = id, status
	return task, nil
}

func startTask(ctx context.Context, conn *sql.Conn, activityID uint64) (Task, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	var state string
	var generation uint64
	err = tx.QueryRowContext(ctx, `SELECT admission_state, generation FROM coupon_activities WHERE id = ? FOR UPDATE`, activityID).Scan(&state, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrActivityNotFound
	}
	if err != nil {
		return Task{}, err
	}
	var taskID uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM recovery_tasks WHERE kind = ? AND source_id = ? AND generation = ? AND status = 'RUNNING' FOR UPDATE`, TaskKind, taskSource(activityID, generation), generation).Scan(&taskID)
	if err == nil {
		task, err := readTask(ctx, tx, taskID)
		if err != nil {
			return task, err
		}
		if state == "OPEN" {
			return task, ErrRebuildFence
		}
		return task, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Task{}, err
	}
	if state != "OPEN" || generation == math.MaxUint64 {
		return Task{}, ErrRebuildFence
	}
	task := Task{ActivityID: activityID, Generation: generation + 1, Phase: PhaseFrozen, Status: "RUNNING"}
	payload, err := json.Marshal(task)
	if err != nil {
		return task, err
	}
	now := time.Now().UTC()
	insert, err := tx.ExecContext(ctx, `INSERT INTO recovery_tasks (kind,source_id,generation,status,available_at,payload,created_at) VALUES (?, ?, ?, 'RUNNING', ?, ?, ?)`, TaskKind, taskSource(activityID, task.Generation), task.Generation, now, payload, now)
	if err != nil {
		return task, err
	}
	id, err := insert.LastInsertId()
	if err != nil || id <= 0 {
		return task, ErrEvidenceMismatch
	}
	task.ID = uint64(id)
	if _, err := tx.ExecContext(ctx, `UPDATE coupon_activities SET admission_state = 'FROZEN', generation = ? WHERE id = ?`, task.Generation, activityID); err != nil {
		return task, err
	}
	if err := tx.Commit(); err != nil {
		return task, err
	}
	return task, nil
}

func taskSource(activityID, generation uint64) string {
	return strconv.FormatUint(activityID, 10) + ":" + strconv.FormatUint(generation, 10)
}

func lockFence(ctx context.Context, tx *sql.Tx, task Task) (uint64, uint64, error) {
	var state string
	var generation, stock, total uint64
	if err := tx.QueryRowContext(ctx, `SELECT admission_state,generation,available_stock,total_stock FROM coupon_activities WHERE id = ? FOR UPDATE`, task.ActivityID).Scan(&state, &generation, &stock, &total); err != nil {
		return 0, 0, err
	}
	if state == "OPEN" || generation != task.Generation || stock > total {
		return 0, 0, ErrRebuildFence
	}
	var status string
	var target uint64
	if err := tx.QueryRowContext(ctx, `SELECT status,generation FROM recovery_tasks WHERE id = ? AND kind = ? FOR UPDATE`, task.ID, TaskKind).Scan(&status, &target); err != nil {
		return 0, 0, err
	}
	if status != "RUNNING" || target != generation {
		return 0, 0, ErrRebuildFence
	}
	return stock, total, nil
}

func normalizeAndSnapshot(ctx context.Context, conn *sql.Conn, task Task) (admission.RebuildSnapshot, error) {
	snapshot := admission.RebuildSnapshot{ActivityID: task.ActivityID, Generation: task.Generation}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback()
	stock, total, err := lockFence(ctx, tx, task)
	if err != nil {
		return snapshot, err
	}
	// Compute in MySQL DECIMAL rather than Go unsigned arithmetic: a corrupt
	// negative balance or a large unsigned total must not wrap into valid stock.
	var balanced bool
	if err := tx.QueryRowContext(ctx, `SELECT CAST(? AS DECIMAL(65,0)) + COALESCE(SUM(delta), 0) = CAST(? AS DECIMAL(65,0))
		FROM inventory_movements WHERE activity_id = ?`, total, stock, task.ActivityID).Scan(&balanced); err != nil {
		return snapshot, err
	}
	if !balanced {
		return snapshot, ErrInventoryMismatch
	}
	snapshot.StockHint = stock
	if err := tx.QueryRowContext(ctx, `SELECT starts_at, ends_at FROM coupon_activities WHERE id = ?`, task.ActivityID).Scan(&snapshot.StartsAt, &snapshot.EndsAt); err != nil {
		return snapshot, err
	}
	// Orders are the authority, including CLOSED and REFUNDED orders. A
	// contradictory snapshot is not silently rewritten or used to open Redis.
	var inconsistent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM purchase_requests p LEFT JOIN orders o ON o.request_id = p.request_id
		WHERE p.activity_id = ? AND (p.generation >= ? OR
		(o.id IS NOT NULL AND (o.user_id <> p.user_id OR o.activity_id <> p.activity_id OR o.shop_id <> p.shop_id OR o.price_cents <> p.price_cents OR o.currency <> p.currency OR o.idempotency_key <> p.idempotency_key)) OR
		(p.status = 'SUCCEEDED' AND o.id IS NULL) OR (p.order_id IS NOT NULL AND (o.id IS NULL OR p.order_id <> o.id)))`, task.ActivityID, task.Generation).Scan(&inconsistent); err != nil {
		return snapshot, err
	}
	if inconsistent != 0 {
		return snapshot, ErrEvidenceMismatch
	}
	if _, err := tx.ExecContext(ctx, `UPDATE purchase_requests p JOIN orders o ON o.request_id = p.request_id
		SET p.status = 'SUCCEEDED', p.order_id = o.id, p.error_code = NULL
		WHERE p.activity_id = ? AND p.generation < ?`, task.ActivityID, task.Generation); err != nil {
		return snapshot, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE purchase_requests p SET p.status = 'FAILED', p.error_code = 'ACTIVITY_GENERATION_CHANGED', p.order_id = NULL
		WHERE p.activity_id = ? AND p.generation < ? AND p.status IN ('RECEIVED','ADMITTED')
		AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.request_id = p.request_id)`, task.ActivityID, task.Generation); err != nil {
		return snapshot, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id,request_id FROM orders WHERE activity_id = ? ORDER BY user_id LIMIT ?`, task.ActivityID, admission.AtomicRebuildOwnerLimit+1)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var owner admission.CommittedOwner
		if err := rows.Scan(&owner.UserID, &owner.RequestID); err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Owners = append(snapshot.Owners, owner)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return snapshot, err
	}
	if len(snapshot.Owners) > admission.AtomicRebuildOwnerLimit {
		return snapshot, admission.ErrSnapshotTooLarge
	}
	if err := tx.Commit(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func finishTask(ctx context.Context, conn *sql.Conn, task Task) (Task, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return task, err
	}
	defer tx.Rollback()
	stock, _, err := lockFence(ctx, tx, task)
	if err != nil {
		return task, err
	}
	if stock != task.Stock || task.Phase != PhaseRedisOpen {
		return task, ErrRebuildFence
	}
	var orders, live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE activity_id = ?`, task.ActivityID).Scan(&orders); err != nil {
		return task, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM purchase_requests WHERE activity_id = ? AND status IN ('RECEIVED','ADMITTED')`, task.ActivityID).Scan(&live); err != nil {
		return task, err
	}
	if orders != task.Owners || live != 0 {
		return task, ErrEvidenceMismatch
	}
	// Old-generation reservation effects are absorbed by this full snapshot.
	// Do not add their releases to new stock. Retain their generation so any
	// already-running old Redis operation remains confined to the old keys.
	if _, err := tx.ExecContext(ctx, `UPDATE purchase_requests SET reservation_state = CASE WHEN status = 'SUCCEEDED' THEN 'COMMITTED' ELSE 'RELEASED' END
		WHERE activity_id = ? AND generation < ? AND status IN ('SUCCEEDED','FAILED') AND reservation_state IN ('PENDING','RESERVED')`, task.ActivityID, task.Generation); err != nil {
		return task, err
	}
	task.Phase, task.Status = PhaseDone, "SUCCEEDED"
	payload, err := json.Marshal(task)
	if err != nil {
		return task, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_tasks SET status = 'SUCCEEDED', payload = ?, last_error = NULL WHERE id = ? AND status = 'RUNNING'`, payload, task.ID); err != nil {
		return task, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE coupon_activities SET admission_state = 'OPEN' WHERE id = ? AND generation = ?`, task.ActivityID, task.Generation); err != nil {
		return task, err
	}
	if err := tx.Commit(); err != nil {
		return task, err
	}
	return task, nil
}
