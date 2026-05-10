// Package pipeline — dlq_observer wires a Kafka consumer onto the
// dead-letter topic so operators can see (and alert on) every
// document that exhausted its pipeline retry budget.
//
// The observer is intentionally cheap: it never tries to reprocess
// a DLQ message — replay belongs in a separate workflow — and it
// holds no Postgres state. Each successfully decoded envelope:
//
//  1. emits a structured log line (ready for the slog handler in
//     internal/observability/logger.go), and
//  2. increments the `context_engine_dlq_messages_total` Prometheus
//     counter, labeled only by `original_topic`. Per-tenant
//     breakdowns live in the structured log line (`tenant_id`
//     field) where Loki / Splunk index them without the cardinality
//     blow-up a Prometheus label would carry on a multi-tenant
//     fleet — see internal/observability/metrics.go.
//
// Wiring is opt-in via the CONTEXT_ENGINE_DLQ_OBSERVE=1 env var so
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

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// DLQEnvelope mirrors the JSON shape Consumer.publishDLQ produces.
// Keep this in lock-step with the publisher in consumer.go — a
// schema drift would silently break observation.
type DLQEnvelope struct {
	Event        IngestEvent `json:"event"`
	Error        string      `json:"error"`
	FailedAt     time.Time   `json:"failed_at"`
	AttemptCount int         `json:"attempt_count,omitempty"`
}

// DLQObserverConfig configures a DLQObserver.
type DLQObserverConfig struct {
	// Group is the Kafka consumer group. The observer can run with
	// any group; production wires `<service>-dlq-observer` so it
	// doesn't share offsets with the main ingest group.
	Group sarama.ConsumerGroup

	// Topic is the dead-letter topic to observe (typically
	// `ingest.dlq`). Must match Consumer.cfg.DLQTopic.
	Topic string

	// Logger is the slog handle used for each dead-letter line.
	// Required.
	Logger *slog.Logger
}

// DLQObserver consumes a single DLQ topic and reports each envelope
// via slog + Prometheus. Stop the observer by cancelling the context
// passed to Run.
type DLQObserver struct {
	cfg DLQObserverConfig
}

// NewDLQObserver validates cfg and constructs an observer.
func NewDLQObserver(cfg DLQObserverConfig) (*DLQObserver, error) {
	if cfg.Group == nil {
		return nil, errors.New("dlq observer: nil Group")
	}
	if cfg.Topic == "" {
		return nil, errors.New("dlq observer: empty Topic")
	}
	if cfg.Logger == nil {
		return nil, errors.New("dlq observer: nil Logger")
	}
	return &DLQObserver{cfg: cfg}, nil
}

// Run consumes from the configured topic until ctx is cancelled or
// the underlying consumer group terminates. The method always
// returns the first error from the consumer group's Consume loop
// (typically context.Canceled on a clean shutdown).
func (o *DLQObserver) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := o.cfg.Group.Consume(ctx, []string{o.cfg.Topic}, o); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Setup is part of sarama.ConsumerGroupHandler. Nothing to do.
func (o *DLQObserver) Setup(sarama.ConsumerGroupSession) error { return nil }

// Cleanup is part of sarama.ConsumerGroupHandler.
func (o *DLQObserver) Cleanup(sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim handles each partition's message stream. The observer
// is read-only: an envelope that fails to decode is logged and
// committed (otherwise the consumer would loop forever on a poison
// message that no human has remediated).
func (o *DLQObserver) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		o.observeMessage(msg)
		sess.MarkMessage(msg, "")
	}
	return nil
}

// observeMessage decodes the envelope, emits a structured log
// line, and bumps the DLQ counter. Exposed (lower-case) so the
// unit test can drive it without spinning up Kafka.
func (o *DLQObserver) observeMessage(msg *sarama.ConsumerMessage) {
	if msg == nil {
		return
	}
	var env DLQEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		o.cfg.Logger.Error("dlq: undecodable envelope",
			slog.String("topic", msg.Topic),
			slog.Int64("offset", msg.Offset),
			slog.Int("partition", int(msg.Partition)),
			slog.String("error", err.Error()),
		)
		// Still bump the counter — operators want to alert on the
		// fact that a DLQ message arrived, not just on the ones we
		// could decode.
		observability.DLQMessagesTotal.WithLabelValues(msg.Topic).Inc()
		return
	}

	originalTopic := string(headerValue(msg, "original_topic"))
	if originalTopic == "" {
		// Best-effort fallback: if the publisher didn't stamp the
		// original topic, fall back to the DLQ topic itself so the
		// counter still has a non-empty label value.
		originalTopic = msg.Topic
	}

	o.cfg.Logger.Warn("dlq: dead-letter observed",
		slog.String("tenant_id", env.Event.TenantID),
		slog.String("source_id", env.Event.SourceID),
		slog.String("document_id", env.Event.DocumentID),
		slog.String("request_id", env.Event.RequestID),
		slog.String("error", env.Error),
		slog.Int("attempt_count", env.AttemptCount),
		slog.String("original_topic", originalTopic),
		slog.Time("failed_at", env.FailedAt),
		slog.String("dlq_topic", msg.Topic),
		slog.Int64("offset", msg.Offset),
		slog.Int("partition", int(msg.Partition)),
	)
	observability.DLQMessagesTotal.WithLabelValues(originalTopic).Inc()
}

// headerValue returns the first matching header value, or nil. The
// Kafka publisher in consumer.go does not currently stamp headers,
// but operators sometimes inject `original_topic` at the broker
// layer (e.g. via a Kafka Streams DLQ enricher). Reading it is a
// no-op when absent.
func headerValue(msg *sarama.ConsumerMessage, key string) []byte {
	for _, h := range msg.Headers {
		if h == nil {
			continue
		}
		if string(h.Key) == key {
			return h.Value
		}
	}
	return nil
}

// EncodeForTest is a small helper used by the observer test to
// construct an envelope identical to what Consumer.publishDLQ
// would emit.
func EncodeForTest(env DLQEnvelope) []byte {
	body, err := json.Marshal(env)
	if err != nil {
		panic(fmt.Sprintf("dlq encode: %v", err))
	}
	return body
}
