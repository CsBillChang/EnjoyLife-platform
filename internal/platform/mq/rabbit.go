package mq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

var (
	ErrPublishConfirmLost  = errors.New("rabbit publish confirmation was not received")
	ErrPublishNotConfirmed = errors.New("rabbit broker rejected publication")
	ErrPublishReturned     = errors.New("rabbit returned an unroutable publication")
	ErrPublisherUnusable   = errors.New("rabbit publisher channel must be replaced")
)

const (
	PurchaseExchange = "purchases"
	PurchaseQueue    = "purchase-orders"
	PurchaseRouteKey = "purchase.created"
)

// PurchaseMessage intentionally carries only the durable request identity. A
// worker must re-read the current purchase request rather than trust message
// copies of user, price, stock, or authorization facts.
type PurchaseMessage struct {
	RequestID string `json:"request_id"`
}

func EncodePurchaseMessage(message PurchaseMessage) ([]byte, error) {
	if message.RequestID == "" {
		return nil, errors.New("purchase request_id is required")
	}
	return json.Marshal(message)
}

func DecodePurchaseMessage(payload []byte) (PurchaseMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var message PurchaseMessage
	if err := decoder.Decode(&message); err != nil {
		return PurchaseMessage{}, fmt.Errorf("decode purchase message: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return PurchaseMessage{}, errors.New("purchase message must contain one JSON value")
	}
	if message.RequestID == "" {
		return PurchaseMessage{}, errors.New("purchase request_id is required")
	}
	return message, nil
}

// RabbitChannel is the small subset of amqp.Channel needed by RabbitPublisher.
type RabbitChannel interface {
	ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error
	QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error)
	QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error
	Confirm(noWait bool) error
	NotifyPublish(confirm chan amqp.Confirmation) chan amqp.Confirmation
	NotifyReturn(returns chan amqp.Return) chan amqp.Return
	GetNextPublishSeqNo() uint64
	PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
}

type RabbitPublisherConfig struct {
	Exchange       string
	Queue          string
	RoutingKey     string
	ConfirmTimeout time.Duration
}

func (c RabbitPublisherConfig) withDefaults() RabbitPublisherConfig {
	if c.Exchange == "" {
		c.Exchange = PurchaseExchange
	}
	if c.Queue == "" {
		c.Queue = PurchaseQueue
	}
	if c.RoutingKey == "" {
		c.RoutingKey = PurchaseRouteKey
	}
	if c.ConfirmTimeout <= 0 {
		c.ConfirmTimeout = 5 * time.Second
	}
	return c
}

// RabbitPublisher publishes persistent messages and waits for the broker's
// publisher confirm. A confirm is only broker acceptance, never consumer Ack.
type RabbitPublisher struct {
	channel  RabbitChannel
	config   RabbitPublisherConfig
	mu       sync.Mutex
	confirms chan amqp.Confirmation
	returns  chan amqp.Return
	unusable bool
}

func NewRabbitPublisher(channel RabbitChannel, config RabbitPublisherConfig) *RabbitPublisher {
	return &RabbitPublisher{channel: channel, config: config.withDefaults()}
}

// Setup declares a durable direct exchange and durable queue, binds them, and
// enables publisher confirms before any publication is attempted.
func (p *RabbitPublisher) Setup() error {
	if p.channel == nil {
		return errors.New("rabbit channel is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unusable {
		return ErrPublisherUnusable
	}
	if p.confirms != nil {
		return nil
	}
	if err := p.channel.ExchangeDeclare(p.config.Exchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare purchase exchange: %w", err)
	}
	if _, err := p.channel.QueueDeclare(p.config.Queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare purchase queue: %w", err)
	}
	if err := p.channel.QueueBind(p.config.Queue, p.config.RoutingKey, p.config.Exchange, false, nil); err != nil {
		return fmt.Errorf("bind purchase queue: %w", err)
	}
	if err := p.channel.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}
	p.confirms = p.channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	p.returns = p.channel.NotifyReturn(make(chan amqp.Return, 1))
	return nil
}

func (p *RabbitPublisher) PublishPurchase(ctx context.Context, eventID string, message PurchaseMessage) error {
	payload, err := EncodePurchaseMessage(message)
	if err != nil {
		return err
	}
	return p.Publish(ctx, eventID, payload)
}

// Publish exclusively owns its AMQP channel and permits one in-flight publish.
// Success requires the matching DeliveryTag's Ack and no Return for MessageId.
// An uncertain send/confirm poisons this instance: callers must create a new
// channel and publisher, never retry on the uncertain channel. This bounds the
// notification buffers and prevents late acknowledgements from crossing calls.
func (p *RabbitPublisher) Publish(ctx context.Context, eventID string, payload []byte) error {
	if eventID == "" {
		return errors.New("outbox event_id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unusable {
		return ErrPublisherUnusable
	}
	if p.channel == nil || p.confirms == nil || p.returns == nil {
		return errors.New("rabbit publisher is not set up")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, p.config.ConfirmTimeout)
	defer cancel()
	sequence := p.channel.GetNextPublishSeqNo()
	if err := p.channel.PublishWithContext(waitCtx, p.config.Exchange, p.config.RoutingKey, true, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		ContentType:  "application/json",
		MessageId:    eventID,
		Timestamp:    time.Now().UTC(),
		Body:         payload,
	}); err != nil {
		p.unusable = true
		return fmt.Errorf("publish outbox event: %w", err)
	}

	var returned error
	for {
		select {
		case result, ok := <-p.returns:
			if !ok {
				p.unusable = true
				return ErrPublishConfirmLost
			}
			if result.MessageId == eventID {
				returned = fmt.Errorf("%w: event=%s reply_code=%d", ErrPublishReturned, eventID, result.ReplyCode)
			}
		case confirmation, ok := <-p.confirms:
			if !ok {
				p.unusable = true
				return ErrPublishConfirmLost
			}
			if confirmation.DeliveryTag != sequence {
				p.unusable = true
				return fmt.Errorf("%w: expected confirmation %d, received %d", ErrPublisherUnusable, sequence, confirmation.DeliveryTag)
			}
			// Rabbit sends Return before Confirm. Both buffered channels may be
			// ready and Go select may choose Confirm first, so drain Return before
			// accepting it. No later publication can run while mu is held.
			select {
			case result, ok := <-p.returns:
				if !ok {
					p.unusable = true
					return ErrPublishConfirmLost
				}
				if result.MessageId == eventID {
					returned = fmt.Errorf("%w: event=%s reply_code=%d", ErrPublishReturned, eventID, result.ReplyCode)
				}
			default:
			}
			if returned != nil {
				return returned
			}
			if !confirmation.Ack {
				return ErrPublishNotConfirmed
			}
			return nil
		case <-waitCtx.Done():
			p.unusable = true
			return fmt.Errorf("%w: %w", ErrPublishConfirmLost, waitCtx.Err())
		}
	}
}
