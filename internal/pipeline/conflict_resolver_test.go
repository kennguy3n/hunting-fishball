package pipeline_test

import (
	"context"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestConflictResolver_AcceptsFirstWrite(t *testing.T) {
	r, err := pipeline.NewConflictResolver(pipeline.NewInMemoryConflictVersionStore())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	d, err := r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "ta", DocumentID: "doc1", ContentVersion: 1})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d != pipeline.ConflictDecisionAccept {
		t.Fatalf("expected accept; got %v", d)
	}
}

func TestConflictResolver_DropsStaleWrite(t *testing.T) {
	r, _ := pipeline.NewConflictResolver(pipeline.NewInMemoryConflictVersionStore())
	_, _ = r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "ta", DocumentID: "doc1", ContentVersion: 5})
	d, _ := r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "ta", DocumentID: "doc1", ContentVersion: 3})
	if d != pipeline.ConflictDecisionDrop {
		t.Fatalf("expected drop; got %v", d)
	}
}

func TestConflictResolver_AcceptsEqualOrHigher(t *testing.T) {
	r, _ := pipeline.NewConflictResolver(pipeline.NewInMemoryConflictVersionStore())
	_, _ = r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "ta", DocumentID: "doc1", ContentVersion: 5})
	d, _ := r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "ta", DocumentID: "doc1", ContentVersion: 5})
	if d != pipeline.ConflictDecisionAccept {
		t.Fatalf("expected accept equal version; got %v", d)
	}
	d, _ = r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "ta", DocumentID: "doc1", ContentVersion: 7})
	if d != pipeline.ConflictDecisionAccept {
		t.Fatalf("expected accept higher version; got %v", d)
	}
}

func TestConflictResolver_TenantIsolation(t *testing.T) {
	store := pipeline.NewInMemoryConflictVersionStore()
	r, _ := pipeline.NewConflictResolver(store)
	_, _ = r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "ta", DocumentID: "doc1", ContentVersion: 5})
	// Same docID for a different tenant should start at 0 — accept version 1.
	d, _ := r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: "tb", DocumentID: "doc1", ContentVersion: 1})
	if d != pipeline.ConflictDecisionAccept {
		t.Fatalf("expected accept across tenant; got %v", d)
	}
}
