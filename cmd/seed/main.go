package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	password := os.Getenv("LOCAL_DEMO_PASSWORD")
	if len(password) < 8 {
		slog.Error("LOCAL_DEMO_PASSWORD must contain at least 8 characters")
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	db, err := database.OpenMySQL(context.Background(), cfg.MySQLDSN)
	if err != nil {
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := seed(context.Background(), db, password); err != nil {
		slog.Error("seed demo data", "error", err)
		os.Exit(1)
	}
	slog.Info("demo data ready", "users", []string{"demo_customer", "demo_async_customer", "demo_recovery_customer", "demo_lua_customer", "demo_merchant"})
}

func seed(ctx context.Context, db *sql.DB, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed transaction: %w", err)
	}
	defer tx.Rollback()

	for _, user := range []struct {
		username string
		role     string
	}{{"demo_customer", "CUSTOMER"}, {"demo_async_customer", "CUSTOMER"}, {"demo_recovery_customer", "CUSTOMER"}, {"demo_lua_customer", "CUSTOMER"}, {"demo_merchant", "MERCHANT"}} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE password_hash = VALUES(password_hash), role = VALUES(role)`,
			user.username, string(hash), user.role); err != nil {
			return fmt.Errorf("upsert user %s: %w", user.username, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO shops (id, name, address, status) VALUES
			(1, '翠湖书店', '五华区翠湖北路 1 号', 'OPEN'),
			(2, '南屏小馆', '盘龙区南屏街 2 号', 'OPEN')
		ON DUPLICATE KEY UPDATE name = VALUES(name), address = VALUES(address), status = VALUES(status)`); err != nil {
		return fmt.Errorf("upsert shops: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO merchant_shop_members (user_id, shop_id)
		SELECT id, 1 FROM users WHERE username = 'demo_merchant'`); err != nil {
		return fmt.Errorf("assign merchant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coupon_activities (
			id, shop_id, title, price_cents, currency, starts_at, ends_at,
			voucher_valid_until, total_stock, available_stock, admission_state, generation
		) VALUES
		(
			1, 1, '50 元代金券', 3000, 'CNY',
			CURRENT_TIMESTAMP(6) - INTERVAL 1 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 7 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 30 DAY,
			100, 100, 'OPEN', 1
		),
		(
			2, 1, '异步成单演示券', 990, 'CNY',
			CURRENT_TIMESTAMP(6) - INTERVAL 1 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 7 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 30 DAY,
			5, 5, 'OPEN', 1
		),
		(
			3, 1, '消息恢复演示券', 1290, 'CNY',
			CURRENT_TIMESTAMP(6) - INTERVAL 1 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 7 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 30 DAY,
			5, 5, 'OPEN', 1
		),
		(
			4, 1, 'Lua 资格预占演示券', 1590, 'CNY',
			CURRENT_TIMESTAMP(6) - INTERVAL 1 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 7 DAY,
			CURRENT_TIMESTAMP(6) + INTERVAL 30 DAY,
			5, 5, 'OPEN', 1
		) ON DUPLICATE KEY UPDATE
			title = VALUES(title), price_cents = VALUES(price_cents), currency = VALUES(currency),
			starts_at = VALUES(starts_at), ends_at = VALUES(ends_at),
			voucher_valid_until = VALUES(voucher_valid_until)`); err != nil {
		return fmt.Errorf("upsert coupon activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed transaction: %w", err)
	}
	return nil
}
