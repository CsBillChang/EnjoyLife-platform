package coupon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	_ "github.com/go-sql-driver/mysql"
)

// This is deliberately opt-in because it writes isolated fixture rows to the
// local Docker MySQL. It verifies the transaction couples authorized mutation
// and durable invalidation rather than treating the HTTP fast path as durable.
func TestUpdateOwnedWritesCouponInvalidationIntegration(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 for MySQL coupon integration")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	suffix := time.Now().UnixNano()
	result, err := db.ExecContext(ctx, `INSERT INTO users (username,password_hash,role) VALUES (?,?, 'MERCHANT')`, fmt.Sprintf("coupon-merchant-%d", suffix), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	merchantID, _ := result.LastInsertId()
	result, err = db.ExecContext(ctx, `INSERT INTO shops (name,address,status,version) VALUES (?,?, 'OPEN',1)`, fmt.Sprintf("coupon-shop-%d", suffix), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	shopID, _ := result.LastInsertId()
	_, err = db.ExecContext(ctx, `INSERT INTO merchant_shop_members (user_id,shop_id) VALUES (?,?)`, merchantID, shopID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	result, err = db.ExecContext(ctx, `INSERT INTO coupon_activities (shop_id,title,price_cents,currency,starts_at,ends_at,voucher_valid_until,total_stock,available_stock,admission_state,generation) VALUES (?,?,100,'CNY',?,?,?,?,?,'OPEN',1)`, shopID, "before", now.Add(time.Hour), now.Add(2*time.Hour), now.Add(3*time.Hour), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	activityID, _ := result.LastInsertId()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM recovery_tasks WHERE kind=? AND source_id=?`, couponCacheInvalidationKind, fmt.Sprint(activityID))
		_, _ = db.ExecContext(context.Background(), `DELETE FROM coupon_activities WHERE id=?`, activityID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM merchant_shop_members WHERE user_id=? AND shop_id=?`, merchantID, shopID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM shops WHERE id=?`, shopID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id=?`, merchantID)
	})
	title := "after"
	got, err := NewMySQLRepository(db).UpdateOwned(ctx, uint64(merchantID), uint64(activityID), UpdateInput{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "after" || got.CacheGeneration == 0 {
		t.Fatalf("updated=%+v", got)
	}
	var state string
	var generation uint64
	if err = db.QueryRowContext(ctx, `SELECT status,generation FROM recovery_tasks WHERE kind=? AND source_id=?`, couponCacheInvalidationKind, fmt.Sprint(activityID)).Scan(&state, &generation); err != nil {
		t.Fatal(err)
	}
	if state != "PENDING" || generation != got.CacheGeneration {
		t.Fatalf("task state=%s generation=%d", state, generation)
	}
}
