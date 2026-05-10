package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/IBM/sarama"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// TestProducer_EmitEvent_RoundTripsRequestID pins the contract
// the round-4 task adds: a request id bound on ctx is copied into
// the JSON envelope, into the sidecar Kafka header, and survives
// decode on the consumer side via RequestIDFromKafkaMessage.
func TestProducer_EmitEvent_RoundTripsRequestID(t *testing.T) {
	t.Parallel()
	prod := &fakeSyncProducer{}
	p, err := pipeline.NewProducer(pipeline.ProducerConfig{Producer: prod, Topic: "ingest"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	ctx := observability.WithRequestID(context.Background(), "rid-42")
	if err := p.EmitEvent(ctx, pipeline.IngestEvent{
		Kind: pipeline.EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "d",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	sent := prod.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	msg := sent[0]

	// Header round-trip
	var headerVal string
	for _, h := range msg.Headers {
		if string(h.Key) == pipeline.KafkaRequestIDHeader {
			headerVal = string(h.Value)
		}
	}
	if headerVal != "rid-42" {
		t.Fatalf("kafka header request_id = %q, want rid-42", headerVal)
	}

	// JSON payload round-trip
	bodyBytes, _ := msg.Value.Encode()
	var decoded pipeline.IngestEvent
	if err := json.Unmarshal(bodyBytes, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.RequestID != "rid-42" {
		t.Fatalf("payload request_id = %q, want rid-42", decoded.RequestID)
	}

	// Consumer-side helper round-trip via the same headers.
	cmsg := &sarama.ConsumerMessage{Headers: []*sarama.RecordHeader{
		{Key: []byte(pipeline.KafkaRequestIDHeader), Value: []byte("rid-42")},
	}}
	if got := pipeline.RequestIDFromKafkaMessage(cmsg); got != "rid-42" {
		t.Fatalf("RequestIDFromKafkaMessage = %q", got)
	}
}

// TestProducer_EmitEvent_NoRequestIDWhenAbsent verifies the producer
// does not stamp a header when ctx has no request id (e.g. cron
// scheduler emissions).
func TestProducer_EmitEvent_NoRequestIDWhenAbsent(t *testing.T) {
	t.Parallel()
	prod := &fakeSyncProducer{}
	p, _ := pipeline.NewProducer(pipeline.ProducerConfig{Producer: prod, Topic: "ingest"})
	if err := p.EmitEvent(context.Background(), pipeline.IngestEvent{
		Kind: pipeline.EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "d",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := len(prod.snapshot()[0].Headers); got != 0 {
		t.Fatalf("expected no headers, got %d", got)
	}
}
