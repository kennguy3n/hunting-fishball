package retrieval_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// TestRetrieveWithSnapshot_HonoursACLDeny verifies that the
// snapshot-driven entrypoint applies the supplied snapshot's ACL
// rules. It is the contract the simulator relies on: the draft
// snapshot's deny rule MUST drop matches that the live ACL would
// have admitted.
func TestRetrieveWithSnapshot_HonoursACLDeny(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "drive:secret:payroll.csv", Score: 0.9, Payload: map[string]any{
				"tenant_id":     "tenant-a",
				"document_id":   "doc-1",
				"block_id":      "b1",
				"title":         "Payroll",
				"text":          "secrets",
				"privacy_label": "internal",
				"source_id":     "drive",
				"path":          "drive/secret/payroll.csv",
			}},
			{ID: "drive:public:intro.md", Score: 0.5, Payload: map[string]any{
				"tenant_id":     "tenant-a",
				"document_id":   "doc-2",
				"block_id":      "b1",
				"title":         "Intro",
				"text":          "hello",
				"privacy_label": "internal",
				"source_id":     "drive",
				"path":          "drive/public/intro.md",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           emb,
		DefaultPrivacyMode: "remote",
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	denySnapshot := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
		}},
	}

	resp, err := h.RetrieveWithSnapshot(context.Background(), "tenant-a",
		retrieval.RetrieveRequest{Query: "anything", TopK: 5}, denySnapshot)
	if err != nil {
		t.Fatalf("RetrieveWithSnapshot: %v", err)
	}
	for _, hit := range resp.Hits {
		if strings.Contains(hit.ID, "secret") {
			t.Fatalf("expected secret/** to be denied, got hit %+v", hit)
		}
	}
	if resp.Policy.BlockedCount == 0 {
		t.Fatalf("expected BlockedCount > 0, got %+v", resp.Policy)
	}
}

// TestRetrieveWithSnapshot_PrivacyModeOverride verifies that the
// supplied snapshot's EffectiveMode wins over the request's
// PrivacyMode field — matching the gin handler's behaviour where
// the resolved snapshot is the source of truth.
func TestRetrieveWithSnapshot_PrivacyModeOverride(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           emb,
		DefaultPrivacyMode: "remote",
	})

	resp, err := h.RetrieveWithSnapshot(context.Background(), "tenant-a",
		retrieval.RetrieveRequest{Query: "x", PrivacyMode: "remote"},
		policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeNoAI})
	if err != nil {
		t.Fatalf("RetrieveWithSnapshot: %v", err)
	}
	if resp.Policy.PrivacyMode != string(policy.PrivacyModeNoAI) {
		t.Fatalf("PrivacyMode: %q (want %q)", resp.Policy.PrivacyMode, policy.PrivacyModeNoAI)
	}
}

// TestRetrieveWithSnapshot_RejectsMissingTenantOrQuery is a guard
// against misuse from the simulator wiring.
func TestRetrieveWithSnapshot_RejectsMissingTenantOrQuery(t *testing.T) {
	t.Parallel()
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: &fakeVectorStore{},
		Embedder:    &fakeEmbedder{vec: []float32{1}},
	})
	if _, err := h.RetrieveWithSnapshot(context.Background(), "",
		retrieval.RetrieveRequest{Query: "x"}, policy.PolicySnapshot{}); err == nil {
		t.Fatal("expected missing-tenant error")
	}
	if _, err := h.RetrieveWithSnapshot(context.Background(), "tenant-a",
		retrieval.RetrieveRequest{Query: ""}, policy.PolicySnapshot{}); err == nil {
		t.Fatal("expected missing-query error")
	}
}
