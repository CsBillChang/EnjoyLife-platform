package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Pause before a checkpoint or final ledger INSERT reaches MySQL, without
// holding a table lock, to control the interleaving of two migrators.
type pauseBootstrapConnector struct {
	driver.Connector
	paused, resume chan struct{}
	once           sync.Once
	beforeFinal    bool
}

func (c *pauseBootstrapConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pauseBootstrapConn{Conn: conn, owner: c}, nil
}

type pauseBootstrapConn struct {
	driver.Conn
	owner *pauseBootstrapConnector
}

func (c *pauseBootstrapConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	pause := strings.Contains(query, "INSERT IGNORE INTO schema_migrations")
	if c.owner.beforeFinal {
		pause = strings.Contains(query, "INSERT INTO schema_migrations") && len(args) != 0 && args[0].Value == "005_admission_reservation_state.sql"
	}
	if pause {
		c.owner.once.Do(func() { close(c.owner.paused) })
		select {
		case <-c.owner.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func pausedBootstrapDB(t *testing.T, db *sql.DB, beforeFinal bool) (*sql.DB, <-chan struct{}, func()) {
	t.Helper()
	var name string
	if err := db.QueryRow(`SELECT DATABASE()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(os.Getenv("LOCAL_LIFE_MIGRATION_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	inner, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &pauseBootstrapConnector{Connector: inner, paused: make(chan struct{}), resume: make(chan struct{}), beforeFinal: beforeFinal}
	pausedDB := sql.OpenDB(wrapper)
	pausedDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pausedDB.Close() })
	var once sync.Once
	resume := func() { once.Do(func() { close(wrapper.resume) }) }
	return pausedDB, wrapper.paused, resume
}

func migrationLockWaits(ctx context.Context, db *sql.DB) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.metadata_locks l
		JOIN performance_schema.threads th ON th.THREAD_ID=l.OWNER_THREAD_ID
		WHERE l.OBJECT_TYPE='USER LEVEL LOCK' AND l.LOCK_STATUS='PENDING'
		AND th.PROCESSLIST_DB=DATABASE()`).Scan(&count)
	return count, err
}

func TestMigrationConcurrentBootstrapDoesNotReinsertCheckpointIntegration(t *testing.T) {
	db := isolatedMigrationDB(t)
	pausedDB, paused, resume := pausedBootstrapDB(t, db, false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer resume()
	directory := filepath.Join("..", "..", "..", "migrations")
	first := make(chan error, 1)
	go func() { first <- ApplyDirectory(ctx, pausedDB, directory) }()
	select {
	case <-paused:
	case err := <-first:
		t.Fatalf("first migrator ended before checkpoint: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := make(chan error, 1)
	go func() { second <- ApplyDirectory(ctx, db, directory) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	// Before the fix, B finishes while A is paused. With mutual exclusion, B
	// waits for the database lock; release A only after observing either event.
	var secondFinished bool
waitForSecond:
	for {
		select {
		case err := <-second:
			if err != nil {
				t.Fatalf("second migrator: %v", err)
			}
			secondFinished = true
			break waitForSecond
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
			waits, err := migrationLockWaits(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if waits != 0 {
				break waitForSecond
			}
		}
	}
	resume()
	for _, result := range []<-chan error{first, second} {
		if secondFinished && result == second {
			continue
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var total, checkpoints int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),SUM(name LIKE '__bootstrap__:%') FROM schema_migrations`).Scan(&total, &checkpoints); err != nil {
		t.Fatal(err)
	}
	if total != 5 || checkpoints != 0 {
		t.Fatalf("both migrators returned success, but history has %d rows and %d checkpoints", total, checkpoints)
	}
	if err := ApplyDirectory(ctx, db, directory); err != nil {
		t.Fatalf("replay after concurrent migration: %v", err)
	}
	assertNoMigrationLocks(t, db)
	t.Logf("concurrent bootstrap and replay: history=%d checkpoints=%d", total, checkpoints)
}

func assertNoMigrationLocks(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	logged := false
	for {
		var count int
		var states sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*),GROUP_CONCAT(l.LOCK_STATUS) FROM performance_schema.metadata_locks l
			JOIN performance_schema.threads th ON th.THREAD_ID=l.OWNER_THREAD_ID
			WHERE l.OBJECT_TYPE='USER LEVEL LOCK' AND th.PROCESSLIST_DB=DATABASE()`).Scan(&count, &states); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			return
		}
		// A canceled GET_LOCK closes its client socket. MySQL may still be
		// finishing the server-side 1s wait before it observes the disconnect.
		if !logged {
			t.Logf("waiting for server disconnect cleanup: locks=%d states=%s", count, states.String)
			logged = true
		}
		select {
		case <-ctx.Done():
			t.Fatalf("migration sessions retained locks: count=%d states=%s", count, states.String)
		case <-ticker.C:
		}
	}
}

func TestMigrationLockCoversFinalLedgerAndIsScopedToDatabaseIntegration(t *testing.T) {
	db := isolatedMigrationDB(t)
	otherDB := isolatedMigrationDB(t)
	pausedDB, paused, resume := pausedBootstrapDB(t, db, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer resume()
	directory := filepath.Join("..", "..", "..", "migrations")
	first := make(chan error, 1)
	go func() { first <- ApplyDirectory(ctx, pausedDB, directory) }()
	select {
	case <-paused:
	case err := <-first:
		t.Fatalf("first migrator ended before final ledger write: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := make(chan error, 1)
	go func() { second <- ApplyDirectory(ctx, db, directory) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waitForLock:
	for {
		select {
		case err := <-second:
			t.Fatalf("same database did not wait until final ledger write: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
			waits, err := migrationLockWaits(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if waits > 0 {
				break waitForLock
			}
		}
	}
	// An unrelated schema must migrate while the first schema remains locked.
	if err := ApplyDirectory(ctx, otherDB, directory); err != nil {
		t.Fatalf("independent database blocked: %v", err)
	}
	resume()
	for _, result := range []<-chan error{first, second} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertNoMigrationLocks(t, db)
	assertNoMigrationLocks(t, otherDB)
}

func TestMigrationLockCancellationDoesNotLeakIntegration(t *testing.T) {
	db := isolatedMigrationDB(t)
	pausedDB, paused, resume := pausedBootstrapDB(t, db, false)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	holderCtx, cancelHolder := context.WithCancel(ctx)
	defer cancelHolder()
	defer resume()
	directory := filepath.Join("..", "..", "..", "migrations")
	holder := make(chan error, 1)
	go func() { holder <- ApplyDirectory(holderCtx, pausedDB, directory) }()
	select {
	case <-paused:
	case err := <-holder:
		t.Fatalf("holder stopped before pause: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancelWait()
	if err := ApplyDirectory(waitCtx, db, directory); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting migrator ignored cancellation: %v", err)
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema=DATABASE() AND table_name<>'schema_migrations'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("waiting migrator applied business DDL: tables=%d err=%v", tables, err)
	}
	cancelHolder()
	select {
	case err := <-holder:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("holder cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertNoMigrationLocks(t, db)
	if err := ApplyDirectory(ctx, db, directory); err != nil {
		t.Fatalf("retry after canceled lock holder/waiter: %v", err)
	}
	assertNoMigrationLocks(t, db)
}
