package retrieval

import (
	"context"
	"strings"
	"testing"
)

func TestSynonymExpander_NoOpWhenEmpty(t *testing.T) {
	e := NewSynonymExpander(nil, 4)
	got, err := e.Expand(context.Background(), "tenant-1", "hello world")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("missing synonyms must be a no-op; got %q", got)
	}
}

func TestSynonymExpander_AppendsSynonyms(t *testing.T) {
	store := NewInMemorySynonymStore()
	if err := store.Set(context.Background(), "t1", map[string][]string{
		"car": {"vehicle", "automobile"},
		"big": {"large", "huge"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	e := NewSynonymExpander(store, 3)
	got, err := e.Expand(context.Background(), "t1", "Big car?")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(got, "Big car?") {
		t.Fatalf("expansion must preserve original; got %q", got)
	}
	for _, expect := range []string{"large", "huge", "vehicle"} {
		if !strings.Contains(got, expect) {
			t.Fatalf("missing synonym %q in %q", expect, got)
		}
	}
}

func TestSynonymExpander_TenantIsolation(t *testing.T) {
	store := NewInMemorySynonymStore()
	_ = store.Set(context.Background(), "t1", map[string][]string{"car": {"vehicle"}})
	e := NewSynonymExpander(store, 4)
	// Tenant-2 has no synonyms — even matching tokens stay
	// untouched.
	out, err := e.Expand(context.Background(), "t2", "car")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if out != "car" {
		t.Fatalf("cross-tenant leakage; got %q", out)
	}
}

func TestSynonymExpander_AppendLimit(t *testing.T) {
	store := NewInMemorySynonymStore()
	_ = store.Set(context.Background(), "t1", map[string][]string{
		"car": {"a", "b", "c", "d", "e"},
	})
	e := NewSynonymExpander(store, 2)
	got, _ := e.Expand(context.Background(), "t1", "car")
	// Original 1 word + at most 2 synonyms = 3 tokens.
	if got == "car" {
		t.Fatalf("expected at least one synonym; got %q", got)
	}
	tokens := strings.Fields(got)
	if len(tokens) > 3 {
		t.Fatalf("append limit not enforced; got %v", tokens)
	}
}

func TestInMemorySynonymStore_OverwriteAndDelete(t *testing.T) {
	store := NewInMemorySynonymStore()
	if err := store.Set(context.Background(), "t1", map[string][]string{"a": {"b"}}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.Set(context.Background(), "t1", map[string][]string{"x": {"y"}}); err != nil {
		t.Fatalf("set overwrite: %v", err)
	}
	got, _ := store.Get(context.Background(), "t1")
	if _, ok := got["a"]; ok {
		t.Fatalf("set must replace not merge")
	}
	if err := store.Set(context.Background(), "t1", nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := store.Get(context.Background(), "t1"); len(got) != 0 {
		t.Fatalf("nil set should clear; got %v", got)
	}
}
