package retrieval_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

func TestApplyPins_InsertsAtPosition(t *testing.T) {
	hits := []retrieval.RetrieveHit{
		{ID: "a", Score: 1.0},
		{ID: "b", Score: 0.9},
		{ID: "c", Score: 0.8},
	}
	pins := []retrieval.Pin{{ChunkID: "z", Position: 0}}
	out := retrieval.ApplyPins(hits, pins)
	if len(out) < 1 || out[0].ID != "z" {
		t.Fatalf("expected z first; got %+v", out)
	}
}

func TestApplyPins_NoPinsReturnsOriginal(t *testing.T) {
	hits := []retrieval.RetrieveHit{{ID: "a"}}
	out := retrieval.ApplyPins(hits, nil)
	if len(out) != 1 || out[0].ID != "a" {
		t.Fatalf("expected original; got %+v", out)
	}
}

func TestApplyPins_DeduplicatesPinnedHit(t *testing.T) {
	hits := []retrieval.RetrieveHit{
		{ID: "a", Score: 1.0},
		{ID: "b", Score: 0.9},
	}
	pins := []retrieval.Pin{{ChunkID: "b", Position: 0}}
	out := retrieval.ApplyPins(hits, pins)
	// b should be at position 0 and not duplicated later.
	if out[0].ID != "b" {
		t.Fatalf("expected b first; got %s", out[0].ID)
	}
	for i, h := range out {
		if i > 0 && h.ID == "b" {
			t.Fatalf("b duplicated at position %d", i)
		}
	}
}

// TestApplyPins_SparsePositions covers the cold-start and sparse-
// position cases the original loop bound dropped silently.
func TestApplyPins_SparsePositions(t *testing.T) {
	cases := []struct {
		name string
		hits []retrieval.RetrieveHit
		pins []retrieval.Pin
		want []string
	}{
		{
			name: "empty hits with pin at position 0",
			hits: nil,
			pins: []retrieval.Pin{{ChunkID: "z", Position: 0}},
			want: []string{"z"},
		},
		{
			name: "empty hits with pin at sparse position",
			hits: nil,
			pins: []retrieval.Pin{{ChunkID: "z", Position: 5}},
			want: []string{"z"},
		},
		{
			name: "small hits with pin beyond hit length",
			hits: []retrieval.RetrieveHit{{ID: "a"}, {ID: "b"}},
			pins: []retrieval.Pin{{ChunkID: "p", Position: 10}},
			want: []string{"a", "b", "p"},
		},
		{
			name: "multiple sparse pins with intermediate hits",
			hits: []retrieval.RetrieveHit{{ID: "a"}, {ID: "b"}},
			pins: []retrieval.Pin{
				{ChunkID: "p1", Position: 0},
				{ChunkID: "p2", Position: 2},
				{ChunkID: "p3", Position: 5},
			},
			want: []string{"p1", "a", "p2", "b", "p3"},
		},
		{
			name: "pins beyond hits emitted in sorted order",
			hits: nil,
			pins: []retrieval.Pin{
				{ChunkID: "third", Position: 3},
				{ChunkID: "first", Position: 0},
				{ChunkID: "second", Position: 1},
			},
			want: []string{"first", "second", "third"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := retrieval.ApplyPins(tc.hits, tc.pins)
			if len(out) != len(tc.want) {
				t.Fatalf("len mismatch: want %d, got %d (%+v)", len(tc.want), len(out), out)
			}
			for i, id := range tc.want {
				if out[i].ID != id {
					t.Errorf("pos %d: want %q, got %q (full=%+v)", i, id, out[i].ID, out)
				}
			}
		})
	}
}
