package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const bootstrapMigration = "001_identity_and_shops.sql"
const bootstrapPrefix = "__bootstrap__:"

func ApplyDirectory(ctx context.Context, db *sql.DB, directory string) (returnErr error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read migration directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	checkpoint, err := bootstrapCheckpoint(directory, names)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve migration connection: %w", err)
	}
	defer conn.Close()
	lockName, err := acquireMigrationLock(ctx, conn)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, releaseMigrationLock(conn, lockName)) }()
	initializing, err := checkMigrationHistory(ctx, conn, checkpoint)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (name)
		) ENGINE=InnoDB`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	if initializing && checkpoint != "" {
		// A reserved checkpoint is not an applied migration. Persist ownership
		// before any business DDL, only after the empty-schema/owned-run check.
		if _, err := conn.ExecContext(ctx, `INSERT IGNORE INTO schema_migrations (name) VALUES (?)`, checkpoint); err != nil {
			return fmt.Errorf("record bootstrap ownership: %w", err)
		}
	}
	for _, name := range names {
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if count > 0 {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		for _, statement := range strings.Split(string(body), ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
		}
		if initializing && checkpoint != "" && name == bootstrapMigration {
			if err := completeBootstrap(ctx, conn, checkpoint); err != nil {
				return err
			}
		} else if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	return nil
}

// A session lock survives MySQL's implicit DDL commits. Pin the same connection
// for the history check, every DDL statement, metadata transactions and release.
func acquireMigrationLock(ctx context.Context, conn *sql.Conn) (string, error) {
	var schema sql.NullString
	var lowerCaseNames int
	if err := conn.QueryRowContext(ctx, `SELECT DATABASE(),@@lower_case_table_names`).Scan(&schema, &lowerCaseNames); err != nil {
		return "", fmt.Errorf("read migration database: %w", err)
	}
	if !schema.Valid || schema.String == "" {
		return "", fmt.Errorf("migration requires a selected database")
	}
	name := schema.String
	if lowerCaseNames != 0 {
		name = strings.ToLower(name)
	}
	// GET_LOCK names are server-wide and limited to 64 characters. Hash the
	// application namespace and database name so different databases do not block.
	lockName := fmt.Sprintf("%x", sha256.Sum256([]byte("local-life:migrate:"+name)))
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		var acquired sql.NullInt64
		// Keep each server wait below the application's 3s socket read timeout.
		if err := conn.QueryRowContext(waitCtx, `SELECT GET_LOCK(?,1)`, lockName).Scan(&acquired); err != nil {
			// The server may have granted the lock before the client saw an error.
			// Never return a possibly locked session to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			return "", fmt.Errorf("acquire migration lock: %w", err)
		}
		if !acquired.Valid {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			return "", fmt.Errorf("acquire migration lock returned NULL")
		}
		if acquired.Int64 == 1 {
			return lockName, nil
		}
	}
}

func releaseMigrationLock(conn *sql.Conn, name string) error {
	// Cancellation of migration work must not prevent releasing a healthy session.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var released sql.NullInt64
	err := conn.QueryRowContext(ctx, `SELECT RELEASE_LOCK(?)`, name).Scan(&released)
	if err == nil && released.Valid && released.Int64 == 1 {
		return nil
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil {
		return fmt.Errorf("release migration lock: %w", err)
	}
	return fmt.Errorf("release migration lock: session no longer owns the lock")
}

// Automatic replay is restricted to this project's idempotent initial CREATEs.
// A source fingerprint prevents continuing an interrupted run with edited SQL.
func bootstrapCheckpoint(directory string, names []string) (string, error) {
	if len(names) == 0 || names[0] != bootstrapMigration {
		return "", nil
	}
	body, err := os.ReadFile(filepath.Join(directory, bootstrapMigration))
	if err != nil {
		return "", fmt.Errorf("read bootstrap migration: %w", err)
	}
	for _, statement := range strings.Split(string(body), ";") {
		statement = strings.TrimSpace(statement)
		if statement != "" && !strings.HasPrefix(statement, "CREATE TABLE IF NOT EXISTS ") {
			return "", nil
		}
	}
	return fmt.Sprintf("%s%s:%x", bootstrapPrefix, bootstrapMigration, sha256.Sum256(body)), nil
}

func completeBootstrap(ctx context.Context, conn *sql.Conn, checkpoint string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bootstrap completion: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, bootstrapMigration); err != nil {
		return fmt.Errorf("record migration %s: %w", bootstrapMigration, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, checkpoint); err != nil {
		return fmt.Errorf("clear bootstrap checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bootstrap completion: %w", err)
	}
	return nil
}

// Docker's former init SQL mount applied DDL without recording its history.
// Refuse to replay non-idempotent ALTERs or invent a baseline for such a schema.
// This check deliberately runs before any DDL or migration-history writes.
func checkMigrationHistory(ctx context.Context, conn *sql.Conn, checkpoint string) (bool, error) {
	var ledgerExists int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'schema_migrations'`).Scan(&ledgerExists); err != nil {
		return false, fmt.Errorf("check migration history table: %w", err)
	}
	if ledgerExists != 0 {
		rows, err := conn.QueryContext(ctx, `SELECT name FROM schema_migrations`)
		if err != nil {
			return false, fmt.Errorf("read migration history: %w", err)
		}
		var applied, owned bool
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return false, err
			}
			if strings.HasPrefix(name, bootstrapPrefix) {
				if checkpoint == "" || name != checkpoint {
					_ = rows.Close()
					return false, fmt.Errorf("bootstrap checkpoint does not match migration source; verify the interrupted schema before proceeding")
				}
				owned = true
			} else {
				applied = true
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return false, fmt.Errorf("read migration history: %w", err)
		}
		if owned && applied {
			return false, fmt.Errorf("bootstrap checkpoint conflicts with completed migration history")
		}
		if owned || applied {
			return owned, nil
		}
	}
	var existing int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name <> 'schema_migrations'`).Scan(&existing); err != nil {
		return false, fmt.Errorf("check existing schema: %w", err)
	}
	if existing != 0 {
		return false, fmt.Errorf("database has existing tables without migration history; verify the schema and establish an explicit baseline before applying migrations (see docs/demo-and-interview.md)")
	}
	return true, nil
}
