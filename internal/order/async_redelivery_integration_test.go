package order_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	platformmq "github.com/CsBillChang/local-life-platform/internal/platform/mq"
	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

// This test closes a real manual-ack consumer channel after the order commit,
// without issuing Ack/Nack/Reject. The broker, not the test, must redeliver the
// original publication. Stop the normal worker: its outbox publisher/recovery
// scanner would race this intentionally controlled first processing attempt.
func TestAsyncRabbitRedeliveryAfterOrderCommitBeforeAck(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 and stop the normal worker for the commit-before-Ack experiment")
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
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	fixture := createPurchaseFixture(t, ctx, db, 1, 1)
	var requestID string
	defer func() { cleanupRedeliveryFixture(t, db, fixture, requestID) }()
	service := order.NewAsyncService(db)
	accepted, err := service.Accept(ctx, fixture.userIDs[0], fixture.activityID, "commit-before-ack", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	requestID = accepted.RequestID
	var eventID, outboxStatus string
	var payload []byte
	if err := db.QueryRowContext(ctx, `SELECT event_id, payload, status FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'PURCHASE_ACCEPTED'`, requestID).
		Scan(&eventID, &payload, &outboxStatus); err != nil {
		t.Fatal(err)
	}
	if outboxStatus != "PENDING" {
		t.Fatalf("normal worker claimed the test outbox before publication (status=%s); stop it and rerun", outboxStatus)
	}

	firstConnection, err := amqp.Dial(cfg.RabbitURL)
	if err != nil {
		t.Fatal(err)
	}
	defer firstConnection.Close()
	publishChannel, err := firstConnection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer publishChannel.Close()
	names := platformmq.RabbitPublisherConfig{
		Exchange:   "redelivery-exchange-" + requestID,
		Queue:      "redelivery-queue-" + requestID,
		RoutingKey: "test.commit-before-ack", ConfirmTimeout: 2 * time.Second,
	}
	publisher := platformmq.NewRabbitPublisher(publishChannel, names)
	if err := publisher.Setup(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := publishChannel.QueueDelete(names.Queue, false, false, false); err != nil {
			t.Errorf("delete isolated redelivery queue: %v", err)
		}
		if err := publishChannel.ExchangeDelete(names.Exchange, false, false); err != nil {
			t.Errorf("delete isolated redelivery exchange: %v", err)
		}
	}()
	firstConsumer, err := firstConnection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer firstConsumer.Close()
	if err := firstConsumer.Qos(1, 0, false); err != nil {
		t.Fatal(err)
	}
	firstDeliveries, err := firstConsumer.Consume(names.Queue, "", false, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(ctx, eventID, payload); err != nil {
		t.Fatal(err)
	}
	first := awaitRedeliveryTestMessage(t, ctx, firstDeliveries)
	if first.Redelivered || first.MessageId != eventID {
		t.Fatalf("unexpected initial delivery: redelivered=%v message_id=%s", first.Redelivered, first.MessageId)
	}
	message, err := platformmq.DecodePurchaseMessage(first.Body)
	if err != nil || message.RequestID != requestID {
		t.Fatalf("initial message request mismatch: %+v err=%v", message, err)
	}
	before, err := service.GetOwnedRequest(ctx, fixture.userIDs[0], requestID)
	if err != nil || before.Status != order.RequestAdmitted || before.OrderID != 0 {
		t.Fatalf("normal worker/recovery scanner processed fixture before controlled consumer: request=%+v err=%v; stop it and rerun", before, err)
	}
	assertPurchaseInvariants(t, ctx, db, fixture.activityID, 1, 0, 0)
	firstResult, err := service.Process(ctx, message.RequestID, time.Now())
	if err != nil || firstResult.Status != order.RequestSucceeded || firstResult.OrderID == 0 {
		t.Fatalf("initial order transaction result=%+v err=%v", firstResult, err)
	}
	// A separate query observes the committed request and inventory before the
	// first consumer channel is closed. No acknowledgement has been issued.
	committed, err := service.GetOwnedRequest(ctx, fixture.userIDs[0], requestID)
	if err != nil || committed.Status != order.RequestSucceeded || committed.OrderID != firstResult.OrderID {
		t.Fatalf("order commit not visible before consumer close: %+v err=%v", committed, err)
	}
	assertPurchaseInvariants(t, ctx, db, fixture.activityID, 0, 1, 1)
	if err := firstConsumer.Close(); err != nil {
		t.Fatalf("close unacknowledged consumer channel: %v", err)
	}

	secondConnection, err := amqp.Dial(cfg.RabbitURL)
	if err != nil {
		t.Fatal(err)
	}
	defer secondConnection.Close()
	secondConsumer, err := secondConnection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer secondConsumer.Close()
	if err := secondConsumer.Qos(1, 0, false); err != nil {
		t.Fatal(err)
	}
	secondDeliveries, err := secondConsumer.Consume(names.Queue, "", false, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := awaitRedeliveryTestMessage(t, ctx, secondDeliveries)
	if !second.Redelivered || second.MessageId != first.MessageId || string(second.Body) != string(first.Body) {
		t.Fatalf("broker did not redeliver the original message: redelivered=%v same_id=%v same_body=%v",
			second.Redelivered, second.MessageId == first.MessageId, string(second.Body) == string(first.Body))
	}
	redeliveredMessage, err := platformmq.DecodePurchaseMessage(second.Body)
	if err != nil || redeliveredMessage.RequestID != requestID {
		t.Fatalf("redelivered request mismatch: %+v err=%v", redeliveredMessage, err)
	}
	secondResult, err := service.Process(ctx, redeliveredMessage.RequestID, time.Now())
	if err != nil || secondResult.RequestID != firstResult.RequestID || secondResult.OrderID != firstResult.OrderID || secondResult.Status != order.RequestSucceeded {
		t.Fatalf("redelivery changed committed result: first=%+v second=%+v err=%v", firstResult, secondResult, err)
	}
	if err := second.Ack(false); err != nil {
		t.Fatalf("ack committed redelivery: %v", err)
	}
	// Closing after Ack provides a broker round trip. If Ack were missing the
	// message would become ready again, just as it did after the first close.
	if err := secondConsumer.Close(); err != nil {
		t.Fatal(err)
	}
	queue, err := publishChannel.QueueInspect(names.Queue)
	if err != nil || queue.Messages != 0 {
		t.Fatalf("queue not empty after successful replay Ack: ready=%d err=%v", queue.Messages, err)
	}
	assertPurchaseInvariants(t, ctx, db, fixture.activityID, 0, 1, 1)
	var ordersForRequest, deductionsForOrder, releasesForOrder int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE request_id = ? AND id = ?`, requestID, firstResult.OrderID).Scan(&ordersForRequest); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE order_id = ? AND kind = 'DEDUCT'`, firstResult.OrderID).Scan(&deductionsForOrder); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_movements WHERE order_id = ? AND kind = 'RELEASE'`, firstResult.OrderID).Scan(&releasesForOrder); err != nil {
		t.Fatal(err)
	}
	if ordersForRequest != 1 || deductionsForOrder != 1 || releasesForOrder != 0 {
		t.Fatalf("redelivery duplicated business effects: orders=%d deductions=%d releases=%d", ordersForRequest, deductionsForOrder, releasesForOrder)
	}
	t.Logf("real broker redelivery after committed order: redelivered=%v same_request=true same_order=true orders=1 DEDUCT=1 RELEASE=0 available_stock=0", second.Redelivered)
}

func awaitRedeliveryTestMessage(t *testing.T, ctx context.Context, deliveries <-chan amqp.Delivery) amqp.Delivery {
	t.Helper()
	select {
	case delivery, ok := <-deliveries:
		if !ok {
			t.Fatal("consumer delivery channel closed before expected message")
		}
		return delivery
	case <-ctx.Done():
		t.Fatalf("waiting for broker delivery: %v", ctx.Err())
		return amqp.Delivery{}
	}
}

func cleanupRedeliveryFixture(t *testing.T, db *sql.DB, fixture purchaseFixture, requestID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, statement := range []struct {
		query string
		arg   any
	}{
		{`DELETE FROM outbox_events WHERE aggregate_id = ?`, requestID},
		{`DELETE FROM purchase_requests WHERE request_id = ?`, requestID},
		{`DELETE FROM inventory_movements WHERE activity_id = ?`, fixture.activityID},
		{`DELETE FROM orders WHERE activity_id = ?`, fixture.activityID},
		{`DELETE FROM coupon_activities WHERE id = ?`, fixture.activityID},
		{`DELETE FROM users WHERE id = ?`, fixture.userIDs[0]},
		{`DELETE FROM shops WHERE id = ?`, fixture.shopID},
	} {
		if _, err := db.ExecContext(ctx, statement.query, statement.arg); err != nil {
			t.Errorf("cleanup isolated redelivery fixture: %v", err)
		}
	}
}
