// Package grpcpool implements a small round-robin gRPC connection
// pool with per-target deadlines and a circuit breaker per pool.
//
// Why this exists:
//   - The Python sidecars (docling, embedding, memory) live behind
//     long-lived gRPC streams. A single *grpc.ClientConn is enough
//     to multiplex calls in HTTP/2, but TLS handshake cost + the
//     Python services' threadpool size mean N parallel conns
//     produce noticeably better tail latency under load.
//   - When a sidecar process flaps, we don't want every in-flight
//     stage to retry against the same broken backend. The breaker
//     trips after `Threshold` consecutive failures and rejects
//     calls until `OpenFor` elapses (then half-open: a single trial
//     call decides the next state).
//
// The pool is thin on purpose: callers Borrow a *grpc.ClientConn,
// invoke their RPC, and Release. We do NOT proxy the gRPC client API
// because each sidecar has a different generated stub — wrapping
// every method would 10x the surface area for no win.
package grpcpool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// State is the breaker state.
type State int

const (
	// StateClosed allows calls.
	StateClosed State = iota
	// StateOpen rejects calls until OpenFor elapses.
	StateOpen
	// StateHalfOpen permits a single trial call.
	StateHalfOpen
)

// ErrCircuitOpen is returned by Borrow when the breaker is open.
var ErrCircuitOpen = errors.New("grpcpool: circuit breaker open")

// Config configures a Pool.
type Config struct {
	// Target is the gRPC target string (host:port) — passed to
	// grpc.NewClient.
	Target string

	// Size is the number of gRPC client connections. Defaults to 4.
	Size int

	// Deadline is applied via context.WithTimeout to every Borrow
	// call. Zero leaves the caller's context unchanged.
	Deadline time.Duration

	// Threshold is the consecutive-failure count that trips the
	// breaker. Defaults to 5.
	Threshold int

	// OpenFor is how long the breaker stays open before allowing a
	// half-open trial call. Defaults to 10s.
	OpenFor time.Duration

	// DialOptions are passed to grpc.NewClient. Defaults to insecure
	// credentials (matching the Phase 1 wiring).
	DialOptions []grpc.DialOption
}

func (c *Config) defaults() {
	if c.Size <= 0 {
		c.Size = 4
	}
	if c.Threshold <= 0 {
		c.Threshold = 5
	}
	if c.OpenFor <= 0 {
		c.OpenFor = 10 * time.Second
	}
	if len(c.DialOptions) == 0 {
		c.DialOptions = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
}

// Pool is a round-robin gRPC connection pool with a circuit breaker.
type Pool struct {
	cfg   Config
	conns []*grpc.ClientConn
	idx   uint64

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
}

// New constructs a Pool by dialing Size connections to Target.
func New(cfg Config) (*Pool, error) {
	if cfg.Target == "" {
		return nil, errors.New("grpcpool: Target required")
	}
	cfg.defaults()
	p := &Pool{cfg: cfg, conns: make([]*grpc.ClientConn, 0, cfg.Size)}
	for i := 0; i < cfg.Size; i++ {
		conn, err := grpc.NewClient(cfg.Target, cfg.DialOptions...)
		if err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("grpcpool: dial %s: %w", cfg.Target, err)
		}
		p.conns = append(p.conns, conn)
	}
	return p, nil
}

// Borrow returns a connection from the pool together with a context
// honouring the configured deadline. The returned releaseFn MUST be
// called regardless of whether the RPC succeeded — the breaker reads
// (success bool) to decide whether to advance state.
func (p *Pool) Borrow(ctx context.Context) (*grpc.ClientConn, context.Context, func(success bool), error) {
	if !p.allow() {
		return nil, ctx, func(bool) {}, ErrCircuitOpen
	}
	if p.cfg.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.Deadline)
		// Wrap the release to also cancel the deadline ctx.
		i := atomic.AddUint64(&p.idx, 1)
		conn := p.conns[(i-1)%uint64(len(p.conns))]
		release := func(success bool) {
			cancel()
			p.report(success)
		}
		return conn, ctx, release, nil
	}
	i := atomic.AddUint64(&p.idx, 1)
	conn := p.conns[(i-1)%uint64(len(p.conns))]
	return conn, ctx, func(success bool) { p.report(success) }, nil
}

// State reports the current breaker state. Safe to call concurrently.
func (p *Pool) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == StateOpen && time.Since(p.openedAt) >= p.cfg.OpenFor {
		// Lazily transition Open → HalfOpen on read.
		p.state = StateHalfOpen
	}
	return p.state
}

// Close closes every pooled connection. Idempotent.
func (p *Pool) Close() error {
	var firstErr error
	for _, c := range p.conns {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.conns = nil
	return firstErr
}

func (p *Pool) allow() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(p.openedAt) < p.cfg.OpenFor {
			return false
		}
		p.state = StateHalfOpen
		// The next Borrow call gets the trial.
		return true
	case StateHalfOpen:
		return true
	}
	return true
}

func (p *Pool) report(success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if success {
		p.failures = 0
		p.state = StateClosed
		return
	}
	p.failures++
	if p.state == StateHalfOpen {
		p.state = StateOpen
		p.openedAt = time.Now()
		return
	}
	if p.failures >= p.cfg.Threshold {
		p.state = StateOpen
		p.openedAt = time.Now()
	}
}
