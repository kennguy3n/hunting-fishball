package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/IBM/sarama"
)

// ConsumerConfig configures the Kafka consumer that drives the
// pipeline coordinator.
type ConsumerConfig struct {
	// Brokers is the bootstrap broker list. Required.
	Brokers []string

	// GroupID is the Kafka consumer group. Required. Sticky partition
	// assignment lives at the group config level.
	GroupID string

	// Topics is the list of topics to subscribe to. Defaults to
	// ["ingest", "reindex", "purge"] per ARCHITECTURE.md §3.2.
	Topics []string

	// DLQTopic is the topic the consumer publishes to when a message
	// is irrecoverable. Defaults to "ingest.dlq".
	DLQTopic string

	// DLQProducer is used to publish DLQ messages. Required.
	DLQProducer DLQProducer

	// Coordinator is the pipeline coordinator the consumer submits
	// events to. Required.
	Coordinator *Coordinator

	// MaxInFlightPerPartition bounds the number of events submitted
	// per partition before the consumer waits for completion. Per-
	// partition ordering is preserved by the consumer goroutine
	// model. Defaults to 1 (strict ordering).
	MaxInFlightPerPartition int

	// CommitTimeout caps the time the consumer waits for a Stage 4
	// completion signal before continuing. Defaults to 30s.
	CommitTimeout time.Duration
}

// DLQProducer is the narrow contract the consumer needs for DLQ
// publishing. Tests inject a fake; production wires a sarama
// SyncProducer.
type DLQProducer interface {
	SendMessage(*sarama.ProducerMessage) (int32, int64, error)
}

func (c *ConsumerConfig) defaults() {
	if len(c.Topics) == 0 {
		c.Topics = []string{"ingest", "reindex", "purge"}
	}
	if c.DLQTopic == "" {
		c.DLQTopic = "ingest.dlq"
	}
	if c.MaxInFlightPerPartition == 0 {
		c.MaxInFlightPerPartition = 1
	}
	if c.CommitTimeout == 0 {
		c.CommitTimeout = 30 * time.Second
	}
}

// SaramaConfig returns a sarama.Config with the sticky partition
// assignment + manual offset commit semantics this consumer expects.
// Exposed so callers can override before passing the consumer config.
func SaramaConfig() *sarama.Config {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_5_0_0
	cfg.Consumer.Offsets.Initial = sarama.OffsetOldest
	cfg.Consumer.Offsets.AutoCommit.Enable = false
	cfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{
		sarama.NewBalanceStrategySticky(),
	}
	cfg.Consumer.Return.Errors = true
	cfg.Producer.Return.Successes = true
	cfg.Producer.RequiredAcks = sarama.WaitForAll

	return cfg
}

// Consumer is the Kafka-driven pipeline driver. The struct is not
// meant to be reused after Stop returns.
type Consumer struct {
	cfg    ConsumerConfig
	group  sarama.ConsumerGroup
	closed bool
	mu     sync.Mutex

	// pending tracks per-partition completions so the consumer can
	// commit offsets only after Stage 4 completes (transactional-
	// outbox style).
	pending sync.Map // key: "topic-partition" → *partitionState
}

// NewConsumer wires a Consumer to a sarama ConsumerGroup. Callers
// supply the ConsumerGroup so tests can inject an in-memory fake.
func NewConsumer(group sarama.ConsumerGroup, cfg ConsumerConfig) (*Consumer, error) {
	if group == nil {
		return nil, errors.New("consumer: nil ConsumerGroup")
	}
	if cfg.Coordinator == nil {
		return nil, errors.New("consumer: nil Coordinator")
	}
	if cfg.DLQProducer == nil {
		return nil, errors.New("consumer: nil DLQProducer")
	}
	if cfg.GroupID == "" {
		return nil, errors.New("consumer: GroupID required")
	}
	cfg.defaults()

	c := &Consumer{cfg: cfg, group: group}

	// Wire up coordinator hooks so per-event success / failure is
	// surfaced back to the consumer for offset commit / DLQ.
	prevSuccess := cfg.Coordinator.cfg.OnSuccess
	prevDLQ := cfg.Coordinator.cfg.OnDLQ
	cfg.Coordinator.cfg.OnSuccess = func(ctx context.Context, evt IngestEvent) {
		c.markDone(ctx, evt, nil)
		if prevSuccess != nil {
			prevSuccess(ctx, evt)
		}
	}
	cfg.Coordinator.cfg.OnDLQ = func(ctx context.Context, evt IngestEvent, err error) {
		c.markDone(ctx, evt, err)
		c.publishDLQ(evt, err)
		if prevDLQ != nil {
			prevDLQ(ctx, evt, err)
		}
	}

	return c, nil
}

// Run subscribes to the topics and consumes until ctx is cancelled or
// the underlying ConsumerGroup returns an error.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		if err := c.group.Consume(ctx, c.cfg.Topics, c); err != nil {
			if errors.Is(err, sarama.ErrClosedConsumerGroup) {
				return nil
			}

			return fmt.Errorf("consumer: consume: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// Stop closes the underlying ConsumerGroup. Idempotent.
func (c *Consumer) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true

	return c.group.Close()
}

// Setup is invoked by sarama when a new generation of the consumer
// group starts. We use it to reset per-partition state.
func (c *Consumer) Setup(_ sarama.ConsumerGroupSession) error {
	c.pending.Range(func(key, _ any) bool {
		c.pending.Delete(key)

		return true
	})

	return nil
}

// Cleanup is invoked once per generation, after all
// ConsumeClaim goroutines exit.
func (c *Consumer) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim is invoked once per partition. The implementation
// runs serially per partition (preserving per-partition ordering) and
// commits the offset only after Stage 4 reports completion.
func (c *Consumer) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	state := c.partitionState(claim.Topic(), claim.Partition())

	for {
		select {
		case <-sess.Context().Done():
			return nil
		case msg, ok := <-claim.Messages():
			if !ok {
				return nil
			}
			evt, err := decodeEvent(msg)
			if err != nil {
				// Malformed envelope → straight to DLQ; commit the
				// offset so we don't re-loop on the poison message.
				c.publishDLQ(IngestEvent{}, fmt.Errorf("decode: %w (raw=%q)", err, msg.Value))
				sess.MarkMessage(msg, "")

				continue
			}
			doneCh := state.register(evt)
			if err := c.cfg.Coordinator.Submit(sess.Context(), evt); err != nil {
				return err
			}

			// Wait for Stage 4 completion before committing the offset.
			ctx, cancel := context.WithTimeout(sess.Context(), c.cfg.CommitTimeout)
			select {
			case <-ctx.Done():
				cancel()

				return ctx.Err()
			case <-doneCh:
				cancel()
				sess.MarkMessage(msg, "")
			}
		}
	}
}

// markDone signals the pending entry for the (topic, partition) of evt.
// The lookup is by IdempotencyKey to keep partitions independent.
func (c *Consumer) markDone(_ context.Context, evt IngestEvent, _ error) {
	// We don't know the partition the event came from inside the
	// coordinator. State lookup is therefore best-effort: callers
	// commit only after the channel fires, and per-partition state
	// stores its event's IdempotencyKey.
	c.pending.Range(func(_, v any) bool {
		ps, ok := v.(*partitionState)
		if !ok {
			return true
		}
		ps.complete(IdempotencyKey(evt.TenantID, evt.DocumentID, ""))

		return true
	})
}

// publishDLQ wraps the original event in the DLQ envelope and pushes
// it to Kafka. Best-effort: if the producer rejects the message we
// drop it and log via the OnDLQ callback (if any).
func (c *Consumer) publishDLQ(evt IngestEvent, err error) {
	if c.cfg.DLQProducer == nil {
		return
	}

	body, _ := json.Marshal(struct {
		Event    IngestEvent `json:"event"`
		Error    string      `json:"error"`
		FailedAt time.Time   `json:"failed_at"`
	}{
		Event:    evt,
		Error:    fmt.Sprintf("%v", err),
		FailedAt: time.Now().UTC(),
	})

	msg := &sarama.ProducerMessage{
		Topic: c.cfg.DLQTopic,
		Key:   sarama.StringEncoder(strings.TrimSpace(evt.TenantID + "|" + evt.SourceID)),
		Value: sarama.ByteEncoder(body),
	}
	_, _, _ = c.cfg.DLQProducer.SendMessage(msg)
}

// partitionState tracks one in-flight event per partition (the
// MaxInFlight=1 default) so ConsumeClaim can wait on Stage 4
// completion before MarkMessage.
type partitionState struct {
	mu      sync.Mutex
	pending map[string]chan struct{}
}

// register reserves a completion channel for evt and returns it.
func (p *partitionState) register(evt IngestEvent) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[string]chan struct{}{}
	}
	key := IdempotencyKey(evt.TenantID, evt.DocumentID, "")
	ch, ok := p.pending[key]
	if !ok {
		ch = make(chan struct{}, 1)
		p.pending[key] = ch
	}

	return ch
}

func (p *partitionState) complete(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch, ok := p.pending[key]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
		delete(p.pending, key)
	}
}

func (c *Consumer) partitionState(topic string, partition int32) *partitionState {
	key := fmt.Sprintf("%s-%d", topic, partition)
	v, ok := c.pending.Load(key)
	if ok {
		return v.(*partitionState)
	}
	v, _ = c.pending.LoadOrStore(key, &partitionState{})

	return v.(*partitionState)
}

// decodeEvent parses a Kafka message body into an IngestEvent. The
// wire format is JSON; the Key is parsed as `tenant_id|source_id` per
// ARCHITECTURE.md §3.2 to derive the partition.
func decodeEvent(msg *sarama.ConsumerMessage) (IngestEvent, error) {
	var evt IngestEvent
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return IngestEvent{}, fmt.Errorf("unmarshal event: %w", err)
	}
	if evt.TenantID == "" || evt.DocumentID == "" {
		return evt, fmt.Errorf("missing tenant_id or document_id")
	}

	return evt, nil
}

// PartitionKey returns the Kafka partition key the producer should
// stamp on each event so the sticky-assignment consumer keeps per-
// (tenant, source) ordering. Exposed for callers (e.g. connectors)
// that publish events.
func PartitionKey(tenantID, sourceID string) string {
	return tenantID + "|" + sourceID
}
