package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

// fakeDLQProducer is a sarama.SyncProducer-shaped fake.
type fakeDLQProducer struct {
	mu   sync.Mutex
	msgs []*sarama.ProducerMessage
}

func (f *fakeDLQProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msg)

	return 0, int64(len(f.msgs) - 1), nil
}

func (f *fakeDLQProducer) snapshot() []*sarama.ProducerMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]*sarama.ProducerMessage, len(f.msgs))
	copy(cp, f.msgs)

	return cp
}

// fakeConsumerGroup is a no-op sarama.ConsumerGroup implementation.
// Unit tests of the Consumer don't need a real broker; they exercise
// the ConsumeClaim path directly.
type fakeConsumerGroup struct{}

func (fakeConsumerGroup) Consume(_ context.Context, _ []string, _ sarama.ConsumerGroupHandler) error {
	return nil
}
func (fakeConsumerGroup) Errors() <-chan error        { return nil }
func (fakeConsumerGroup) Close() error                { return nil }
func (fakeConsumerGroup) Pause(_ map[string][]int32)  {}
func (fakeConsumerGroup) Resume(_ map[string][]int32) {}
func (fakeConsumerGroup) PauseAll()                   {}
func (fakeConsumerGroup) ResumeAll()                  {}

// fakeSession is a sarama.ConsumerGroupSession in-memory for tests.
type fakeSession struct {
	ctx    context.Context
	marked sync.Map // key: topic-partition-offset → struct{}
}

func (s *fakeSession) Claims() map[string][]int32                       { return nil }
func (s *fakeSession) MemberID() string                                 { return "test" }
func (s *fakeSession) GenerationID() int32                              { return 1 }
func (s *fakeSession) MarkOffset(_ string, _ int32, _ int64, _ string)  {}
func (s *fakeSession) Commit()                                          {}
func (s *fakeSession) ResetOffset(_ string, _ int32, _ int64, _ string) {}
func (s *fakeSession) Context() context.Context                         { return s.ctx }
func (s *fakeSession) MarkMessage(msg *sarama.ConsumerMessage, _ string) {
	s.marked.Store(messageKey(msg), struct{}{})
}
func messageKey(msg *sarama.ConsumerMessage) string {
	return msg.Topic + "-" +
		string(rune('0'+msg.Partition)) + "-" +
		formatOffset(msg.Offset)
}
func formatOffset(off int64) string {
	const digits = "0123456789"
	if off == 0 {
		return "0"
	}
	var buf []byte
	for off > 0 {
		buf = append([]byte{digits[off%10]}, buf...)
		off /= 10
	}

	return string(buf)
}

// fakeClaim is a sarama.ConsumerGroupClaim in-memory for tests.
type fakeClaim struct {
	topic     string
	partition int32
	msgs      chan *sarama.ConsumerMessage
}

func (c *fakeClaim) Topic() string                            { return c.topic }
func (c *fakeClaim) Partition() int32                         { return c.partition }
func (c *fakeClaim) InitialOffset() int64                     { return 0 }
func (c *fakeClaim) HighWaterMarkOffset() int64               { return 0 }
func (c *fakeClaim) Messages() <-chan *sarama.ConsumerMessage { return c.msgs }

func newTestConsumer(t *testing.T, fetch FetchStage, parse ParseStage, embed EmbedStage, store StoreStage) (*Consumer, *fakeDLQProducer) {
	t.Helper()
	cfg := newFastConfig(fetch, parse, embed, store)
	coord, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	dlq := &fakeDLQProducer{}
	cons, err := NewConsumer(fakeConsumerGroup{}, ConsumerConfig{
		GroupID:       "test-group",
		Topics:        []string{"ingest"},
		DLQTopic:      "ingest.dlq",
		DLQProducer:   dlq,
		Coordinator:   coord,
		CommitTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	return cons, dlq
}

func mustEnvelope(t *testing.T, evt IngestEvent) *sarama.ConsumerMessage {
	t.Helper()
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return &sarama.ConsumerMessage{
		Topic:     "ingest",
		Partition: 0,
		Offset:    int64(time.Now().UnixNano()),
		Key:       []byte(PartitionKey(evt.TenantID, evt.SourceID)),
		Value:     b,
	}
}

func TestConsumer_ConsumeClaim_HappyPath(t *testing.T) {
	t.Parallel()

	cons, _ := newTestConsumer(t,
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b", Text: "x"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)

	// Start the coordinator in a goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordDone := make(chan error, 1)
	go func() { coordDone <- cons.cfg.Coordinator.Run(ctx) }()

	msgs := make(chan *sarama.ConsumerMessage, 3)
	for i := 0; i < 3; i++ {
		msgs <- mustEnvelope(t, IngestEvent{
			Kind: EventDocumentChanged, TenantID: "t", SourceID: "s",
			DocumentID: "d-" + formatOffset(int64(i)),
		})
	}
	close(msgs)

	sess := &fakeSession{ctx: ctx}
	claim := &fakeClaim{topic: "ingest", partition: 0, msgs: msgs}

	// _ = cons.Setup(sess) — Setup is exercised by sarama; not needed here.
	if err := cons.ConsumeClaim(sess, claim); err != nil {
		t.Fatalf("ConsumeClaim: %v", err)
	}

	// All three messages must be marked.
	count := 0
	sess.marked.Range(func(_, _ any) bool { count++; return true })
	if count != 3 {
		t.Fatalf("expected 3 marked messages, got %d", count)
	}

	cancel()
	cons.cfg.Coordinator.CloseInputs()
	<-coordDone
}

func TestConsumer_DLQOnPoison(t *testing.T) {
	t.Parallel()

	cons, dlq := newTestConsumer(t,
		fakeFetch{fn: func(_ context.Context, _ IngestEvent) (*Document, error) {
			return nil, ErrPoisonMessage
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, nil }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) { return nil, "", nil }},
		&fakeStore{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordDone := make(chan error, 1)
	go func() { coordDone <- cons.cfg.Coordinator.Run(ctx) }()

	msgs := make(chan *sarama.ConsumerMessage, 1)
	msgs <- mustEnvelope(t, IngestEvent{Kind: EventDocumentChanged, TenantID: "t", DocumentID: "d"})
	close(msgs)

	sess := &fakeSession{ctx: ctx}
	claim := &fakeClaim{topic: "ingest", partition: 0, msgs: msgs}
	if err := cons.ConsumeClaim(sess, claim); err != nil {
		t.Fatalf("ConsumeClaim: %v", err)
	}
	if got := dlq.snapshot(); len(got) != 1 {
		t.Fatalf("DLQ messages: %d", len(got))
	} else if got[0].Topic != "ingest.dlq" {
		t.Fatalf("DLQ topic: %s", got[0].Topic)
	}

	cancel()
	cons.cfg.Coordinator.CloseInputs()
	<-coordDone
}

func TestConsumer_DLQOnMalformedEnvelope(t *testing.T) {
	t.Parallel()

	cons, dlq := newTestConsumer(t,
		fakeFetch{fn: func(_ context.Context, _ IngestEvent) (*Document, error) { return nil, nil }},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, nil }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) { return nil, "", nil }},
		&fakeStore{},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordDone := make(chan error, 1)
	go func() { coordDone <- cons.cfg.Coordinator.Run(ctx) }()

	msgs := make(chan *sarama.ConsumerMessage, 1)
	msgs <- &sarama.ConsumerMessage{Topic: "ingest", Partition: 0, Offset: 1, Value: []byte("not json")}
	close(msgs)

	sess := &fakeSession{ctx: ctx}
	claim := &fakeClaim{topic: "ingest", partition: 0, msgs: msgs}
	if err := cons.ConsumeClaim(sess, claim); err != nil {
		t.Fatalf("ConsumeClaim: %v", err)
	}
	if got := dlq.snapshot(); len(got) != 1 {
		t.Fatalf("DLQ messages: %d", len(got))
	}

	// Marked despite poison so we don't loop.
	count := 0
	sess.marked.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("expected 1 marked message, got %d", count)
	}

	cancel()
	cons.cfg.Coordinator.CloseInputs()
	<-coordDone
}

func TestConsumer_GracefulShutdown(t *testing.T) {
	t.Parallel()

	cons, _ := newTestConsumer(t,
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return []Block{{BlockID: "b", Text: "x"}}, nil }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = cons.cfg.Coordinator.Run(ctx) }()

	if err := cons.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := cons.Stop(); err != nil { // idempotent
		t.Fatalf("Stop idempotent: %v", err)
	}
	cancel()
}

func TestConsumer_NewConsumer_Validation(t *testing.T) {
	t.Parallel()

	c, _ := NewCoordinator(newFastConfig(fakeFetch{}, fakeParse{}, fakeEmbed{}, &fakeStore{}))
	dlq := &fakeDLQProducer{}
	for _, tc := range []struct {
		name  string
		group sarama.ConsumerGroup
		cfg   ConsumerConfig
	}{
		{"nil group", nil, ConsumerConfig{GroupID: "g", Coordinator: c, DLQProducer: dlq}},
		{"nil coord", fakeConsumerGroup{}, ConsumerConfig{GroupID: "g", DLQProducer: dlq}},
		{"nil dlq", fakeConsumerGroup{}, ConsumerConfig{GroupID: "g", Coordinator: c}},
		{"missing group id", fakeConsumerGroup{}, ConsumerConfig{Coordinator: c, DLQProducer: dlq}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewConsumer(tc.group, tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestConsumer_OffsetCommitOnlyAfterStage4(t *testing.T) {
	t.Parallel()

	storeReady := make(chan struct{})
	cons, _ := newTestConsumer(t,
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return []Block{{BlockID: "b", Text: "x"}}, nil }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&blockingStore{ready: storeReady},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cons.cfg.Coordinator.Run(ctx) }()

	msgs := make(chan *sarama.ConsumerMessage, 1)
	msgs <- mustEnvelope(t, IngestEvent{Kind: EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "d"})

	sess := &fakeSession{ctx: ctx}
	claim := &fakeClaim{topic: "ingest", partition: 0, msgs: msgs}

	consumeDone := make(chan error, 1)
	go func() { consumeDone <- cons.ConsumeClaim(sess, claim) }()

	// Give it a little time to enter the wait.
	time.Sleep(80 * time.Millisecond)

	// Offset should NOT yet be marked (Stage 4 still blocked).
	notYet := atomic.Bool{}
	sess.marked.Range(func(_, _ any) bool { notYet.Store(true); return false })
	if notYet.Load() {
		t.Fatal("offset committed before Stage 4 completed")
	}

	// Unblock store.
	close(storeReady)
	close(msgs)

	if err := <-consumeDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("ConsumeClaim: %v", err)
	}
	count := 0
	sess.marked.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("expected 1 marked message after Stage 4 completes, got %d", count)
	}
}
