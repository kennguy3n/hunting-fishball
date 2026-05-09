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

	// OnLegacyKey, when non-nil, is invoked once per consumed message
	// whose Kafka partition key used the deprecated single-`|`
	// separator. Production wires this to a metric counter so the
	// legacy fallback in ParsePartitionKey can be removed once the
	// topic drains. The hook fires after decode succeeds; misrouted
	// or malformed legacy keys still go to the DLQ.
	OnLegacyKey func(ctx context.Context, evt IngestEvent)
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
		// Publish to DLQ first so ConsumeClaim's MarkMessage cannot
		// race with the DLQ producer (markDone is what unblocks
		// ConsumeClaim).
		c.publishDLQ(evt, err)
		c.markDone(ctx, evt, err)
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
			evt, legacyKey, err := decodeEvent(msg)
			if err != nil {
				// Malformed envelope → straight to DLQ; commit the
				// offset so we don't re-loop on the poison message.
				c.publishDLQ(IngestEvent{}, fmt.Errorf("decode: %w (raw=%q)", err, msg.Value))
				sess.MarkMessage(msg, "")

				continue
			}
			if legacyKey && c.cfg.OnLegacyKey != nil {
				c.cfg.OnLegacyKey(sess.Context(), evt)
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
// The lookup key MUST match the key partitionState.register uses; both
// include SourceID because the Kafka partition key is
// `tenant_id||source_id` (see PartitionKey) and two different sources
// under the same tenant can land on different partitions while still
// sharing a DocumentID (e.g. both have a file named `readme.md`).
// Without SourceID in the key, completing one event would falsely
// signal the other partition's pending entry and ConsumeClaim would
// commit before Stage 4 actually finished — leading to message loss
// on consumer restart.
func (c *Consumer) markDone(_ context.Context, evt IngestEvent, _ error) {
	key := pendingKey(evt)
	c.pending.Range(func(_, v any) bool {
		ps, ok := v.(*partitionState)
		if !ok {
			return true
		}
		ps.complete(key)

		return true
	})
}

// pendingKey is the lookup key shared by partitionState.register and
// Consumer.markDone. Including SourceID is required for partition
// isolation — see the comment on markDone.
func pendingKey(evt IngestEvent) string {
	return IdempotencyKey(evt.TenantID, evt.SourceID+":"+evt.DocumentID, "")
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
		Key:   sarama.StringEncoder(strings.TrimSpace(PartitionKey(evt.TenantID, evt.SourceID))),
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
	key := pendingKey(evt)
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
// wire format is JSON; the Key is parsed as `tenant_id||source_id`
// (see PartitionKey) per ARCHITECTURE.md §3.2 to derive the partition.
//
// The body's tenant_id/source_id MUST match the key — a mismatch is
// treated as a poison message so a misrouted producer can't smuggle
// an event onto another tenant's partition. When the body is missing
// tenant_id / source_id but the key is present, the key acts as a
// fallback.
//
// The second return value is true when the key used the deprecated
// single-`|` separator (see ParsePartitionKey). Callers surface this
// to ConsumerConfig.OnLegacyKey for metric counting; the value is
// never an error condition on its own.
func decodeEvent(msg *sarama.ConsumerMessage) (IngestEvent, bool, error) {
	var evt IngestEvent
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return IngestEvent{}, false, fmt.Errorf("unmarshal event: %w", err)
	}

	var legacyKey bool
	if len(msg.Key) > 0 {
		keyTenant, keySource, legacy, ok := ParsePartitionKeyDetailed(string(msg.Key))
		if !ok {
			return evt, false, fmt.Errorf("malformed partition key: %q", msg.Key)
		}
		legacyKey = legacy
		if evt.TenantID == "" {
			evt.TenantID = keyTenant
		} else if evt.TenantID != keyTenant {
			return evt, false, fmt.Errorf("tenant_id mismatch: key=%q body=%q", keyTenant, evt.TenantID)
		}
		if evt.SourceID == "" {
			evt.SourceID = keySource
		} else if evt.SourceID != keySource {
			return evt, false, fmt.Errorf("source_id mismatch: key=%q body=%q", keySource, evt.SourceID)
		}
	}

	if evt.TenantID == "" || evt.DocumentID == "" {
		return evt, legacyKey, fmt.Errorf("missing tenant_id or document_id")
	}

	return evt, legacyKey, nil
}

// PartitionKeySeparator is the on-wire separator between tenant and
// source IDs in Kafka partition keys. The doubled `||` form is the
// product spec; producers MUST emit it. The single-`|` form is a
// one-release migration aid (see ParsePartitionKey) so in-flight
// messages produced by the prior shape don't go to DLQ during a
// rolling deploy.
const PartitionKeySeparator = "||"

// legacyPartitionKeySeparator is the deprecated single-pipe form
// accepted by ParsePartitionKey on the consume path only. New
// producers always use PartitionKeySeparator.
const legacyPartitionKeySeparator = "|"

// PartitionKey returns the Kafka partition key the producer should
// stamp on each event so the sticky-assignment consumer keeps per-
// (tenant, source) ordering. Exposed for callers (e.g. connectors)
// that publish events.
func PartitionKey(tenantID, sourceID string) string {
	return tenantID + PartitionKeySeparator + sourceID
}

// ParsePartitionKey is the inverse of PartitionKey. Returns
// (tenantID, sourceID, true) on a well-formed key, or ("", "", false)
// otherwise. The doubled `||` separator is preferred; a single `|`
// is accepted as a deprecated legacy form (see
// ParsePartitionKeyDetailed for callers that need to distinguish
// the two). A missing source_id is rejected under both shapes — the
// admin source-management surface always emits both halves.
func ParsePartitionKey(key string) (string, string, bool) {
	t, s, _, ok := ParsePartitionKeyDetailed(key)
	return t, s, ok
}

// ParsePartitionKeyDetailed is ParsePartitionKey plus a `legacy`
// flag set when the key used the deprecated single-`|` separator.
// Callers (decodeEvent) forward the flag to ConsumerConfig.OnLegacyKey
// so production can record a counter and decide when to drop the
// fallback.
func ParsePartitionKeyDetailed(key string) (string, string, bool, bool) {
	if i := strings.Index(key, PartitionKeySeparator); i >= 0 {
		tenant := key[:i]
		source := key[i+len(PartitionKeySeparator):]
		if tenant == "" || source == "" {
			return "", "", false, false
		}
		return tenant, source, false, true
	}
	// Legacy single-`|` fallback. We never split on `|` if the
	// preferred `||` separator is present anywhere in the key,
	// because the strings.Index check above would have hit first.
	if i := strings.Index(key, legacyPartitionKeySeparator); i >= 0 {
		tenant := key[:i]
		source := key[i+len(legacyPartitionKeySeparator):]
		if tenant == "" || source == "" {
			return "", "", false, false
		}
		return tenant, source, true, true
	}
	return "", "", false, false
}
