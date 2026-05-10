package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// memDLQStore is an in-memory DLQStore for unit tests.
type memDLQStore struct {
	mu       sync.Mutex
	rows     map[string]*pipeline.DLQMessage
	insertCh chan *pipeline.DLQMessage
	failNext error

	// bumpErr, when non-nil, makes the next BumpAttemptCount call
	// return that error WITHOUT mutating the row — used by the
	// Devin-Review-#4 regression to prove MarkReplayed still runs
	// when bookkeeping fails.
	bumpErr error
}

func newMemDLQStore() *memDLQStore {
	return &memDLQStore{
		rows:     map[string]*pipeline.DLQMessage{},
		insertCh: make(chan *pipeline.DLQMessage, 16),
	}
}

func (s *memDLQStore) Insert(_ context.Context, msg *pipeline.DLQMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	cp := *msg
	s.rows[msg.ID] = &cp
	select {
	case s.insertCh <- &cp:
	default:
	}
	return nil
}

func (s *memDLQStore) List(_ context.Context, f pipeline.DLQListFilter) ([]pipeline.DLQMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pipeline.DLQMessage
	for _, r := range s.rows {
		if r.TenantID != f.TenantID {
			continue
		}
		if f.OriginalTopic != "" && r.OriginalTopic != f.OriginalTopic {
			continue
		}
		if !f.IncludeReplayed && r.ReplayedAt != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *memDLQStore) Get(_ context.Context, tenantID, id string) (*pipeline.DLQMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, pipeline.ErrDLQNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *memDLQStore) MarkReplayed(_ context.Context, id string, replayErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return pipeline.ErrDLQNotFound
	}
	now := time.Now().UTC()
	r.ReplayedAt = &now
	if replayErr != nil {
		r.ReplayError = replayErr.Error()
	} else {
		r.ReplayError = ""
	}
	return nil
}

func (s *memDLQStore) BumpAttemptCount(_ context.Context, id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bumpErr != nil {
		err := s.bumpErr
		s.bumpErr = nil
		return 0, err
	}
	r, ok := s.rows[id]
	if !ok {
		return 0, pipeline.ErrDLQNotFound
	}
	r.AttemptCount++
	return r.AttemptCount, nil
}

// fakeProducer satisfies pipeline.SyncProducer / DLQProducer.
type fakeProducer struct {
	mu       sync.Mutex
	sent     []*sarama.ProducerMessage
	failNext error
}

func (p *fakeProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNext != nil {
		err := p.failNext
		p.failNext = nil
		return 0, 0, err
	}
	p.sent = append(p.sent, msg)
	return 0, int64(len(p.sent)), nil
}

func (p *fakeProducer) Close() error { return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func encodedDLQEnvelope(t *testing.T, evt pipeline.IngestEvent, errMsg string, attempts int) []byte {
	t.Helper()
	body, err := json.Marshal(pipeline.DLQEnvelope{
		Event:        evt,
		Error:        errMsg,
		FailedAt:     time.Now().UTC(),
		AttemptCount: attempts,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func TestDLQConsumer_NewDLQConsumer_Validation(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	logger := discardLogger()
	if _, err := pipeline.NewDLQConsumer(pipeline.DLQConsumerConfig{Topic: "x", Store: store, Logger: logger}); err == nil {
		t.Fatalf("expected error for nil Group")
	}
}

func TestDLQConsumer_Persist_DecodableEnvelope(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	c, err := pipeline.NewDLQConsumer(pipeline.DLQConsumerConfig{
		Group:  noopConsumerGroup{},
		Topic:  "ingest.dlq",
		Store:  store,
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	evt := pipeline.IngestEvent{
		Kind:       pipeline.EventDocumentChanged,
		TenantID:   "tenant-a",
		SourceID:   "src-1",
		DocumentID: "doc-1",
		FetchURL:   "https://example.test/x",
	}
	body := encodedDLQEnvelope(t, evt, "boom", 2)
	pipeline.DLQConsumerPersistForTest(c, &sarama.ConsumerMessage{
		Topic:     "ingest.dlq",
		Partition: 0,
		Offset:    1,
		Value:     body,
	})
	// MaxAttempts default is 5 when zero passed.
	if c.MaxAttempts() != 5 {
		t.Fatalf("default MaxAttempts=%d", c.MaxAttempts())
	}
	rows, _ := store.List(context.Background(), pipeline.DLQListFilter{TenantID: "tenant-a"})
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	got := rows[0]
	if got.TenantID != "tenant-a" || got.SourceID != "src-1" || got.DocumentID != "doc-1" {
		t.Fatalf("row scope mismatch: %+v", got)
	}
	if got.AttemptCount != 2 {
		t.Fatalf("attempt_count=%d", got.AttemptCount)
	}
	if got.OriginalTopic != "ingest.dlq" {
		t.Fatalf("original_topic=%q", got.OriginalTopic)
	}
}

func TestDLQConsumer_Persist_UndecodableEnvelopeIsDropped(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	c, _ := pipeline.NewDLQConsumer(pipeline.DLQConsumerConfig{
		Group:  noopConsumerGroup{},
		Topic:  "ingest.dlq",
		Store:  store,
		Logger: discardLogger(),
	})
	pipeline.DLQConsumerPersistForTest(c, &sarama.ConsumerMessage{
		Topic: "ingest.dlq",
		Value: []byte("garbage"),
	})
	rows, _ := store.List(context.Background(), pipeline.DLQListFilter{TenantID: "tenant-a", IncludeReplayed: true})
	if len(rows) != 0 {
		t.Fatalf("undecodable envelope should not persist; rows=%d", len(rows))
	}
}

func TestReplayer_Replay_HappyPath(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	prod := &fakeProducer{}
	r, err := pipeline.NewReplayer(store, prod, 5)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	evt := pipeline.IngestEvent{Kind: pipeline.EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "doc-1"}
	body := encodedDLQEnvelope(t, evt, "boom", 0)
	row := &pipeline.DLQMessage{
		ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest",
		Payload: body, ErrorText: "boom", FailedAt: time.Now(), CreatedAt: time.Now(),
	}
	_ = store.Insert(context.Background(), row)

	if err := r.Replay(context.Background(), "tenant-a", row.ID, "ingest", false); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(prod.sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(prod.sent))
	}
	if prod.sent[0].Topic != "ingest" {
		t.Fatalf("topic=%q", prod.sent[0].Topic)
	}
	got, _ := store.Get(context.Background(), "tenant-a", row.ID)
	if got.ReplayedAt == nil {
		t.Fatalf("replayed_at not set")
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count=%d", got.AttemptCount)
	}
}

func TestReplayer_Replay_AlreadyReplayed(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	prod := &fakeProducer{}
	r, _ := pipeline.NewReplayer(store, prod, 5)
	evt := pipeline.IngestEvent{Kind: pipeline.EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "doc-1"}
	now := time.Now().UTC()
	row := &pipeline.DLQMessage{
		ID: "01HZRYDLQID00000000000000B", TenantID: "tenant-a", OriginalTopic: "ingest",
		Payload: encodedDLQEnvelope(t, evt, "boom", 0), ErrorText: "boom", FailedAt: now, CreatedAt: now, ReplayedAt: &now,
	}
	_ = store.Insert(context.Background(), row)
	err := r.Replay(context.Background(), "tenant-a", row.ID, "ingest", false)
	if !errors.Is(err, pipeline.ErrAlreadyReplayed) {
		t.Fatalf("expected ErrAlreadyReplayed, got %v", err)
	}
	// Force=true bypasses the gate.
	if err := r.Replay(context.Background(), "tenant-a", row.ID, "ingest", true); err != nil {
		t.Fatalf("force replay: %v", err)
	}
}

func TestReplayer_Replay_MaxRetriesExceeded(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	prod := &fakeProducer{}
	r, _ := pipeline.NewReplayer(store, prod, 2)
	evt := pipeline.IngestEvent{Kind: pipeline.EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "doc-1"}
	row := &pipeline.DLQMessage{
		ID: "01HZRYDLQID00000000000000C", TenantID: "tenant-a", OriginalTopic: "ingest",
		Payload: encodedDLQEnvelope(t, evt, "boom", 0), ErrorText: "boom",
		FailedAt: time.Now(), CreatedAt: time.Now(), AttemptCount: 2,
	}
	_ = store.Insert(context.Background(), row)
	err := r.Replay(context.Background(), "tenant-a", row.ID, "ingest", true)
	if !errors.Is(err, pipeline.ErrMaxRetriesExceeded) {
		t.Fatalf("expected ErrMaxRetriesExceeded, got %v", err)
	}
}

func TestReplayer_Replay_TenantScopeEnforced(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	prod := &fakeProducer{}
	r, _ := pipeline.NewReplayer(store, prod, 5)
	evt := pipeline.IngestEvent{Kind: pipeline.EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "doc-1"}
	row := &pipeline.DLQMessage{
		ID: "01HZRYDLQID00000000000000D", TenantID: "tenant-a", OriginalTopic: "ingest",
		Payload: encodedDLQEnvelope(t, evt, "boom", 0), ErrorText: "boom", FailedAt: time.Now(), CreatedAt: time.Now(),
	}
	_ = store.Insert(context.Background(), row)
	err := r.Replay(context.Background(), "tenant-b", row.ID, "ingest", false)
	if !errors.Is(err, pipeline.ErrDLQNotFound) {
		t.Fatalf("expected ErrDLQNotFound for cross-tenant replay, got %v", err)
	}
}

func TestReplayer_Replay_ProducerFailureMarksReplayedWithError(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	prod := &fakeProducer{failNext: errors.New("kafka unavailable")}
	r, _ := pipeline.NewReplayer(store, prod, 5)
	evt := pipeline.IngestEvent{Kind: pipeline.EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "doc-1"}
	row := &pipeline.DLQMessage{
		ID: "01HZRYDLQID00000000000000E", TenantID: "tenant-a", OriginalTopic: "ingest",
		Payload: encodedDLQEnvelope(t, evt, "boom", 0), ErrorText: "boom", FailedAt: time.Now(), CreatedAt: time.Now(),
	}
	_ = store.Insert(context.Background(), row)
	if err := r.Replay(context.Background(), "tenant-a", row.ID, "ingest", false); err == nil {
		t.Fatal("expected error from producer")
	}
	got, _ := store.Get(context.Background(), "tenant-a", row.ID)
	if got.ReplayError == "" {
		t.Fatalf("expected replay_error to be set, got empty string")
	}
}

// TestReplayer_Replay_BumpFailureStillMarksReplayed is the regression
// test for the Devin-Review #4 finding on PR #12. Pre-fix the
// bookkeeping order in Replayer.Replay was:
//
//  1. SendMessage   — publishes to ingest topic.
//  2. BumpAttemptCount — increments attempt counter.
//  3. MarkReplayed  — stamps replayed_at = NOW().
//
// A transient DB error on step 2 short-circuited before step 3, so
// `replayed_at` stayed NULL and the admin handler's ErrAlreadyReplayed
// guard never triggered — an operator (or a retry) could replay the
// SAME envelope again, double-injecting it onto the ingest topic.
//
// Post-fix the order is SendMessage → MarkReplayed → BumpAttemptCount,
// so a bump failure leaves the row in a safe state: the envelope is on
// the topic AND the row is marked replayed, so a subsequent Replay
// hits ErrAlreadyReplayed and stops.
func TestReplayer_Replay_BumpFailureStillMarksReplayed(t *testing.T) {
	t.Parallel()
	store := newMemDLQStore()
	store.bumpErr = errors.New("db unavailable")
	prod := &fakeProducer{}
	r, _ := pipeline.NewReplayer(store, prod, 5)

	evt := pipeline.IngestEvent{Kind: pipeline.EventDocumentChanged, TenantID: "tenant-a", SourceID: "src-1", DocumentID: "doc-1"}
	row := &pipeline.DLQMessage{
		ID: "01HZRYDLQID00000000000000F", TenantID: "tenant-a", OriginalTopic: "ingest",
		Payload: encodedDLQEnvelope(t, evt, "boom", 0), ErrorText: "boom",
		FailedAt: time.Now(), CreatedAt: time.Now(),
	}
	_ = store.Insert(context.Background(), row)

	err := r.Replay(context.Background(), "tenant-a", row.ID, "ingest", false)
	if err == nil {
		t.Fatal("expected bump-attempt error to surface")
	}
	if !strings.Contains(err.Error(), "bump attempts") {
		t.Fatalf("error should be wrapped as bump-attempts failure, got %v", err)
	}

	// The envelope must have been published despite the bump failure
	// — SendMessage runs first.
	if len(prod.sent) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(prod.sent))
	}

	// MarkReplayed MUST have run before BumpAttemptCount, so
	// replayed_at is set and a follow-up Replay is rejected.
	got, _ := store.Get(context.Background(), "tenant-a", row.ID)
	if got.ReplayedAt == nil {
		t.Fatal("replayed_at must be set even when BumpAttemptCount fails")
	}
	if got.AttemptCount != 0 {
		t.Fatalf("AttemptCount must remain 0 when bump errored, got %d", got.AttemptCount)
	}

	// A second Replay (without force) must hit the ErrAlreadyReplayed
	// guard — i.e. no duplicate publish even after the bump failure.
	if err := r.Replay(context.Background(), "tenant-a", row.ID, "ingest", false); !errors.Is(err, pipeline.ErrAlreadyReplayed) {
		t.Fatalf("second replay must be blocked by ErrAlreadyReplayed, got %v", err)
	}
	if len(prod.sent) != 1 {
		t.Fatalf("second replay must NOT publish; sent count=%d", len(prod.sent))
	}
}

// noopConsumerGroup satisfies sarama.ConsumerGroup so we can construct
// a DLQConsumer in unit tests without touching Kafka.
type noopConsumerGroup struct{}

func (noopConsumerGroup) Consume(_ context.Context, _ []string, _ sarama.ConsumerGroupHandler) error {
	return nil
}
func (noopConsumerGroup) Errors() <-chan error        { return nil }
func (noopConsumerGroup) Close() error                { return nil }
func (noopConsumerGroup) Pause(_ map[string][]int32)  {}
func (noopConsumerGroup) Resume(_ map[string][]int32) {}
func (noopConsumerGroup) PauseAll()                   {}
func (noopConsumerGroup) ResumeAll()                  {}
