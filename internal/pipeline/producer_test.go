package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/IBM/sarama"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// fakeSyncProducer captures sent messages without talking to Kafka.
type fakeSyncProducer struct {
	mu      sync.Mutex
	sent    []*sarama.ProducerMessage
	sendErr error
}

func (f *fakeSyncProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, 0, f.sendErr
	}
	f.sent = append(f.sent, msg)
	return 0, int64(len(f.sent) - 1), nil
}

func (f *fakeSyncProducer) Close() error { return nil }

func (f *fakeSyncProducer) snapshot() []*sarama.ProducerMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]*sarama.ProducerMessage, len(f.sent))
	copy(cp, f.sent)
	return cp
}

func TestNewProducer_RejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := pipeline.NewProducer(pipeline.ProducerConfig{Topic: "ingest"}); err == nil {
		t.Fatal("nil Producer must error")
	}
	if _, err := pipeline.NewProducer(pipeline.ProducerConfig{Producer: &fakeSyncProducer{}}); err == nil {
		t.Fatal("empty Topic must error")
	}
}

func TestProducer_EmitEvent_StampsKey(t *testing.T) {
	t.Parallel()
	prod := &fakeSyncProducer{}
	p, err := pipeline.NewProducer(pipeline.ProducerConfig{Producer: prod, Topic: "ingest"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	evt := pipeline.IngestEvent{
		Kind: pipeline.EventDocumentChanged, TenantID: "tenant-a",
		SourceID: "src-1", DocumentID: "doc-1",
	}
	if err := p.EmitEvent(context.Background(), evt); err != nil {
		t.Fatalf("EmitEvent: %v", err)
	}
	got := prod.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	key, _ := got[0].Key.Encode()
	wantKey := pipeline.PartitionKey("tenant-a", "src-1")
	if string(key) != wantKey {
		t.Fatalf("key: got=%q want=%q", string(key), wantKey)
	}
	if got[0].Topic != "ingest" {
		t.Fatalf("topic: %q", got[0].Topic)
	}
	body, _ := got[0].Value.Encode()
	var roundtrip pipeline.IngestEvent
	if err := json.Unmarshal(body, &roundtrip); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if roundtrip.DocumentID != "doc-1" {
		t.Fatalf("body: %+v", roundtrip)
	}
}

func TestProducer_EmitEvent_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	p, _ := pipeline.NewProducer(pipeline.ProducerConfig{Producer: &fakeSyncProducer{}, Topic: "ingest"})
	cases := []pipeline.IngestEvent{
		{Kind: pipeline.EventDocumentChanged, SourceID: "s", DocumentID: "d"},
		{Kind: pipeline.EventDocumentChanged, TenantID: "t", DocumentID: "d"},
		{Kind: pipeline.EventDocumentChanged, TenantID: "t", SourceID: "s"},
	}
	for i, evt := range cases {
		if err := p.EmitEvent(context.Background(), evt); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestProducer_EmitEvent_PropagatesSendError(t *testing.T) {
	t.Parallel()
	prod := &fakeSyncProducer{sendErr: errors.New("broker down")}
	p, _ := pipeline.NewProducer(pipeline.ProducerConfig{Producer: prod, Topic: "ingest"})
	evt := pipeline.IngestEvent{
		Kind: pipeline.EventDocumentChanged, TenantID: "t",
		SourceID: "s", DocumentID: "d",
	}
	if err := p.EmitEvent(context.Background(), evt); err == nil {
		t.Fatal("expected send error")
	}
}

func TestProducer_EmitSourceConnected_TagsBackfill(t *testing.T) {
	t.Parallel()
	prod := &fakeSyncProducer{}
	p, _ := pipeline.NewProducer(pipeline.ProducerConfig{Producer: prod, Topic: "ingest"})
	if err := p.EmitSourceConnected(context.Background(), "tenant-a", "src-1", "google-drive"); err != nil {
		t.Fatalf("EmitSourceConnected: %v", err)
	}
	got := prod.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	body, _ := got[0].Value.Encode()
	var evt pipeline.IngestEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if evt.SyncMode != pipeline.SyncModeBackfill {
		t.Fatalf("SyncMode: %q", evt.SyncMode)
	}
	if !pipeline.IsKickoffEvent(evt) {
		t.Fatalf("IsKickoffEvent: %+v", evt)
	}
	if evt.DocumentID != pipeline.KickoffDocumentID("src-1") {
		t.Fatalf("DocumentID: %q", evt.DocumentID)
	}
}

func TestParsePartitionKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in          string
		ok          bool
		tenant, src string
	}{
		{"tenant-a||src-1", true, "tenant-a", "src-1"},
		{"||src-1", false, "", ""},
		{"tenant-a||", false, "", ""},
		// Single-pipe legacy keys are explicitly rejected so a stray
		// `|` in a tenant/source ID can never split a partition key.
		{"tenant-a|src-1", false, "", ""},
		{"no-separator", false, "", ""},
		{"", false, "", ""},
	}
	for _, c := range cases {
		gotT, gotS, gotOk := pipeline.ParsePartitionKey(c.in)
		if gotOk != c.ok || gotT != c.tenant || gotS != c.src {
			t.Fatalf("ParsePartitionKey(%q) = (%q,%q,%v) want (%q,%q,%v)", c.in, gotT, gotS, gotOk, c.tenant, c.src, c.ok)
		}
	}
}
