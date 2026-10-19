package mq

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Use a connection-local temporary clone rather than changing the project's
// live outbox. A one-connection pool keeps every repository query on that clone.
func isolatedOutboxDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 for MySQL/RabbitMQ reliability tests")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var tableName, definition string
	if err := db.QueryRowContext(ctx, `SHOW CREATE TABLE outbox_events`).Scan(&tableName, &definition); err != nil {
		t.Fatalf("read actual outbox schema: %v", err)
	}
	const createPrefix = "CREATE TABLE `outbox_events`"
	if tableName != "outbox_events" || !strings.HasPrefix(definition, createPrefix) {
		t.Fatal("unexpected outbox schema definition")
	}
	// MySQL rejects CREATE TEMPORARY ... LIKE with identical source/target
	// names. Reuse its exact DDL with TEMPORARY to safely shadow the live table.
	definition = strings.Replace(definition, createPrefix, "CREATE TEMPORARY TABLE `outbox_events`", 1)
	if _, err := db.ExecContext(ctx, definition); err != nil {
		t.Fatalf("create isolated outbox clone: %v", err)
	}
	// This fails clearly when the owner/DEAD migration has not been applied.
	rows, err := db.QueryContext(ctx, `SELECT lease_owner, dead_at FROM outbox_events LIMIT 0`)
	if err != nil {
		t.Fatalf("outbox lease migration is required: %v", err)
	}
	rows.Close()
	return db
}

func insertOutboxFixture(t *testing.T, db *sql.DB, now time.Time, attempts uint32) string {
	t.Helper()
	id, err := newLeaseOwner()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO outbox_events
		(event_id, aggregate_type, aggregate_id, event_type, payload, attempts, available_at)
		VALUES (?, 'PURCHASE_REQUEST', ?, 'PURCHASE_ACCEPTED', JSON_OBJECT('request_id', ?), ?, ?)`,
		id, id, id, attempts, now)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMySQLOutboxLeaseOwnerFencesStalePublisher(t *testing.T) {
	db := isolatedOutboxDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := insertOutboxFixture(t, db, now, 0)
	first := NewOutboxRepository(db, OutboxConfig{LeaseDuration: time.Second})
	second := NewOutboxRepository(db, OutboxConfig{LeaseDuration: time.Second})
	claimed, err := first.ClaimBatch(ctx, 32, now)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("initial claim=%+v err=%v", claimed, err)
	}
	old := claimed[0]
	if old.EventID != id || old.OwnerToken == "" {
		t.Fatal("claim lacks event/owner identity")
	}
	reclaimedAt := now.Add(2 * time.Second)
	claimed, err = second.ClaimBatch(ctx, 32, reclaimedAt)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("reclaim=%+v err=%v", claimed, err)
	}
	current := claimed[0]
	if current.OwnerToken == old.OwnerToken || current.Attempts != 2 {
		t.Fatal("lease identity did not rotate")
	}
	if err := first.MarkSent(ctx, old, reclaimedAt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale success mutated new lease: %v", err)
	}
	if err := first.ReleaseFailure(ctx, old, reclaimedAt, errors.New("old publisher failed")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale failure mutated new lease: %v", err)
	}
	var owner, status string
	if err := db.QueryRow(`SELECT lease_owner, status FROM outbox_events WHERE event_id = ?`, id).Scan(&owner, &status); err != nil {
		t.Fatal(err)
	}
	if owner != current.OwnerToken || status != "PENDING" {
		t.Fatalf("new lease changed: owner_matches=%v status=%s", owner == current.OwnerToken, status)
	}
	if err := second.MarkSent(ctx, current, reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM outbox_events WHERE event_id = ?`, id).Scan(&status); err != nil || status != "SENT" {
		t.Fatalf("current owner could not finish: status=%s err=%v", status, err)
	}
}

func TestMySQLOutboxExhaustionDoesNotPoisonHealthyClaims(t *testing.T) {
	db := isolatedOutboxDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	exhausted := insertOutboxFixture(t, db, now, 1000)
	healthy := insertOutboxFixture(t, db, now, 0)
	repository := NewOutboxRepository(db, OutboxConfig{})
	claimed, err := repository.ClaimBatch(ctx, 32, now)
	if err != nil || len(claimed) != 1 || claimed[0].EventID != healthy {
		t.Fatalf("exhausted head blocked healthy claim: %+v err=%v", claimed, err)
	}
	var status string
	var attempts uint32
	var deadAt sql.NullTime
	if err := db.QueryRow(`SELECT status, attempts, dead_at FROM outbox_events WHERE event_id = ?`, exhausted).Scan(&status, &attempts, &deadAt); err != nil {
		t.Fatal(err)
	}
	if status != "DEAD" || attempts != 1000 || !deadAt.Valid {
		t.Fatalf("exhaustion state=%s attempts=%d dead_at_valid=%v", status, attempts, deadAt.Valid)
	}
	if err := repository.MarkSent(ctx, claimed[0], now); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLOutboxLastAttemptFailureAndCrashBecomeDead(t *testing.T) {
	for _, crash := range []bool{false, true} {
		name := "reported_failure"
		if crash {
			name = "expired_crash_lease"
		}
		t.Run(name, func(t *testing.T) {
			db := isolatedOutboxDB(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			id := insertOutboxFixture(t, db, now, 1)
			repository := NewOutboxRepository(db, OutboxConfig{MaxAttempts: 2, LeaseDuration: time.Second})
			claimed, err := repository.ClaimBatch(ctx, 32, now)
			if err != nil || len(claimed) != 1 || claimed[0].Attempts != 2 {
				t.Fatalf("last claim=%+v err=%v", claimed, err)
			}
			if crash {
				claimedAgain, err := repository.ClaimBatch(ctx, 32, now.Add(2*time.Second))
				if err != nil || len(claimedAgain) != 0 {
					t.Fatalf("exhausted crash was reclaimed: %+v err=%v", claimedAgain, err)
				}
			} else if err := repository.ReleaseFailure(ctx, claimed[0], now, errors.New("simulated broker failure")); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := db.QueryRow(`SELECT status FROM outbox_events WHERE event_id = ?`, id).Scan(&status); err != nil || status != "DEAD" {
				t.Fatalf("last attempt state=%s err=%v", status, err)
			}
		})
	}
}

func TestMySQLOutboxBackoffAndSingleEventClaims(t *testing.T) {
	db := isolatedOutboxDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	firstID := insertOutboxFixture(t, db, now, 0)
	secondID := insertOutboxFixture(t, db, now, 0)
	repository := NewOutboxRepository(db, OutboxConfig{BackoffBase: time.Second})
	claimed, err := repository.ClaimBatch(ctx, 100, now)
	if err != nil || len(claimed) != 1 || claimed[0].EventID != firstID {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	var unclaimed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE event_id = ? AND lease_until IS NULL AND lease_owner IS NULL`, secondID).Scan(&unclaimed); err != nil || unclaimed != 1 {
		t.Fatalf("batch tail was prematurely leased: %d err=%v", unclaimed, err)
	}
	if err := repository.ReleaseFailure(ctx, claimed[0], now, errors.New("first transient failure")); err != nil {
		t.Fatal(err)
	}
	claimed, err = repository.ClaimBatch(ctx, 32, now.Add(100*time.Millisecond))
	if err != nil || len(claimed) != 1 || claimed[0].EventID != secondID {
		t.Fatalf("backoff did not let next event progress: %+v err=%v", claimed, err)
	}
	if err := repository.MarkSent(ctx, claimed[0], now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	claimed, err = repository.ClaimBatch(ctx, 32, now.Add(2*time.Second))
	if err != nil || len(claimed) != 1 || claimed[0].EventID != firstID || claimed[0].Attempts != 2 {
		t.Fatalf("retry claim=%+v err=%v", claimed, err)
	}
	if err := repository.MarkSent(ctx, claimed[0], now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestRabbitMandatoryReturnAndConfirmedDelivery(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 for RabbitMQ reliability tests")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := amqp.Dial(cfg.RabbitURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	suffix, err := newLeaseOwner()
	if err != nil {
		t.Fatal(err)
	}
	configuration := RabbitPublisherConfig{Exchange: "test-purchases-" + suffix, Queue: "test-orders-" + suffix, RoutingKey: "test.purchase", ConfirmTimeout: time.Second}
	publisher := NewRabbitPublisher(channel, configuration)
	if err := publisher.Setup(); err != nil {
		t.Fatal(err)
	}
	defer channel.ExchangeDelete(configuration.Exchange, false, false)
	defer channel.QueueDelete(configuration.Queue, false, false, false)
	if err := channel.QueueUnbind(configuration.Queue, configuration.RoutingKey, configuration.Exchange, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := publisher.Publish(ctx, "unroutable-"+suffix, []byte(`{"request_id":"not-routed"}`)); !errors.Is(err, ErrPublishReturned) {
		t.Fatalf("mandatory return was treated as confirmed delivery: %v", err)
	}
	if err := channel.QueueBind(configuration.Queue, configuration.RoutingKey, configuration.Exchange, false, nil); err != nil {
		t.Fatal(err)
	}
	eventID := "routed-" + suffix
	if err := publisher.Publish(ctx, eventID, []byte(`{"request_id":"routed"}`)); err != nil {
		t.Fatal(err)
	}
	delivery, found, err := channel.Get(configuration.Queue, true)
	if err != nil || !found || delivery.MessageId != eventID {
		t.Fatalf("confirmed message missing/mismatched: found=%v id=%s err=%v", found, delivery.MessageId, err)
	}
}
