package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
	"golang.org/x/crypto/bcrypt"
)

const (
	firstBenchmarkShopID = 1_000_001
	benchmarkShopCount   = 10_000
	seedBatchSize        = 500
	defaultUserCount     = 500
	defaultActivityID    = 200
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.OpenMySQL(ctx, cfg.MySQLDSN)
	if err != nil {
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("begin benchmark seed", "error", err)
		os.Exit(1)
	}
	defer tx.Rollback()
	for offset := 0; offset < benchmarkShopCount; offset += seedBatchSize {
		count := seedBatchSize
		if remaining := benchmarkShopCount - offset; remaining < count {
			count = remaining
		}
		var query strings.Builder
		query.WriteString("INSERT INTO shops (id, name, address, status, version) VALUES ")
		args := make([]any, 0, count*5)
		for index := 0; index < count; index++ {
			if index > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?, ?, ?, 'OPEN', 1)")
			id := firstBenchmarkShopID + offset + index
			args = append(args, id, fmt.Sprintf("benchmark-shop-%05d", offset+index+1), fmt.Sprintf("benchmark-address-%05d", offset+index+1))
		}
		query.WriteString(" ON DUPLICATE KEY UPDATE name=VALUES(name), address=VALUES(address), status='OPEN'")
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			slog.Error("seed benchmark shops", "offset", offset, "error", err)
			os.Exit(1)
		}
	}
	password := os.Getenv("BENCHMARK_PASSWORD")
	if password != "" {
		if len(password) < 12 {
			slog.Error("BENCHMARK_PASSWORD must contain at least 12 characters")
			os.Exit(2)
		}
		userCount, err := envPositive("BENCHMARK_USER_COUNT", defaultUserCount)
		if err != nil {
			slog.Error("load benchmark user count", "error", err)
			os.Exit(2)
		}
		activityID, err := envPositive("BENCHMARK_ACTIVITY_ID", defaultActivityID)
		if err != nil {
			slog.Error("load benchmark activity", "error", err)
			os.Exit(2)
		}
		if err := seedPurchaseFixture(ctx, tx, password, userCount, activityID); err != nil {
			slog.Error("seed purchase benchmark fixture", "error", err)
			os.Exit(1)
		}
		slog.Info("purchase benchmark fixture ready", "activity_id", activityID, "users", userCount)
	}
	if err := tx.Commit(); err != nil {
		slog.Error("commit benchmark seed", "error", err)
		os.Exit(1)
	}
	slog.Info("benchmark shops ready", "first_id", firstBenchmarkShopID, "count", benchmarkShopCount)
}

func seedPurchaseFixture(ctx context.Context, tx *sql.Tx, password string, userCount, activityID int) error {
	var existingFacts int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM orders WHERE activity_id = ?) +
		(SELECT COUNT(*) FROM purchase_requests WHERE activity_id = ?)`, activityID, activityID).Scan(&existingFacts); err != nil {
		return fmt.Errorf("check existing benchmark facts: %w", err)
	}
	if existingFacts != 0 {
		return fmt.Errorf("activity %d already has %d transaction facts; choose a fresh BENCHMARK_ACTIVITY_ID", activityID, existingFacts)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash benchmark password: %w", err)
	}
	for offset := 0; offset < userCount; offset += seedBatchSize {
		count := seedBatchSize
		if remaining := userCount - offset; remaining < count {
			count = remaining
		}
		var query strings.Builder
		query.WriteString("INSERT INTO users (username,password_hash,role) VALUES ")
		args := make([]any, 0, count*3)
		for index := 0; index < count; index++ {
			if index > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?,?,'CUSTOMER')")
			args = append(args, fmt.Sprintf("benchmark_customer_%d", offset+index+1), string(hash))
		}
		query.WriteString(" ON DUPLICATE KEY UPDATE password_hash=VALUES(password_hash), role='CUSTOMER'")
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			return fmt.Errorf("upsert benchmark users at %d: %w", offset, err)
		}
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO coupon_activities
		(id,shop_id,title,price_cents,currency,starts_at,ends_at,voucher_valid_until,total_stock,available_stock,admission_state,generation)
		VALUES (?,1,?,100,'CNY',?,?,?, ?,?,'OPEN',1)
		ON DUPLICATE KEY UPDATE title=VALUES(title),starts_at=VALUES(starts_at),ends_at=VALUES(ends_at),
		voucher_valid_until=VALUES(voucher_valid_until),total_stock=VALUES(total_stock),available_stock=VALUES(available_stock),admission_state='OPEN',generation=1`,
		activityID, fmt.Sprintf("async benchmark %d", activityID), now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour), userCount, userCount)
	if err != nil {
		return fmt.Errorf("upsert benchmark activity: %w", err)
	}
	return nil
}

func envPositive(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 100000 {
		return 0, fmt.Errorf("%s must be between 1 and 100000", name)
	}
	return value, nil
}
