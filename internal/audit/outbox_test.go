package audit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// fakeProducer is an in-memory sarama.SyncProducer-shaped fake. It
// implements audit.MessageProducer.
type fakeProducer struct {
	mu          sync.Mutex
	sent        []*sarama.ProducerMessage
	sendErr     error
	failAtIndex int // 0 = no failure
}

func (f *fakeProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, 0, f.sendErr
	}
	if f.failAtIndex > 0 && len(f.sent)+1 >= f.failAtIndex {
		return 0, 0, errors.New("synthetic broker error")
	}
	f.sent = append(f.sent, msg)

	return 0, int64(len(f.sent) - 1), nil
}

func (f *fakeProducer) Close() error { return nil }

func (f *fakeProducer) snapshot() []*sarama.ProducerMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	cp := make([]*sarama.ProducerMessage, len(f.sent))
	copy(cp, f.sent)

	return cp
}

func TestOutbox_RunOnce_PublishesAndMarks(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		log := audit.NewAuditLog("tenant-a", "", audit.ActionSourceSynced, "source", "src", nil, "")
		log.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := repo.Create(ctx, log); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	prod := &fakeProducer{}
	ob, err := audit.NewOutbox(repo, prod, audit.OutboxConfig{Topic: "audit.events"})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}

	n, err := ob.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 published, got %d", n)
	}

	sent := prod.snapshot()
	if len(sent) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(sent))
	}
	for _, m := range sent {
		if m.Topic != "audit.events" {
			t.Fatalf("wrong topic: %q", m.Topic)
		}
		if k, _ := m.Key.Encode(); string(k) != "tenant-a" {
			t.Fatalf("wrong key: %q", k)
		}
	}

	// Pending must drop to zero after publish.
	pending, err := repo.PendingPublish(ctx, 10)
	if err != nil {
		t.Fatalf("PendingPublish: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after publish, got %d", len(pending))
	}
}

func TestOutbox_RunOnce_NoOpOnEmptyQueue(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	prod := &fakeProducer{}
	ob, err := audit.NewOutbox(repo, prod, audit.OutboxConfig{Topic: "audit.events"})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	n, err := ob.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
}

func TestOutbox_RunOnce_PartialFailure_StopsAndPersistsProgress(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		log := audit.NewAuditLog("tenant-a", "", audit.ActionSourceSynced, "source", "src", nil, "")
		log.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := repo.Create(ctx, log); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	prod := &fakeProducer{failAtIndex: 2} // succeed once, then fail
	ob, err := audit.NewOutbox(repo, prod, audit.OutboxConfig{Topic: "audit.events"})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}

	n, err := ob.RunOnce(ctx)
	if err == nil {
		t.Fatal("expected publish error")
	}
	if n != 1 {
		t.Fatalf("expected 1 published before failure, got %d", n)
	}

	pending, err := repo.PendingPublish(ctx, 10)
	if err != nil {
		t.Fatalf("PendingPublish: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 still pending after partial failure, got %d", len(pending))
	}
}

func TestOutbox_NewOutbox_RejectsBadConfig(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	prod := &fakeProducer{}

	for _, tc := range []struct {
		name string
		repo *audit.Repository
		prod audit.MessageProducer
		cfg  audit.OutboxConfig
	}{
		{"nil repo", nil, prod, audit.OutboxConfig{Topic: "x"}},
		{"nil producer", repo, nil, audit.OutboxConfig{Topic: "x"}},
		{"missing topic", repo, prod, audit.OutboxConfig{}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := audit.NewOutbox(tc.repo, tc.prod, tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestOutbox_Run_ContextCancellation(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	prod := &fakeProducer{}
	ob, err := audit.NewOutbox(repo, prod, audit.OutboxConfig{
		Topic:        "audit.events",
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Run must return *some* error after the context fires. SQLite's
	// driver often surfaces an "interrupted" error when ctx is cancelled
	// mid-query; either flavour is acceptable as long as the loop
	// terminates promptly.
	done := make(chan error, 1)
	go func() { done <- ob.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run must return non-nil after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}
