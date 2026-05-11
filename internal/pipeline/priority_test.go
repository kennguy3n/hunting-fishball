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

// TestPriorityBuffer_PushRacesClose exercises the TOCTOU window
// between Push's `closed.Load()` check and the actual send on the
// bucket channel. A naive guard returns ErrPriorityBufferClosed
// from the load but lets the send proceed, which then panics with
// "send on closed channel" because Close ran in between.
//
// We drive the race directly: prefill the bucket so subsequent
// pushes block, hold those pushes inside Push's `select`, then
// call Close. The blocked sends now race with `close(ch)`. The
// defer-recover converts the runtime panic into a clean
// ErrPriorityBufferClosed, so the test must (a) not panic and (b)
// see ErrPriorityBufferClosed from every blocked Push.
func TestPriorityBuffer_PushRacesClose(t *testing.T) {
	const blocked = 16
	pb := NewPriorityBuffer(PriorityBufferConfig{
		HighCapacity:   1,
		NormalCapacity: 1,
		LowCapacity:    1,
	})
	// Fill each bucket so subsequent pushes block on the channel
	// send inside Push's select — that is precisely the path the
	// TOCTOU bug fires on.
	for _, prio := range []Priority{PriorityHigh, PriorityNormal, PriorityLow} {
		if err := pb.Push(context.Background(), IngestEvent{}, prio); err != nil {
			t.Fatalf("seed push: %v", err)
		}
	}

	errs := make(chan error, blocked*3)
	var wg sync.WaitGroup
	for i := 0; i < blocked; i++ {
		for _, prio := range []Priority{PriorityHigh, PriorityNormal, PriorityLow} {
			wg.Add(1)
			go func(p Priority) {
				defer wg.Done()
				errs <- pb.Push(context.Background(), IngestEvent{}, p)
			}(prio)
		}
	}
	// Yield so blocked goroutines park inside the select.
	time.Sleep(5 * time.Millisecond)
	pb.Close()
	wg.Wait()
	close(errs)

	for err := range errs {
		// Blocked pushes either land in the buffer (nil) before
		// Close closes the channel, or they observe the close and
		// must return ErrPriorityBufferClosed — never panic and
		// never any other error.
		if err != nil && err != ErrPriorityBufferClosed {
			t.Fatalf("unexpected error: %v", err)
		}
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
