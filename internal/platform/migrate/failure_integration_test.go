package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

type migrationFault string

const (
	faultAcquireError      migrationFault = "acquire-response-error"
	faultAcquireNull       migrationFault = "acquire-response-null"
	faultReleaseError      migrationFault = "release-response-error"
	faultReleaseZero       migrationFault = "release-response-zero"
	faultReleaseNull       migrationFault = "release-response-null"
	faultBodyError         migrationFault = "ddl-error"
	faultTransactionCancel migrationFault = "bootstrap-transaction-cancel"
)

var errInjectedMigrationFailure = errors.New("injected migration response failure")

type faultMigrationConnector struct {
	driver.Connector
	fault    migrationFault
	cancel   context.CancelFunc
	fired    atomic.Bool
	acquired atomic.Bool
	closed   atomic.Int64
}

func (c *faultMigrationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &faultMigrationConn{Conn: conn, owner: c}, nil
}

type faultMigrationConn struct {
	driver.Conn
	owner *faultMigrationConnector
}

func (c *faultMigrationConn) Close() error {
	c.owner.closed.Add(1)
	return c.Conn.Close()
}

// Preserve the real driver's transaction-cancellation and pool validation
// behavior; omitting these interfaces can make database/sql discard healthy
// sessions and conceal a cleanup regression.
func (c *faultMigrationConn) IsValid() bool {
	return c.Conn.(driver.Validator).IsValid()
}

func (c *faultMigrationConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *faultMigrationConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *faultMigrationConn) CheckNamedValue(value *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(value)
}

func (c *faultMigrationConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	fault := c.owner.fault
	acquireFault := fault == faultAcquireError || fault == faultAcquireNull
	releaseFault := fault == faultReleaseError || fault == faultReleaseZero || fault == faultReleaseNull
	if acquireFault && strings.Contains(query, "GET_LOCK(") && c.owner.fired.CompareAndSwap(false, true) {
		// Acquire the real server lock, then hide its successful result. The
		// physical connection must be discarded even though the client saw failure.
		rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
		if err != nil {
			return nil, err
		}
		values := make([]driver.Value, 1)
		readErr := rows.Next(values)
		closeErr := rows.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		if values[0] != int64(1) {
			return nil, errors.New("fault fixture did not acquire the real server lock")
		}
		c.owner.acquired.Store(true)
		if fault == faultAcquireError {
			return nil, errInjectedMigrationFailure
		}
		return &faultMigrationRow{}, nil
	}
	if releaseFault && strings.Contains(query, "RELEASE_LOCK(") && c.owner.fired.CompareAndSwap(false, true) {
		// Do not release the actual lock: only discarding the physical session
		// can clean it up after this ambiguous response.
		if fault == faultReleaseError {
			return nil, errInjectedMigrationFailure
		}
		if fault == faultReleaseZero {
			return &faultMigrationRow{value: int64(0)}, nil
		}
		return &faultMigrationRow{}, nil
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *faultMigrationConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.owner.fault == faultBodyError && strings.HasPrefix(strings.TrimSpace(query), "CREATE TABLE IF NOT EXISTS shops") && c.owner.fired.CompareAndSwap(false, true) {
		return nil, errInjectedMigrationFailure
	}
	if c.owner.fault == faultTransactionCancel && strings.Contains(query, "DELETE FROM schema_migrations") && c.owner.fired.CompareAndSwap(false, true) {
		c.owner.cancel()
		return nil, ctx.Err()
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

type faultMigrationRow struct {
	value driver.Value
	read  bool
}

func (r *faultMigrationRow) Columns() []string { return []string{"value"} }
func (r *faultMigrationRow) Close() error      { return nil }
func (r *faultMigrationRow) Next(values []driver.Value) error {
	if r.read {
		return io.EOF
	}
	values[0] = r.value
	r.read = true
	return nil
}

func migrationFaultDB(t *testing.T, fixture *sql.DB, fault migrationFault, cancel context.CancelFunc) (*sql.DB, *faultMigrationConnector) {
	t.Helper()
	var schema string
	if err := fixture.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(os.Getenv("LOCAL_LIFE_MIGRATION_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = schema
	cfg.ReadTimeout = 3 * time.Second
	cfg.WriteTimeout = 3 * time.Second
	// QueryContext otherwise returns ErrSkip for bound parameters, so the
	// database/sql Prepare fallback would bypass the post-acquisition injection.
	// Other cases keep the driver's normal prepared-statement behavior.
	cfg.InterpolateParams = fault == faultAcquireError || fault == faultAcquireNull
	inner, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	connector := &faultMigrationConnector{Connector: inner, fault: fault, cancel: cancel}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, connector
}

func TestMigrationFailureReleasesLocksAndRecoversPoolIntegration(t *testing.T) {
	for _, fault := range []migrationFault{faultAcquireError, faultAcquireNull, faultReleaseError, faultReleaseZero, faultReleaseNull, faultBodyError, faultTransactionCancel} {
		t.Run(string(fault), func(t *testing.T) {
			fixture := isolatedMigrationDB(t)
			ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			workCtx, cancelWork := context.WithCancel(ctx)
			defer cancelWork()
			db, connector := migrationFaultDB(t, fixture, fault, cancelWork)
			directory := filepath.Join("..", "..", "..", "migrations")
			err := ApplyDirectory(workCtx, db, directory)
			if err == nil || !connector.fired.Load() {
				t.Fatalf("fault was not exercised: fired=%v err=%v", connector.fired.Load(), err)
			}
			switch fault {
			case faultAcquireError, faultReleaseError, faultBodyError:
				if !errors.Is(err, errInjectedMigrationFailure) {
					t.Fatalf("original failure was lost: %v", err)
				}
			case faultAcquireNull:
				if !strings.Contains(err.Error(), "acquire migration lock returned NULL") {
					t.Fatalf("unexpected acquisition error: %v", err)
				}
			case faultTransactionCancel:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("transaction cancellation was lost: %v", err)
				}
			}
			stats := db.Stats()
			if stats.InUse != 0 {
				t.Fatalf("migration connection remains pinned: %+v", stats)
			}
			acquireFault := fault == faultAcquireError || fault == faultAcquireNull
			if acquireFault || fault == faultReleaseError || fault == faultReleaseZero || fault == faultReleaseNull {
				if stats.OpenConnections != 0 || connector.closed.Load() == 0 {
					t.Fatalf("ambiguous locked session returned to pool: stats=%+v physical_closes=%d", stats, connector.closed.Load())
				}
			}
			if fault == faultBodyError && (stats.Idle != 1 || connector.closed.Load() != 0) {
				t.Fatalf("healthy failed session was not reusable: stats=%+v physical_closes=%d", stats, connector.closed.Load())
			}
			assertNoMigrationLocks(t, fixture)
			if acquireFault {
				if !connector.acquired.Load() {
					t.Fatal("acquisition response injection bypassed the real GET_LOCK")
				}
				var tables int
				if err := fixture.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()`).Scan(&tables); err != nil || tables != 0 {
					t.Fatalf("failed acquisition modified schema: tables=%d err=%v", tables, err)
				}
			}
			if fault == faultTransactionCancel {
				var completed, checkpoints int
				if err := fixture.QueryRowContext(ctx, `SELECT SUM(name='001_identity_and_shops.sql'),SUM(name LIKE '__bootstrap__:%') FROM schema_migrations`).Scan(&completed, &checkpoints); err != nil || completed != 0 || checkpoints != 1 {
					t.Fatalf("metadata rollback failed: completed=%d checkpoints=%d err=%v", completed, checkpoints, err)
				}
			}
			t.Logf("fault=%s error=%v pool_open=%d pool_idle=%d pool_inuse=%d physical_closes=%d", fault, err, stats.OpenConnections, stats.Idle, stats.InUse, connector.closed.Load())
			if err := ApplyDirectory(ctx, db, directory); err != nil {
				t.Fatalf("retry after fault: %v", err)
			}
			var total, checkpoints int
			if err := fixture.QueryRowContext(ctx, `SELECT COUNT(*),SUM(name LIKE '__bootstrap__:%') FROM schema_migrations`).Scan(&total, &checkpoints); err != nil || total != 5 || checkpoints != 0 {
				t.Fatalf("history after retry: total=%d checkpoints=%d err=%v", total, checkpoints, err)
			}
			assertNoMigrationLocks(t, fixture)
			if stats := db.Stats(); stats.InUse != 0 || stats.Idle != 1 {
				t.Fatalf("pool not reusable after successful retry: %+v", stats)
			}
		})
	}
}
