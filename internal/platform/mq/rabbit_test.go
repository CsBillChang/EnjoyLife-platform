package mq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestPurchaseMessageEncodeDecode(t *testing.T) {
	payload, err := EncodePurchaseMessage(PurchaseMessage{RequestID: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodePurchaseMessage(payload)
	if err != nil || message.RequestID != "request-1" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

type fakeRabbitChannel struct {
	confirms     chan amqp.Confirmation
	returns      chan amqp.Return
	next         uint64
	publications int
	onPublish    func(uint64, amqp.Publishing)
	publishError error
}

func (f *fakeRabbitChannel) ExchangeDeclare(string, string, bool, bool, bool, bool, amqp.Table) error {
	return nil
}
func (f *fakeRabbitChannel) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}
func (f *fakeRabbitChannel) QueueBind(string, string, string, bool, amqp.Table) error { return nil }
func (f *fakeRabbitChannel) Confirm(bool) error                                       { return nil }
func (f *fakeRabbitChannel) NotifyPublish(c chan amqp.Confirmation) chan amqp.Confirmation {
	f.confirms = c
	return c
}
func (f *fakeRabbitChannel) NotifyReturn(c chan amqp.Return) chan amqp.Return {
	f.returns = c
	return c
}
func (f *fakeRabbitChannel) GetNextPublishSeqNo() uint64 { return f.next }
func (f *fakeRabbitChannel) PublishWithContext(_ context.Context, _, _ string, mandatory, immediate bool, msg amqp.Publishing) error {
	if !mandatory || immediate || msg.DeliveryMode != amqp.Persistent {
		return errors.New("incorrect routing or persistence options")
	}
	sequence := f.next
	f.next++
	f.publications++
	if f.publishError != nil {
		return f.publishError
	}
	if f.onPublish != nil {
		f.onPublish(sequence, msg)
	}
	return nil
}

func testRabbitPublisher(t *testing.T, timeout time.Duration) (*RabbitPublisher, *fakeRabbitChannel) {
	t.Helper()
	channel := &fakeRabbitChannel{next: 1}
	publisher := NewRabbitPublisher(channel, RabbitPublisherConfig{ConfirmTimeout: timeout})
	if err := publisher.Setup(); err != nil {
		t.Fatal(err)
	}
	return publisher, channel
}

func TestPublisherReturnPreventsSuccessEvenWhenAckIsReady(t *testing.T) {
	publisher, channel := testRabbitPublisher(t, time.Second)
	channel.onPublish = func(sequence uint64, msg amqp.Publishing) {
		// Both notifications are ready before Publish waits. A select can choose
		// Ack first; it must still inspect the queued Return for this MessageId.
		channel.returns <- amqp.Return{MessageId: msg.MessageId, ReplyCode: 312}
		channel.confirms <- amqp.Confirmation{DeliveryTag: sequence, Ack: true}
	}
	for i := range 100 {
		if err := publisher.Publish(context.Background(), fmt.Sprintf("returned-%d", i), []byte(`{}`)); !errors.Is(err, ErrPublishReturned) {
			t.Fatalf("returned publication was accepted: %v", err)
		}
	}
}

func TestPublisherMatchesReturnMessageIDAndConfirmationSequence(t *testing.T) {
	publisher, channel := testRabbitPublisher(t, time.Second)
	channel.next = 17
	channel.onPublish = func(sequence uint64, _ amqp.Publishing) {
		channel.returns <- amqp.Return{MessageId: "unrelated-event", ReplyCode: 312}
		channel.confirms <- amqp.Confirmation{DeliveryTag: sequence, Ack: true}
	}
	if err := publisher.Publish(context.Background(), "current-event", []byte(`{}`)); err != nil {
		t.Fatalf("unrelated return affected current event: %v", err)
	}
	channel.onPublish = func(sequence uint64, _ amqp.Publishing) {
		channel.confirms <- amqp.Confirmation{DeliveryTag: sequence - 1, Ack: true}
	}
	if err := publisher.Publish(context.Background(), "next-event", []byte(`{}`)); !errors.Is(err, ErrPublisherUnusable) {
		t.Fatalf("wrong sequence accepted: %v", err)
	}
}

func TestPublisherTimeoutCannotConsumeLateAckForNextEvent(t *testing.T) {
	publisher, channel := testRabbitPublisher(t, 10*time.Millisecond)
	if err := publisher.Publish(context.Background(), "event-a", []byte(`{}`)); !errors.Is(err, ErrPublishConfirmLost) {
		t.Fatalf("missing confirmation error=%v", err)
	}
	channel.confirms <- amqp.Confirmation{DeliveryTag: 1, Ack: true}
	if err := publisher.Publish(context.Background(), "event-b", []byte(`{}`)); !errors.Is(err, ErrPublisherUnusable) {
		t.Fatalf("late A confirmation could accept B: %v", err)
	}
	if channel.publications != 1 {
		t.Fatalf("published after uncertain delivery: %d", channel.publications)
	}
	if err := publisher.Setup(); !errors.Is(err, ErrPublisherUnusable) {
		t.Fatalf("Setup reused uncertain channel: %v", err)
	}
	// Recovery creates a fresh channel, whose sequence starts independently.
	replacement, fresh := testRabbitPublisher(t, time.Second)
	fresh.onPublish = func(sequence uint64, _ amqp.Publishing) {
		fresh.confirms <- amqp.Confirmation{DeliveryTag: sequence, Ack: true}
	}
	if err := replacement.Publish(context.Background(), "event-b", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherNackAndClosedNotifications(t *testing.T) {
	t.Run("nack", func(t *testing.T) {
		publisher, channel := testRabbitPublisher(t, time.Second)
		channel.onPublish = func(sequence uint64, _ amqp.Publishing) {
			channel.confirms <- amqp.Confirmation{DeliveryTag: sequence, Ack: false}
		}
		if err := publisher.Publish(context.Background(), "event", []byte(`{}`)); !errors.Is(err, ErrPublishNotConfirmed) {
			t.Fatal(err)
		}
	})
	t.Run("closed", func(t *testing.T) {
		publisher, channel := testRabbitPublisher(t, time.Second)
		channel.onPublish = func(uint64, amqp.Publishing) { close(channel.confirms) }
		if err := publisher.Publish(context.Background(), "event", []byte(`{}`)); !errors.Is(err, ErrPublishConfirmLost) {
			t.Fatal(err)
		}
		if err := publisher.Publish(context.Background(), "next", []byte(`{}`)); !errors.Is(err, ErrPublisherUnusable) {
			t.Fatal(err)
		}
	})
	t.Run("send_error", func(t *testing.T) {
		publisher, channel := testRabbitPublisher(t, time.Second)
		cause := errors.New("socket closed")
		channel.publishError = cause
		if err := publisher.Publish(context.Background(), "event", []byte(`{}`)); !errors.Is(err, cause) {
			t.Fatal(err)
		}
		if err := publisher.Publish(context.Background(), "next", []byte(`{}`)); !errors.Is(err, ErrPublisherUnusable) {
			t.Fatal(err)
		}
	})
}

func TestPublisherSerializesConcurrentCalls(t *testing.T) {
	publisher, channel := testRabbitPublisher(t, time.Second)
	channel.onPublish = func(sequence uint64, _ amqp.Publishing) {
		channel.confirms <- amqp.Confirmation{DeliveryTag: sequence, Ack: true}
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := publisher.Publish(context.Background(), fmt.Sprintf("event-%d", i), []byte(`{}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if channel.publications != 16 {
		t.Fatalf("publication count=%d", channel.publications)
	}
}

func TestCanceledPublishDoesNotPoisonUnusedChannel(t *testing.T) {
	publisher, channel := testRabbitPublisher(t, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := publisher.Publish(ctx, "event", []byte(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	channel.onPublish = func(sequence uint64, _ amqp.Publishing) {
		channel.confirms <- amqp.Confirmation{DeliveryTag: sequence, Ack: true}
	}
	if err := publisher.Publish(context.Background(), "event", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPurchaseMessageRejectsMissingOrUnknownFields(t *testing.T) {
	if _, err := EncodePurchaseMessage(PurchaseMessage{}); err == nil {
		t.Fatal("empty request ID was accepted")
	}
	if _, err := DecodePurchaseMessage([]byte(`{"request_id":"request-1","user_id":7}`)); err == nil {
		t.Fatal("unknown message field was accepted")
	}
}
