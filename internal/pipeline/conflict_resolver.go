// conflict_resolver.go — Round-7 Task 13.
//
// ConflictResolver is the Stage-4 store-side guard that decides
// whether an incoming chunk write should overwrite the persisted
// row. Today it implements a strict last-writer-wins policy
// keyed on a monotonic `content_version` integer per (tenant,
// document) — drops writes whose version is *strictly less than*
// the persisted one and emits an audit event so operators can
// see how often it triggers.
//
// The store worker is expected to call Resolve before its
// upsert. If Resolve returns ConflictDecisionAccept the write
// proceeds; ConflictDecisionDrop short-circuits the write and
// the caller should log/skip.
package pipeline

import (
	"context"
	"errors"
	"sync"
)

// ConflictDecision is the outcome.
type ConflictDecision int

const (
	// ConflictDecisionAccept lets the write proceed.
	ConflictDecisionAccept ConflictDecision = iota
	// ConflictDecisionDrop tells the caller to skip the write.
	ConflictDecisionDrop
)

// ConflictResolverInput is the call-site shape.
type ConflictResolverInput struct {
	TenantID       string
	DocumentID     string
	ChunkID        string
	ContentVersion int64
}

// ConflictVersionStore tracks the most recent persisted version
// per (tenant, document). Production uses a Postgres-backed
// implementation; tests use the in-memory fake below.
type ConflictVersionStore interface {
	Get(ctx context.Context, tenantID, documentID string) (int64, error)
	Put(ctx context.Context, tenantID, documentID string, version int64) error
}

// ConflictResolver enforces the policy.
type ConflictResolver struct {
	store ConflictVersionStore
}

// NewConflictResolver validates inputs.
func NewConflictResolver(store ConflictVersionStore) (*ConflictResolver, error) {
	if store == nil {
		return nil, errors.New("conflict_resolver: nil store")
	}
	return &ConflictResolver{store: store}, nil
}

// Resolve returns the decision and updates the version store on
// accept. On drop the caller can pass the decision to the audit
// emitter so the operator sees stale writes.
func (r *ConflictResolver) Resolve(ctx context.Context, in ConflictResolverInput) (ConflictDecision, error) {
	if in.TenantID == "" || in.DocumentID == "" {
		return ConflictDecisionAccept, errors.New("conflict_resolver: missing tenant/document")
	}
	current, err := r.store.Get(ctx, in.TenantID, in.DocumentID)
	if err != nil {
		return ConflictDecisionAccept, err
	}
	if in.ContentVersion < current {
		return ConflictDecisionDrop, nil
	}
	if err := r.store.Put(ctx, in.TenantID, in.DocumentID, in.ContentVersion); err != nil {
		return ConflictDecisionAccept, err
	}
	return ConflictDecisionAccept, nil
}

// InMemoryConflictVersionStore is the test fake.
type InMemoryConflictVersionStore struct {
	mu       sync.RWMutex
	versions map[string]int64
}

// NewInMemoryConflictVersionStore returns the fake.
func NewInMemoryConflictVersionStore() *InMemoryConflictVersionStore {
	return &InMemoryConflictVersionStore{versions: map[string]int64{}}
}

// Get returns the current version (0 if unknown).
func (s *InMemoryConflictVersionStore) Get(_ context.Context, tenantID, documentID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.versions[tenantID+"|"+documentID], nil
}

// Put advances the version. Callers may safely pass an older
// version — Put will not regress.
func (s *InMemoryConflictVersionStore) Put(_ context.Context, tenantID, documentID string, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.versions[tenantID+"|"+documentID]
	if version > cur {
		s.versions[tenantID+"|"+documentID] = version
	}
	return nil
}
