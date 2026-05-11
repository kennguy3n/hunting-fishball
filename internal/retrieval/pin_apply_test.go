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
