package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// An explicit administrative test DSN is required: only schemas created by this
// helper are dropped. The application's configured database is never modified.
func isolatedMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LOCAL_LIFE_MIGRATION_TEST_DSN")
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" || dsn == "" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 and LOCAL_LIFE_MIGRATION_TEST_DSN for disposable-schema migration tests")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("huixiang_migration_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("drop owned test schema: %v", err)
		}
	})
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrationFreshSchemaAndReplayIntegration(t *testing.T) {
	db := isolatedMigrationDB(t)
	ctx := context.Background()
	directory := filepath.Join("..", "..", "..", "migrations")
	if err := ApplyDirectory(ctx, db, directory); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO shops (id,name,address,status) VALUES (1,'retained','fixture','OPEN')`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDirectory(ctx, db, directory); err != nil {
		t.Fatalf("replaying migrations: %v", err)
	}
	var applied, shops int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil || applied != 5 {
		t.Fatalf("migration history=%d err=%v", applied, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM shops WHERE id=1 AND name='retained'`).Scan(&shops); err != nil || shops != 1 {
		t.Fatalf("existing shop=%d err=%v", shops, err)
	}
	for _, query := range []string{
		`SELECT lease_owner,dead_at FROM outbox_events LIMIT 0`,
		`SELECT reservation_state FROM purchase_requests LIMIT 0`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
}

func TestMigrationRejectsUntrackedSchemaIntegration(t *testing.T) {
	for _, emptyLedger := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty_ledger_%t", emptyLedger), func(t *testing.T) {
			db := isolatedMigrationDB(t)
			if _, err := db.Exec(`CREATE TABLE retained (id INT PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO retained VALUES (7)`); err != nil {
				t.Fatal(err)
			}
			if emptyLedger {
				if _, err := db.Exec(`CREATE TABLE schema_migrations (name VARCHAR(255) PRIMARY KEY)`); err != nil {
					t.Fatal(err)
				}
			}
			err := ApplyDirectory(context.Background(), db, filepath.Join("..", "..", "..", "migrations"))
			if err == nil || !strings.Contains(err.Error(), "without migration history") {
				t.Fatalf("untracked schema error=%v", err)
			}
			assertNoMigrationLocks(t, db)
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='orders'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unexpected business DDL: count=%d err=%v", count, err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM retained WHERE id=7`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("retained row count=%d err=%v", count, err)
			}
		})
	}
}

func TestMigrationRejectsDockerInitializedSchemaIntegration(t *testing.T) {
	db := isolatedMigrationDB(t)
	directory := filepath.Join("..", "..", "..", "migrations")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the old image-entrypoint path: execute every SQL file, without
	// a schema_migrations ledger. These fixtures contain simple SQL statements.
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(body), ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	err = ApplyDirectory(context.Background(), db, directory)
	if err == nil || !strings.Contains(err.Error(), "without migration history") {
		t.Fatalf("Docker-initialized schema error=%v", err)
	}
	assertNoMigrationLocks(t, db)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='schema_migrations'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unverified migration ledger created: count=%d err=%v", count, err)
	}
}

func TestMigrationMissingDirectoryDoesNotCreateHistoryIntegration(t *testing.T) {
	db := isolatedMigrationDB(t)
	err := ApplyDirectory(context.Background(), db, filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.Contains(err.Error(), "read migration directory") {
		t.Fatalf("missing directory error=%v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected DDL after missing directory: count=%d err=%v", count, err)
	}
}

var errMigrationInterrupted = errors.New("injected migration interruption")

type interruptMigrationConnector struct {
	driver.Connector
	beforeLedger bool
	interrupted  atomic.Bool
}

func (c *interruptMigrationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &interruptMigrationConn{Conn: conn, owner: c}, nil
}

type interruptMigrationConn struct {
	driver.Conn
	owner *interruptMigrationConnector
}

func (c *interruptMigrationConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	stop := strings.HasPrefix(strings.TrimSpace(query), "CREATE TABLE IF NOT EXISTS shops")
	if c.owner.beforeLedger {
		stop = strings.Contains(query, "INSERT INTO schema_migrations") && len(args) != 0 && args[0].Value == "001_identity_and_shops.sql"
	}
	if stop && c.owner.interrupted.CompareAndSwap(false, true) {
		return nil, errMigrationInterrupted
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func interruptedMigrationDB(t *testing.T, db *sql.DB, beforeLedger bool) *sql.DB {
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
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := sql.OpenDB(&interruptMigrationConnector{Connector: connector, beforeLedger: beforeLedger})
	interrupted.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = interrupted.Close() })
	return interrupted
}

func TestMigrationResumesOwnedBootstrapInterruptionIntegration(t *testing.T) {
	for _, beforeLedger := range []bool{false, true} {
		t.Run(fmt.Sprintf("before_ledger_%t", beforeLedger), func(t *testing.T) {
			db := isolatedMigrationDB(t)
			directory := filepath.Join("..", "..", "..", "migrations")
			err := ApplyDirectory(context.Background(), interruptedMigrationDB(t, db, beforeLedger), directory)
			if !errors.Is(err, errMigrationInterrupted) {
				t.Fatalf("interruption error=%v", err)
			}
			assertNoMigrationLocks(t, db)
			var applied int
			if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name='001_identity_and_shops.sql'`).Scan(&applied); err != nil || applied != 0 {
				t.Fatalf("incomplete migration recorded as applied=%d err=%v", applied, err)
			}
			if _, err := db.Exec(`INSERT INTO users (username,password_hash,role) VALUES ('retained','fixture','CUSTOMER')`); err != nil {
				t.Fatal(err)
			}
			if err := ApplyDirectory(context.Background(), db, directory); err != nil {
				t.Fatalf("resume tool-owned initialization: %v", err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil || applied != 5 {
				t.Fatalf("completed history count=%d err=%v", applied, err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE username='retained'`).Scan(&applied); err != nil || applied != 1 {
				t.Fatalf("retained user count=%d err=%v", applied, err)
			}
		})
	}
}

func TestMigrationBootstrapSourceChangeRemainsClosedIntegration(t *testing.T) {
	for _, change := range []string{"fingerprint", "non-idempotent"} {
		t.Run(change, func(t *testing.T) {
			db := isolatedMigrationDB(t)
			original := filepath.Join("..", "..", "..", "migrations")
			err := ApplyDirectory(context.Background(), interruptedMigrationDB(t, db, false), original)
			if !errors.Is(err, errMigrationInterrupted) {
				t.Fatalf("interruption error=%v", err)
			}
			changed := t.TempDir()
			entries, err := os.ReadDir(original)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				body, err := os.ReadFile(filepath.Join(original, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if entry.Name() == "001_identity_and_shops.sql" {
					if change == "fingerprint" {
						body = append(body, '\n')
					} else {
						body = []byte(strings.Replace(string(body), "CREATE TABLE IF NOT EXISTS shops", "CREATE TABLE shops", 1))
					}
				}
				if err := os.WriteFile(filepath.Join(changed, entry.Name()), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = ApplyDirectory(context.Background(), db, changed)
			if err == nil || !strings.Contains(err.Error(), "bootstrap checkpoint does not match") {
				t.Fatalf("changed bootstrap error=%v", err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='shops'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("changed bootstrap advanced DDL: count=%d err=%v", count, err)
			}
		})
	}
}
