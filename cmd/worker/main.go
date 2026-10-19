package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/payment"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
	platformmq "github.com/CsBillChang/local-life-platform/internal/platform/mq"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const (
	reconnectInitialDelay = 250 * time.Millisecond
	reconnectMaxDelay     = 5 * time.Second
	recoveryBatchSize     = 100
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := database.OpenMySQL(ctx, cfg.MySQLDSN)
	if err != nil {
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	service := order.NewAsyncService(db)
	redisClient := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	defer redisClient.Close()
	reservationStore, err := admission.New(redisClient, "local-life:admission")
	if err != nil {
		slog.Error("create Redis admission store", "error", err)
		os.Exit(1)
	}
	coordinator := order.NewAdmissionCoordinator(service, reservationStore)
	processor := durableRequestProcessor{orders: service, admissions: coordinator}
	fulfillment := order.NewFulfillmentService(db)
	provider := payment.NewSimulator()
	outbox := platformmq.NewOutboxRepository(db, platformmq.OutboxConfig{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runRabbitLoop(ctx, cfg, processor, outbox)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		runRecoveryScan(ctx, db, processor)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		runFulfillmentRecovery(ctx, db, fulfillment, provider, reservationStore)
	}()

	<-ctx.Done()
	// Rabbit sessions close their connection as soon as ctx is cancelled. That
	// also releases a publish blocked below the AMQP library. Do not let a
	// misbehaving dependency keep a container alive indefinitely, however.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cfg.ShutdownPeriod):
		slog.Warn("worker shutdown deadline reached")
	}
}

func runRabbitLoop(ctx context.Context, cfg config.Config, service requestProcessor, outbox *platformmq.OutboxRepository) {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if err := runRabbitSession(ctx, cfg, service, outbox); err != nil && ctx.Err() == nil {
			slog.Warn("rabbit session ended; reconnecting", "error", err, "delay", reconnectDelay(attempt))
		}
		if !waitForReconnect(ctx, reconnectDelay(attempt)) {
			return
		}
	}
}

// runRabbitSession owns one AMQP connection. Every reconnect creates fresh
// channels and runs Setup again, so both the publisher topology and consumer
// subscription are rebuilt after a broker restart.
func runRabbitSession(parent context.Context, cfg config.Config, service requestProcessor, outbox *platformmq.OutboxRepository) error {
	connection, err := amqp.Dial(cfg.RabbitURL)
	if err != nil {
		return err
	}
	sessionCtx, cancel := context.WithCancel(parent)
	defer cancel()

	// Closing the TCP connection is deliberate on SIGTERM: PublishWithContext
	// can otherwise be blocked while a broker is gone.
	closeDone := make(chan struct{})
	go func() {
		select {
		case <-parent.Done():
			_ = connection.Close()
		case <-closeDone:
		}
	}()
	defer close(closeDone)
	defer connection.Close()

	publishChannel, err := connection.Channel()
	if err != nil {
		return err
	}
	defer publishChannel.Close()
	publisher := platformmq.NewRabbitPublisher(publishChannel, platformmq.RabbitPublisherConfig{})
	if err := publisher.Setup(); err != nil {
		return err
	}

	consumerChannel, err := connection.Channel()
	if err != nil {
		return err
	}
	defer consumerChannel.Close()
	if err := consumerChannel.Qos(cfg.Prefetch, 0, false); err != nil {
		return err
	}
	deliveries, err := consumerChannel.Consume(platformmq.PurchaseQueue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}

	connectionClosed := connection.NotifyClose(make(chan *amqp.Error, 1))
	publisherClosed := publishChannel.NotifyClose(make(chan *amqp.Error, 1))
	consumerClosed := consumerChannel.NotifyClose(make(chan *amqp.Error, 1))
	outboxResult := make(chan error, 1)
	go func() { outboxResult <- runOutbox(sessionCtx, outbox, publisher) }()

	var workers sync.WaitGroup
	for range cfg.WorkerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runConsumer(sessionCtx, deliveries, service)
		}()
	}
	consumersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(consumersDone)
	}()

	var sessionErr error
	select {
	case <-parent.Done():
	case err := <-connectionClosed:
		sessionErr = rabbitCloseError("connection", err)
	case err := <-publisherClosed:
		sessionErr = rabbitCloseError("publisher channel", err)
	case err := <-consumerClosed:
		sessionErr = rabbitCloseError("consumer channel", err)
	case <-consumersDone:
		sessionErr = errors.New("rabbit consumer deliveries stopped")
	case err := <-outboxResult:
		if err != nil && !errors.Is(err, context.Canceled) {
			sessionErr = err
		}
	}
	cancel()
	// The consumer has already stopped receiving new deliveries. Any running
	// Process call sees sessionCtx cancellation and cannot Ack an uncommitted
	// transaction.
	workers.Wait()
	return sessionErr
}

func runConsumer(ctx context.Context, deliveries <-chan amqp.Delivery, service requestProcessor) {
	for {
		select {
		case <-ctx.Done():
			return
		case delivery, ok := <-deliveries:
			if !ok {
				return
			}
			message, err := platformmq.DecodePurchaseMessage(delivery.Body)
			if err != nil {
				_ = delivery.Reject(false)
				continue
			}
			if _, err := service.ProcessMessage(ctx, message.RequestID, time.Now()); err != nil {
				_ = delivery.Nack(false, false)
				continue
			}
			// Process returns nil only after its business transaction commits.
			_ = delivery.Ack(false)
		}
	}
}

// runOutbox reports a lost publisher confirmation to its session. A timed out
// confirm leaves the AMQP channel unsafe for reuse, so the session is rebuilt.
func runOutbox(ctx context.Context, repository *platformmq.OutboxRepository, publisher *platformmq.RabbitPublisher) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// ClaimBatch deliberately leases one event at a time. Drain a
			// bounded burst per wake-up so lease safety does not impose a
			// four-message-per-second publisher ceiling.
			for range 100 {
				events, err := repository.ClaimBatch(ctx, 1, time.Now())
				if err != nil || len(events) == 0 {
					break
				}
				event := events[0]
				if publishErr := publisher.Publish(ctx, event.EventID, event.Payload); publishErr != nil {
					_ = repository.ReleaseFailure(ctx, event, time.Now(), publishErr)
					// Any broker-level failure ends the session. This covers
					// uncertain confirms, closed sockets, nacks and a deleted
					// binding; reconnecting rebuilds a clean channel and topology.
					return publishErr
				}
				if markErr := repository.MarkSent(ctx, event, time.Now()); markErr != nil {
					// The broker may already hold the message. Leave the durable
					// event recoverable; a duplicate delivery is harmless because
					// Process is idempotent.
					_ = repository.ReleaseFailure(ctx, event, time.Now(), markErr)
				}
			}
		}
	}
}

type requestProcessor interface {
	ProcessMessage(context.Context, string, time.Time) (order.PurchaseRequest, error)
	Resume(context.Context, string, time.Time) (order.PurchaseRequest, error)
}

type durableRequestProcessor struct {
	orders     *order.AsyncService
	admissions *order.AdmissionCoordinator
}

func (p durableRequestProcessor) ProcessMessage(ctx context.Context, requestID string, now time.Time) (order.PurchaseRequest, error) {
	item, err := p.orders.Process(ctx, requestID, now)
	if err != nil {
		return item, err
	}
	return p.admissions.Resume(ctx, requestID, time.Now())
}

func (p durableRequestProcessor) Resume(ctx context.Context, requestID string, now time.Time) (order.PurchaseRequest, error) {
	return p.admissions.Resume(ctx, requestID, now)
}

func runRecoveryScan(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, service requestProcessor) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := recoverAdmittedRequests(ctx, db, service, recoveryBatchSize, now); err != nil && ctx.Err() == nil {
				slog.Warn("recover admitted purchase requests", "error", err)
			}
		}
	}
}

// recoverAdmittedRequests uses the monotonic primary key as a cursor. A
// permanently failing early request cannot prevent later requests in a pass.
func recoverAdmittedRequests(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, service requestProcessor, limit int, now time.Time) error {
	if limit <= 0 {
		limit = recoveryBatchSize
	}
	var afterID uint64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rows, err := db.QueryContext(ctx, `SELECT id, request_id, status FROM purchase_requests
			WHERE id > ? AND (status IN ('RECEIVED', 'ADMITTED') OR
			(status IN ('SUCCEEDED', 'FAILED') AND reservation_state IN ('PENDING', 'RESERVED')))
			ORDER BY id LIMIT ?`, afterID, limit)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var id uint64
			var requestID string
			var status order.RequestStatus
			if err := rows.Scan(&id, &requestID, &status); err != nil {
				_ = rows.Close()
				return err
			}
			count++
			afterID = id
			if status == order.RequestAdmitted {
				_, _ = service.ProcessMessage(ctx, requestID, now)
			} else {
				_, _ = service.Resume(ctx, requestID, now)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if count < limit {
			return nil
		}
	}
}

func reconnectDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return reconnectInitialDelay
	}
	delay := reconnectInitialDelay
	for i := 0; i < attempt && delay < reconnectMaxDelay; i++ {
		delay *= 2
	}
	if delay > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return delay
}

func waitForReconnect(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func rabbitCloseError(component string, err *amqp.Error) error {
	if err == nil {
		return errors.New("rabbit " + component + " closed")
	}
	return err
}
