package rerankerv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestRerank_RoundTrip(t *testing.T) {
	t.Parallel()

	req := &RerankRequest{
		TenantId: "tenant-a",
		Query:    "how do I rotate creds?",
		Candidates: []*Candidate{
			{ChunkId: "c1", Text: "rotation guide", Score: 0.9},
			{ChunkId: "c2", Text: "outage runbook", Score: 0.4},
		},
		ModelId: "ms-marco-MiniLM-L-6-v2",
		TopK:    1,
	}
	resp := &RerankResponse{
		Scored: []*ScoredCandidate{
			{ChunkId: "c1", Score: 0.987},
		},
		ModelId: "ms-marco-MiniLM-L-6-v2",
	}

	for _, m := range []proto.Message{req, resp} {
		wire, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		clone := proto.Clone(m)
		proto.Reset(clone)
		if err := proto.Unmarshal(wire, clone); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !proto.Equal(m, clone) {
			t.Fatalf("round-trip mismatch: %+v vs %+v", m, clone)
		}
	}
}
