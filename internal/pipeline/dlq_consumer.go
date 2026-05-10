// Package pipeline — dlq_consumer wires a Kafka consumer onto the
// dead-letter topic that PERSISTS each envelope to Postgres so the
// admin portal can list failed events and trigger replays.
//
// This is a strictly larger contract than dlq_observer (which only
// emits log lines + a Prometheus counter): the consumer here owns
// durable storage, an attempt-count cap, and a replay path back
// into the main ingest topic.
//
// Wiring is opt-in via the CONTEXT_ENGINE_DLQ_CONSUME=1 env var so
// existing single-binary deployments don't sprout a second consumer
// group by accident.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/IBM/sarama"
	"github.com/oklog/ulid/v2"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// DLQMessage is one persisted dead-letter row. Mirrors the
// migrations/009_dlq_messages.sql schema verbatim.
type DLQMessage struct {
	ID            string     `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID      string     `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	SourceID      string     `gorm:"type:varchar(128);column:source_id" json:"source_id,omitempty"`
	DocumentID    string     `gorm:"type:varchar(256);column:document_id" json:"document_id,omitempty"`
	OriginalTopic string     `gorm:"type:varchar(128);not null;column:original_topic" json:"original_topic"`
	PartitionKey  string     `gorm:"type:text;column:partition_key" json:"partition_key,omitempty"`
	Payload       []byte     `gorm:"type:bytea;not null;column:payload" json:"payload"`
	ErrorText     string     `gorm:"type:text;not null;column:error_text" json:"error_text"`
	FailedAt      time.Time  `gorm:"not null;column:failed_at" json:"failed_at"`
	AttemptCount  int        `gorm:"not null;column:attempt_count" json:"attempt_count"`
	ReplayedAt    *time.Time `gorm:"column:replayed_at" json:"replayed_at,omitempty"`
	ReplayError   string     `gorm:"type:text;column:replay_error" json:"replay_error,omitempty"`
	CreatedAt     time.Time  `gorm:"not null;column:created_at" json:"created_at"`
}

// TableName overrides the GORM default pluralisation.
func (DLQMessage) TableName() string { return "dlq_messages" }

// DLQStore is the storage seam the consumer + admin handler share.
// Production wires a *gorm.DB-backed implementation; tests inject an
// in-memory fake.
type DLQStore interface {
	Insert(ctx context.Context, msg *DLQMessage) error
	List(ctx context.Context, filter DLQListFilter) ([]DLQMessage, error)
	Get(ctx context.Context, tenantID, id string) (*DLQMessage, error)
	MarkReplayed(ctx context.Context, id string, replayErr error) error
	BumpAttemptCount(ctx context.Context, id string) (int, error)
}

// DLQListFilter narrows the GET /v1/admin/dlq query.
type DLQListFilter struct {
	TenantID        string
	OriginalTopic   string
	IncludeReplayed bool
	PageSize        int
	PageToken       string
}

// DLQConsumerConfig configures a DLQConsumer.
type DLQConsumerConfig struct {
	// Group is the Kafka consumer group. Required.
	Group sarama.ConsumerGroup

	// Topic is the dead-letter topic to consume. Required.
	Topic string

	// Store is the persistence backend. Required.
	Store DLQStore

	// Logger is the slog handle. Required.
	Logger *slog.Logger

	// MaxAttempts caps how many replays a single dead-letter row can
	// undergo before the admin handler refuses further replays. The
	// guard prevents an infinite-loop ping-pong between the main
	// topic and the DLQ. Defaults to 5.
	MaxAttempts int
}

// DLQConsumer subscribes to the dead-letter topic and persists each
// envelope into the configured DLQStore.
type DLQConsumer struct {
	cfg DLQConsumerConfig
}

// NewDLQConsumer validates cfg and constructs a consumer.
func NewDLQConsumer(cfg DLQConsumerConfig) (*DLQConsumer, error) {
	if cfg.Group == nil {
		return nil, errors.New("dlq consumer: nil Group")
	}
	if cfg.Topic == "" {
		return nil, errors.New("dlq consumer: empty Topic")
	}
	if cfg.Store == nil {
		return nil, errors.New("dlq consumer: nil Store")
	}
	if cfg.Logger == nil {
		return nil, errors.New("dlq consumer: nil Logger")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	return &DLQConsumer{cfg: cfg}, nil
}

// MaxAttempts returns the configured replay cap. Exposed so the
// admin handler can echo the same value the consumer enforces.
func (c *DLQConsumer) MaxAttempts() int { return c.cfg.MaxAttempts }

// Run consumes from the configured topic until ctx is cancelled.
func (c *DLQConsumer) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := c.cfg.Group.Consume(ctx, []string{c.cfg.Topic}, c); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Setup is part of sarama.ConsumerGroupHandler.
func (c *DLQConsumer) Setup(sarama.ConsumerGroupSession) error { return nil }

// Cleanup is part of sarama.ConsumerGroupHandler.
func (c *DLQConsumer) Cleanup(sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim handles each partition's message stream.
func (c *DLQConsumer) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		c.persist(sess.Context(), msg)
		sess.MarkMessage(msg, "")
	}
	return nil
}

// persist decodes the envelope, lands a row in the store, and bumps
// the same Prometheus counter dlq_observer increments so operators
// don't lose alerting parity when they wire the persistent consumer
// instead of the observer.
func (c *DLQConsumer) persist(ctx context.Context, msg *sarama.ConsumerMessage) {
	if msg == nil {
		return
	}
	var env DLQEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		c.cfg.Logger.Error("dlq consumer: undecodable envelope",
			slog.String("topic", msg.Topic),
			slog.Int64("offset", msg.Offset),
			slog.Int("partition", int(msg.Partition)),
			slog.String("error", err.Error()),
		)
		observability.DLQMessagesTotal.WithLabelValues(msg.Topic).Inc()
		return
	}

	originalTopic := string(headerValue(msg, "original_topic"))
	if originalTopic == "" {
		originalTopic = msg.Topic
	}

	row := &DLQMessage{
		ID:            ulid.Make().String(),
		TenantID:      env.Event.TenantID,
		SourceID:      env.Event.SourceID,
		DocumentID:    env.Event.DocumentID,
		OriginalTopic: originalTopic,
		PartitionKey:  string(msg.Key),
		Payload:       msg.Value,
		ErrorText:     env.Error,
		FailedAt:      env.FailedAt,
		AttemptCount:  env.AttemptCount,
		CreatedAt:     time.Now().UTC(),
	}
	if row.FailedAt.IsZero() {
		row.FailedAt = time.Now().UTC()
	}

	if err := c.cfg.Store.Insert(ctx, row); err != nil {
		c.cfg.Logger.Error("dlq consumer: persist failed",
			slog.String("tenant_id", row.TenantID),
			slog.String("error", err.Error()),
		)
		// Do NOT bump the counter here — the message landed in Kafka
		// but didn't make it to Postgres, so the counter would
		// double-count once an operator reruns the consumer with a
		// healthy database.
		return
	}

	c.cfg.Logger.Warn("dlq consumer: persisted",
		slog.String("id", row.ID),
		slog.String("tenant_id", row.TenantID),
		slog.String("source_id", row.SourceID),
		slog.String("document_id", row.DocumentID),
		slog.String("original_topic", originalTopic),
		slog.Int("attempt_count", row.AttemptCount),
	)
	observability.DLQMessagesTotal.WithLabelValues(originalTopic).Inc()
}

// ErrMaxRetriesExceeded is returned by the admin replay handler when
// a row's attempt_count has reached the configured cap. The
// retainable failure surfaces back to the operator so they know the
// envelope must be remediated by hand instead of replayed again.
var ErrMaxRetriesExceeded = errors.New("dlq: max replay attempts exceeded")

// ErrAlreadyReplayed is returned by the admin replay handler when a
// row has already been replayed and the request did not opt into a
// duplicate replay.
var ErrAlreadyReplayed = errors.New("dlq: already replayed")

// Replayer wires a DLQStore + a SyncProducer so the admin handler
// can re-inject a persisted DLQ row back onto the main ingest topic
// without growing the admin package's import surface.
type Replayer struct {
	store       DLQStore
	producer    SyncProducer
	maxAttempts int
}

// NewReplayer validates inputs and returns a Replayer.
func NewReplayer(store DLQStore, producer SyncProducer, maxAttempts int) (*Replayer, error) {
	if store == nil {
		return nil, errors.New("replayer: nil store")
	}
	if producer == nil {
		return nil, errors.New("replayer: nil producer")
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return &Replayer{store: store, producer: producer, maxAttempts: maxAttempts}, nil
}

// MaxAttempts returns the configured replay cap.
func (r *Replayer) MaxAttempts() int { return r.maxAttempts }

// Replay re-injects the supplied DLQ row's original IngestEvent onto
// the named topic. The attempt_count guard rejects rows that have
// already exhausted their replay budget; a row that has already been
// replayed once is rejected unless force is true.
func (r *Replayer) Replay(ctx context.Context, tenantID, id, topic string, force bool) error {
	if topic == "" {
		return errors.New("replayer: empty topic")
	}
	row, err := r.store.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if row.ReplayedAt != nil && !force {
		return ErrAlreadyReplayed
	}
	if row.AttemptCount >= r.maxAttempts {
		return ErrMaxRetriesExceeded
	}

	var env DLQEnvelope
	if err := json.Unmarshal(row.Payload, &env); err != nil {
		return fmt.Errorf("replayer: decode envelope: %w", err)
	}
	body, err := json.Marshal(env.Event)
	if err != nil {
		return fmt.Errorf("replayer: marshal event: %w", err)
	}

	msg := &sarama.ProducerMessage{
		Topic:     topic,
		Key:       sarama.StringEncoder(PartitionKey(env.Event.TenantID, env.Event.SourceID)),
		Value:     sarama.ByteEncoder(body),
		Timestamp: time.Now().UTC(),
	}
	if _, _, err := r.producer.SendMessage(msg); err != nil {
		_ = r.store.MarkReplayed(ctx, id, err)
		return fmt.Errorf("replayer: send: %w", err)
	}

	// SendMessage succeeded — the envelope is on the ingest topic
	// and cannot be unsent. Bookkeeping must run in this order:
	//
	//  1. MarkReplayed(nil)   — flips replayed_at from NULL to NOW().
	//                            This is the only thing that prevents
	//                            an operator (or this Replayer on a
	//                            retry) from re-injecting the same
	//                            envelope.
	//  2. BumpAttemptCount()  — purely a counter, safe to fail.
	//
	// Pre-fix the order was reversed: BumpAttemptCount first, then
	// MarkReplayed. A transient DB blip on the bump short-circuited
	// before MarkReplayed ran, so replayed_at stayed NULL and the
	// admin handler happily allowed the same envelope to be replayed
	// again — a duplicate on the ingest topic. Devin Review surfaced
	// this on PR #12; we now mark replayed first so a bump failure
	// can no longer leave the row in a re-replayable state.
	if err := r.store.MarkReplayed(ctx, id, nil); err != nil {
		// MarkReplayed failure is genuinely bad: the envelope is on
		// the topic but the row still says replayed_at=NULL, so a
		// subsequent Replay would double-inject. We can't undo the
		// publish; surface the error loudly so the operator can
		// reconcile manually. Skip BumpAttemptCount in this branch
		// — the row is already inconsistent and the counter is
		// secondary.
		return fmt.Errorf("replayer: mark replayed: %w", err)
	}
	if _, err := r.store.BumpAttemptCount(ctx, id); err != nil {
		// Counter bump failure is non-fatal — the envelope is on
		// the topic and the row is correctly marked replayed, so a
		// retry will hit the ErrAlreadyReplayed guard. Surface the
		// error so the operator sees the counter is stale, but the
		// system is in a safe state.
		return fmt.Errorf("replayer: bump attempts: %w", err)
	}
	return nil
}
