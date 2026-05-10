package grpcpool_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/kennguy3n/hunting-fishball/internal/grpcpool"
)

// startFakeServer spins up an empty gRPC server on a random port so
// the pool's Dial succeeds. We don't register any service — the pool
// only needs the connection.
func startFakeServer(t *testing.T) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), func() { srv.Stop() }
}

func TestPoolRoundRobin(t *testing.T) {
	t.Parallel()
	addr, stop := startFakeServer(t)
	defer stop()

	p, err := grpcpool.New(grpcpool.Config{Target: addr, Size: 3})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = p.Close() }()

	seen := map[*grpc.ClientConn]int{}
	for i := 0; i < 9; i++ {
		conn, _, release, err := p.Borrow(context.Background())
		if err != nil {
			t.Fatalf("borrow: %v", err)
		}
		seen[conn]++
		release(true)
	}
	if len(seen) != 3 {
		t.Fatalf("want 3 unique conns, got %d (%+v)", len(seen), seen)
	}
}

func TestCircuitBreakerTrips(t *testing.T) {
	t.Parallel()
	addr, stop := startFakeServer(t)
	defer stop()

	p, err := grpcpool.New(grpcpool.Config{Target: addr, Size: 1, Threshold: 3, OpenFor: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = p.Close() }()

	for i := 0; i < 3; i++ {
		_, _, release, err := p.Borrow(context.Background())
		if err != nil {
			t.Fatalf("borrow %d: %v", i, err)
		}
		release(false)
	}
	if p.State() != grpcpool.StateOpen {
		t.Fatalf("expected open, got %v", p.State())
	}
	_, _, _, err = p.Borrow(context.Background())
	if !errors.Is(err, grpcpool.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	_, _, release, err := p.Borrow(context.Background())
	if err != nil {
		t.Fatalf("half-open borrow: %v", err)
	}
	release(true)
	if p.State() != grpcpool.StateClosed {
		t.Fatalf("expected closed after success, got %v", p.State())
	}
}

func TestDeadline(t *testing.T) {
	t.Parallel()
	addr, stop := startFakeServer(t)
	defer stop()

	p, err := grpcpool.New(grpcpool.Config{Target: addr, Size: 1, Deadline: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = p.Close() }()

	_, ctx, release, err := p.Borrow(context.Background())
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatalf("ctx has no deadline")
	}
	release(true)
}
