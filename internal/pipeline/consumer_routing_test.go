package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

// TestDecodeEvent_KeyOverridesEmptyBody verifies the consumer
// decoder pulls tenant_id/source_id from the partition key when the
// JSON body omits them. This lets producers stamp the routing key
// without duplicating the values in the body.
func TestDecodeEvent_KeyOverridesEmptyBody(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(IngestEvent{
		Kind: EventDocumentChanged, DocumentID: "d-1",
	})
	msg := &sarama.ConsumerMessage{
		Topic: "ingest", Partition: 0, Offset: 1,
		Key:   []byte(PartitionKey("tenant-a", "src-1")),
		Value: body,
	}
	evt, legacy, err := decodeEvent(msg)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if evt.TenantID != "tenant-a" || evt.SourceID != "src-1" {
		t.Fatalf("evt: %+v", evt)
	}
	if legacy {
		t.Fatal("`||` key must not be flagged as legacy")
	}
}

// TestDecodeEvent_LegacyKeyFlagged covers the one-release migration
// fallback in ParsePartitionKey: a Kafka message stamped with the
// deprecated single-`|` separator must still parse, but the second
// return value must be `true` so the consumer can surface it via
// OnLegacyKey for metrics. Without the fallback, in-flight messages
// produced by the prior shape would be DLQ'd during a rolling deploy.
func TestDecodeEvent_LegacyKeyFlagged(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(IngestEvent{
		Kind: EventDocumentChanged, DocumentID: "d-1",
	})
	msg := &sarama.ConsumerMessage{
		Topic: "ingest", Partition: 0, Offset: 1,
		// Single-pipe is the deprecated shape. Note the literal
		// string here, not PartitionKey(), because the producer
		// no longer emits this form.
		Key:   []byte("tenant-a|src-1"),
		Value: body,
	}
	evt, legacy, err := decodeEvent(msg)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if !legacy {
		t.Fatal("legacy `|` key must be flagged")
	}
	if evt.TenantID != "tenant-a" || evt.SourceID != "src-1" {
		t.Fatalf("evt: %+v", evt)
	}
}

// TestConsumer_LegacyKeyHookFires is the full-loop assertion that the
// OnLegacyKey hook fires exactly once per legacy-keyed message. The
// hook is what production wires to a metric counter so the legacy
// fallback can be removed once the topic drains.
func TestConsumer_LegacyKeyHookFires(t *testing.T) {
	t.Parallel()

	cons, _ := newTestConsumer(t,
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b", Text: "x"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)

	var (
		hookMu      sync.Mutex
		hookCalls   []string
		hookFiredCh = make(chan struct{}, 2)
	)
	cons.cfg.OnLegacyKey = func(_ context.Context, evt IngestEvent) {
		hookMu.Lock()
		hookCalls = append(hookCalls, evt.TenantID+"/"+evt.SourceID)
		hookMu.Unlock()
		hookFiredCh <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordDone := make(chan error, 1)
	go func() { coordDone <- cons.cfg.Coordinator.Run(ctx) }()

	body, _ := json.Marshal(IngestEvent{
		Kind: EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "d-1",
	})
	legacyMsg := &sarama.ConsumerMessage{
		Topic: "ingest", Partition: 0, Offset: 1,
		Key:   []byte("tenant-a|src-1"),
		Value: body,
	}
	modernMsg := mustEnvelope(t, IngestEvent{
		Kind: EventDocumentChanged, TenantID: "tenant-b", SourceID: "src-2", DocumentID: "d-2",
	})

	msgs := make(chan *sarama.ConsumerMessage, 2)
	msgs <- legacyMsg
	msgs <- modernMsg
	close(msgs)

	sess := &fakeSession{ctx: ctx}
	claim := &fakeClaim{topic: "ingest", partition: 0, msgs: msgs}
	if err := cons.ConsumeClaim(sess, claim); err != nil {
		t.Fatalf("ConsumeClaim: %v", err)
	}

	// The hook should fire exactly once: only for the legacy key.
	select {
	case <-hookFiredCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnLegacyKey did not fire")
	}
	// Drain any second hook invocation that would indicate a bug.
	select {
	case <-hookFiredCh:
		t.Fatal("OnLegacyKey fired for the modern `||` key")
	case <-time.After(100 * time.Millisecond):
	}

	hookMu.Lock()
	defer hookMu.Unlock()
	if len(hookCalls) != 1 || hookCalls[0] != "tenant-a/src-1" {
		t.Fatalf("hook calls: %+v", hookCalls)
	}

	cancel()
	cons.cfg.Coordinator.CloseInputs()
	if err := <-coordDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("coordinator: %v", err)
	}
}

// TestDecodeEvent_KeyMismatchRejected verifies the decoder fails
// closed when a misrouted producer puts the wrong tenant in the
// partition key. Without this guard a misconfigured producer could
// smuggle events onto another tenant's partition.
func TestDecodeEvent_KeyMismatchRejected(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(IngestEvent{
		Kind: EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "d",
	})
	cases := map[string][]byte{
		"tenant_mismatch": []byte(PartitionKey("tenant-evil", "src-1")),
		"source_mismatch": []byte(PartitionKey("tenant-a", "src-evil")),
		"malformed":       []byte("nope"),
	}
	for name, key := range cases {
		key := key
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := &sarama.ConsumerMessage{
				Topic: "ingest", Partition: 0, Offset: 1,
				Key: key, Value: body,
			}
			if _, _, err := decodeEvent(msg); err == nil {
				t.Fatal("expected decodeEvent to reject")
			}
		})
	}
}

// TestConsumer_RoutesByPartitionKey is an integration-style test of
// the routing path: two events with different (tenant, source) pairs
// must both reach the coordinator with their tenant_id/source_id
// preserved. This is the full contract Task 2 requires.
func TestConsumer_RoutesByPartitionKey(t *testing.T) {
	t.Parallel()

	type seen struct {
		tenant, source, doc string
	}
	var (
		mu     sync.Mutex
		events []seen
	)
	fetched := make(chan struct{}, 2)

	cons, _ := newTestConsumer(t,
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			mu.Lock()
			events = append(events, seen{evt.TenantID, evt.SourceID, evt.DocumentID})
			mu.Unlock()
			fetched <- struct{}{}
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b", Text: "x"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordDone := make(chan error, 1)
	go func() { coordDone <- cons.cfg.Coordinator.Run(ctx) }()

	msgs := make(chan *sarama.ConsumerMessage, 2)
	msgs <- mustEnvelope(t, IngestEvent{Kind: EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "d-1"})
	msgs <- mustEnvelope(t, IngestEvent{Kind: EventDocumentChanged, TenantID: "tenant-b", SourceID: "src-2", DocumentID: "d-2"})
	close(msgs)

	sess := &fakeSession{ctx: ctx}
	claim := &fakeClaim{topic: "ingest", partition: 0, msgs: msgs}

	if err := cons.ConsumeClaim(sess, claim); err != nil {
		t.Fatalf("ConsumeClaim: %v", err)
	}

	// Wait for both fetches.
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-fetched:
		case <-timeout:
			t.Fatal("timeout waiting for fetches")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	gotTenants := map[string]string{}
	for _, e := range events {
		gotTenants[e.tenant] = e.source
	}
	if gotTenants["tenant-a"] != "src-1" || gotTenants["tenant-b"] != "src-2" {
		t.Fatalf("routing lost (tenant,source) pairing: %+v", events)
	}

	cancel()
	cons.cfg.Coordinator.CloseInputs()
	if err := <-coordDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("coordinator: %v", err)
	}
}
