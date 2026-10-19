package order_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	_ "github.com/go-sql-driver/mysql"
)

func TestMySQLPurchaseConcurrencyAndRollback(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run against the project MySQL")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}

	fixture := createPurchaseFixture(t, ctx, db, 1, 8)
	defer fixture.cleanup(t, ctx, db)
	service := order.NewMySQLService(db)
	now := time.Now().UTC()

	var successCount atomic.Int32
	var soldOutCount atomic.Int32
	type winner struct {
		userID uint64
		key    string
		order  order.Order
	}
	winnerCh := make(chan winner, 1)
	var wg sync.WaitGroup
	for index, userID := range fixture.userIDs {
		wg.Add(1)
		go func(index int, userID uint64) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-%d", index)
			item, err := service.Purchase(ctx, userID, fixture.activityID, key, now)
			switch {
			case err == nil:
				successCount.Add(1)
				winnerCh <- winner{userID: userID, key: key, order: item}
			case errors.Is(err, order.ErrSoldOut):
				soldOutCount.Add(1)
			default:
				t.Errorf("Purchase(user=%d) error = %v", userID, err)
			}
		}(index, userID)
	}
	wg.Wait()
	close(winnerCh)
	if successCount.Load() != 1 || soldOutCount.Load() != 7 {
		t.Fatalf("success=%d sold_out=%d, want 1 and 7", successCount.Load(), soldOutCount.Load())
	}
	won := <-winnerCh
	replayed, err := service.Purchase(ctx, won.userID, fixture.activityID, won.key, now.Add(time.Hour))
	if err != nil || replayed.ID != won.order.ID {
		t.Fatalf("idempotent replay = %+v, %v; want order %d", replayed, err, won.order.ID)
	}
	if _, err := service.Purchase(ctx, won.userID, fixture.activityID, "different-key", now); !errors.Is(err, order.ErrAlreadyPurchased) {
		t.Fatalf("repeat purchase error = %v, want ErrAlreadyPurchased", err)
	}
	assertPurchaseInvariants(t, ctx, db, fixture.activityID, 0, 1, 1)

	rollbackFixture := createPurchaseFixture(t, ctx, db, 1, 0)
	defer rollbackFixture.cleanup(t, ctx, db)
	if _, err := service.Purchase(ctx, ^uint64(0), rollbackFixture.activityID, "foreign-key-rollback", now); err == nil {
		t.Fatal("purchase with missing user unexpectedly succeeded")
	}
	assertPurchaseInvariants(t, ctx, db, rollbackFixture.activityID, 1, 0, 0)
}

type purchaseFixture struct {
	shopID     uint64
	activityID uint64
	userIDs    []uint64
}

func createPurchaseFixture(t *testing.T, ctx context.Context, db *sql.DB, stock, users int) purchaseFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	shopResult, err := db.ExecContext(ctx, `INSERT INTO shops (name, address, status) VALUES (?, 'integration-test', 'OPEN')`, fmt.Sprintf("integration-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	shopID, _ := shopResult.LastInsertId()
	activityResult, err := db.ExecContext(ctx, `INSERT INTO coupon_activities
		(shop_id, title, price_cents, currency, starts_at, ends_at, voucher_valid_until,
		total_stock, available_stock, admission_state, generation)
		VALUES (?, 'integration', 100, 'CNY', ?, ?, ?, ?, ?, 'OPEN', 1)`,
		shopID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), time.Now().Add(24*time.Hour), stock, stock)
	if err != nil {
		t.Fatal(err)
	}
	activityID, _ := activityResult.LastInsertId()
	fixture := purchaseFixture{shopID: uint64(shopID), activityID: uint64(activityID)}
	for index := 0; index < users; index++ {
		result, err := db.ExecContext(ctx, `INSERT INTO users (username, password_hash, role) VALUES (?, 'test-only', 'CUSTOMER')`, fmt.Sprintf("integration-%d-%d", suffix, index))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		fixture.userIDs = append(fixture.userIDs, uint64(id))
	}
	return fixture
}

func (f purchaseFixture) cleanup(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, _ = db.ExecContext(ctx, `DELETE FROM inventory_movements WHERE activity_id = ?`, f.activityID)
	_, _ = db.ExecContext(ctx, `DELETE FROM orders WHERE activity_id = ?`, f.activityID)
	_, _ = db.ExecContext(ctx, `DELETE FROM coupon_activities WHERE id = ?`, f.activityID)
	for _, userID := range f.userIDs {
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM shops WHERE id = ?`, f.shopID)
}

func assertPurchaseInvariants(t *testing.T, ctx context.Context, db *sql.DB, activityID uint64, wantStock, wantOrders, wantDeducts int) {
	t.Helper()
	var stock, orders, deducts int
	if err := db.QueryRowContext(ctx, `SELECT available_stock FROM coupon_activities WHERE id = ?`, activityID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE activity_id = ?`, activityID).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE activity_id = ? AND kind = 'DEDUCT'`, activityID).Scan(&deducts); err != nil {
		t.Fatal(err)
	}
	if stock != wantStock || orders != wantOrders || deducts != wantDeducts {
		t.Fatalf("stock=%d orders=%d deducts=%d, want %d/%d/%d", stock, orders, deducts, wantStock, wantOrders, wantDeducts)
	}
}
