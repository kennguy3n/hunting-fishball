package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IBM/sarama"
)

// SyncProducer is the narrow contract the producer needs from a
// sarama.SyncProducer. *sarama.SyncProducer satisfies it; tests
// inject a fake.
type SyncProducer interface {
	SendMessage(*sarama.ProducerMessage) (partition int32, offset int64, err error)
	Close() error
}

// ProducerConfig configures Producer.
type ProducerConfig struct {
	// Producer is the underlying Kafka client.
	Producer SyncProducer

	// Topic is the destination topic ("ingest"). The partition key is
	// derived from PartitionKey(tenantID, sourceID) so events for one
	// (tenant, source) pair always land on the same partition.
	Topic string
}

// Producer publishes IngestEvents into the ingest topic. The admin
// source handler uses it to emit `source.connected` and re-scope
// kick-off events; the backfill orchestrator paces emissions through
// the same path.
type Producer struct {
	cfg ProducerConfig
}

// NewProducer validates cfg and returns a Producer.
func NewProducer(cfg ProducerConfig) (*Producer, error) {
	if cfg.Producer == nil {
		return nil, errors.New("producer: nil Producer")
	}
	if strings.TrimSpace(cfg.Topic) == "" {
		return nil, errors.New("producer: empty Topic")
	}
	return &Producer{cfg: cfg}, nil
}

// EmitEvent publishes a fully-formed IngestEvent. Used by the
// backfill orchestrator and re-scope flow.
func (p *Producer) EmitEvent(_ context.Context, evt IngestEvent) error {
	if evt.TenantID == "" || evt.SourceID == "" {
		return errors.New("producer: missing tenant/source")
	}
	if evt.DocumentID == "" {
		return errors.New("producer: missing document_id")
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("producer: marshal: %w", err)
	}

	msg := &sarama.ProducerMessage{
		Topic:     p.cfg.Topic,
		Key:       sarama.StringEncoder(PartitionKey(evt.TenantID, evt.SourceID)),
		Value:     sarama.ByteEncoder(body),
		Timestamp: time.Now().UTC(),
	}
	if _, _, err := p.cfg.Producer.SendMessage(msg); err != nil {
		return fmt.Errorf("producer: send: %w", err)
	}
	return nil
}

// EmitSourceConnected emits a kick-off event for a newly connected
// source. The event has no DocumentID — the consumer's backfill stage
// fans it out into per-document events. Implements the
// admin.EventEmitter contract.
//
// Note: the event uses EventReindex with a synthetic document id
// `source:<sourceID>` so the existing consumer/coordinator path
// accepts it without schema changes. Stage 1 of the coordinator
// recognises the prefix and routes the event into the backfill
// orchestrator instead of running Fetch.
func (p *Producer) EmitSourceConnected(ctx context.Context, tenantID, sourceID, connectorType string) error {
	if tenantID == "" || sourceID == "" {
		return errors.New("producer: missing tenant/source")
	}
	evt := IngestEvent{
		Kind:       EventReindex,
		TenantID:   tenantID,
		SourceID:   sourceID,
		DocumentID: KickoffDocumentID(sourceID),
		Metadata: map[string]string{
			"connector_type": connectorType,
			"sync_mode":      string(SyncModeBackfill),
		},
		SyncMode: SyncModeBackfill,
	}
	return p.EmitEvent(ctx, evt)
}

// KickoffDocumentID returns the synthetic DocumentID used by
// EmitSourceConnected. Exposed so the consumer can recognise the
// kick-off event and route it into the backfill orchestrator.
func KickoffDocumentID(sourceID string) string {
	return "source:" + sourceID
}

// IsKickoffEvent reports whether evt is a backfill kick-off event
// emitted by EmitSourceConnected.
func IsKickoffEvent(evt IngestEvent) bool {
	return evt.Kind == EventReindex && strings.HasPrefix(evt.DocumentID, "source:") && evt.SyncMode == SyncModeBackfill
}
