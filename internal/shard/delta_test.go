package shard_test

import (
	"reflect"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

func TestComputeDelta_AddsAndRemoves(t *testing.T) {
	t.Parallel()
	prev := []string{"a", "b", "c"}
	curr := []string{"b", "c", "d", "e"}

	got := shard.ComputeDelta(prev, curr)
	want := []shard.DeltaOp{
		{Kind: shard.DeltaAdd, ChunkID: "d"},
		{Kind: shard.DeltaAdd, ChunkID: "e"},
		{Kind: shard.DeltaRemove, ChunkID: "a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestComputeDelta_EmptyPrevIsFullSync(t *testing.T) {
	t.Parallel()
	got := shard.ComputeDelta(nil, []string{"a", "b"})
	if len(got) != 2 {
		t.Fatalf("want 2 ops, got %d", len(got))
	}
	for _, op := range got {
		if op.Kind != shard.DeltaAdd {
			t.Fatalf("expected only adds, got %v", op)
		}
	}
}

func TestComputeDelta_EmptyCurrRemovesAll(t *testing.T) {
	t.Parallel()
	got := shard.ComputeDelta([]string{"a", "b"}, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 ops, got %d", len(got))
	}
	for _, op := range got {
		if op.Kind != shard.DeltaRemove {
			t.Fatalf("expected only removes, got %v", op)
		}
	}
}

func TestComputeDelta_DropsDuplicates(t *testing.T) {
	t.Parallel()
	got := shard.ComputeDelta([]string{"a", "a", ""}, []string{"a", "a", "b", ""})
	want := []shard.DeltaOp{{Kind: shard.DeltaAdd, ChunkID: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestComputeDelta_DeterministicOrdering(t *testing.T) {
	t.Parallel()
	got1 := shard.ComputeDelta([]string{"x", "y"}, []string{"a", "b", "c"})
	got2 := shard.ComputeDelta([]string{"y", "x"}, []string{"c", "b", "a"})
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("not deterministic:\n  got1: %v\n  got2: %v", got1, got2)
	}
}
