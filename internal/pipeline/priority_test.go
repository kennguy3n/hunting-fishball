package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestDefaultPriorityClassifier(t *testing.T) {
	c := DefaultPriorityClassifier{Overrides: map[string]Priority{"src-vip": PriorityHigh}}
	cases := []struct {
		evt  IngestEvent
		want Priority
	}{
		{IngestEvent{Kind: EventReindex, SourceID: "x"}, PriorityHigh},
		{IngestEvent{Kind: EventDocumentDeleted}, PriorityHigh},
		{IngestEvent{Kind: EventPurge}, PriorityHigh},
		{IngestEvent{Kind: EventDocumentChanged, SourceID: "x"}, PriorityNormal},
		{IngestEvent{Kind: EventDocumentChanged, SourceID: "src-vip"}, PriorityHigh},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.evt); got != tc.want {
			t.Fatalf("classify(%+v) = %v want %v", tc.evt, got, tc.want)
		}
	}
}

func TestPriorityBuffer_PriorityOrdering(t *testing.T) {
	pb := NewPriorityBuffer(PriorityBufferConfig{NormalAfter: 100, LowAfter: 100})
	for i := 0; i < 5; i++ {
		_ = pb.Push(context.Background(), IngestEvent{DocumentID: "low"}, PriorityLow)
	}
	for i := 0; i < 3; i++ {
		_ = pb.Push(context.Background(), IngestEvent{DocumentID: "normal"}, PriorityNormal)
	}
	for i := 0; i < 2; i++ {
		_ = pb.Push(context.Background(), IngestEvent{DocumentID: "high"}, PriorityHigh)
	}
	pb.Close()

	got := []string{}
	pb.Drain(context.Background(), func(evt IngestEvent) {
		got = append(got, evt.DocumentID)
	})
	// Expected ordering (with NormalAfter=100, LowAfter=100 starvation
	// counters disabled): high (2), then normal (3), then low (5).
	want := []string{"high", "high", "normal", "normal", "normal", "low", "low", "low", "low", "low"}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d (got %v)", len(got), len(want), got)
	}
	for i, g := range got {
		if g != want[i] {
			t.Fatalf("at %d got %q want %q (full %v)", i, g, want[i], got)
		}
	}
}

func TestPriorityBuffer_StarvationPrevention(t *testing.T) {
	pb := NewPriorityBuffer(PriorityBufferConfig{NormalAfter: 2, LowAfter: 4})
	// Continuously feed high-priority events; check that some
	// normal/low pops still happen.
	for i := 0; i < 12; i++ {
		_ = pb.Push(context.Background(), IngestEvent{DocumentID: "h"}, PriorityHigh)
	}
	for i := 0; i < 12; i++ {
		_ = pb.Push(context.Background(), IngestEvent{DocumentID: "n"}, PriorityNormal)
	}
	for i := 0; i < 12; i++ {
		_ = pb.Push(context.Background(), IngestEvent{DocumentID: "l"}, PriorityLow)
	}
	pb.Close()

	got := []string{}
	pb.Drain(context.Background(), func(evt IngestEvent) {
		got = append(got, evt.DocumentID)
	})

	if len(got) != 36 {
		t.Fatalf("expected 36 events; got %d", len(got))
	}
	high, normal, low := pb.Stats()
	if high != 12 || normal != 12 || low != 12 {
		t.Fatalf("unexpected stats: %d/%d/%d", high, normal, low)
	}
	// Within the first 16 events we MUST see at least one low pick
	// (LowAfter=4 forces it). Without anti-starvation we'd see 12
	// highs in a row.
	gotLowEarly := false
	for _, e := range got[:16] {
		if e == "l" {
			gotLowEarly = true
			break
		}
	}
	if !gotLowEarly {
		t.Fatalf("low priority starved; first 16: %v", got[:16])
	}
}

func TestPriorityBuffer_PushAfterClose(t *testing.T) {
	pb := NewPriorityBuffer(PriorityBufferConfig{})
	pb.Close()
	err := pb.Push(context.Background(), IngestEvent{}, PriorityHigh)
	if err == nil {
		t.Fatalf("push after close must error")
	}
}

func TestPriorityBuffer_DrainStopsOnContext(t *testing.T) {
	pb := NewPriorityBuffer(PriorityBufferConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	count := 0
	go func() {
		pb.Drain(ctx, func(IngestEvent) {
			mu.Lock()
			count++
			mu.Unlock()
		})
	}()
	cancel()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("drain should not pop after cancel; got %d", count)
	}
}
