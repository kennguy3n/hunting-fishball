package retrieval_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// fakeShardLookup is a minimal ShardVersionLookup the device-first
// tests configure with a constant freshest version (or an error).
type fakeShardLookup struct {
	version int64
	err     error
}

func (f *fakeShardLookup) LatestShardVersion(_ context.Context, _, _, _ string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.version, nil
}

func TestDeviceFirst_HighTier_PrefersLocal(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "c1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1, 2}},
		ShardVersionLookup: &fakeShardLookup{version: 9},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:       "hello",
		PrivacyMode: "hybrid",
		DeviceTier:  "high",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.PreferLocal {
		t.Fatalf("expected prefer_local=true, got %+v", got)
	}
	if got.LocalShardVersion != 9 {
		t.Fatalf("local_shard_version=%d", got.LocalShardVersion)
	}
	if got.PreferLocalReason != "prefer_local" {
		t.Fatalf("reason=%q", got.PreferLocalReason)
	}
}

func TestDeviceFirst_LowTier_StaysRemote(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "c1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1, 2}},
		ShardVersionLookup: &fakeShardLookup{version: 9},
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:       "hello",
		PrivacyMode: "hybrid",
		DeviceTier:  "low",
	})
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PreferLocal {
		t.Fatalf("expected prefer_local=false for low tier")
	}
	if got.PreferLocalReason != "device_tier_too_low" {
		t.Fatalf("reason=%q", got.PreferLocalReason)
	}
}

func TestDeviceFirst_NoShard_StaysRemote(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "c1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1, 2}},
		ShardVersionLookup: &fakeShardLookup{version: 0},
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:       "hello",
		PrivacyMode: "hybrid",
		DeviceTier:  "high",
	})
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PreferLocal {
		t.Fatalf("expected prefer_local=false")
	}
	if got.PreferLocalReason != "no_local_shard" {
		t.Fatalf("reason=%q", got.PreferLocalReason)
	}
}

func TestDeviceFirst_LookupErrorTreatedAsNoShard(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "c1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1, 2}},
		ShardVersionLookup: &fakeShardLookup{err: errors.New("repo down")},
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:       "hello",
		PrivacyMode: "hybrid",
		DeviceTier:  "high",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even on lookup error, got %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PreferLocal {
		t.Fatalf("expected prefer_local=false on lookup error")
	}
	if got.PreferLocalReason != "no_local_shard" {
		t.Fatalf("reason=%q", got.PreferLocalReason)
	}
}

// TestDeviceFirst_ChannelDisallowed_StaysRemote verifies that a
// channel which has explicitly opted out of local retrieval (via
// PolicySnapshot.DenyLocalRetrieval=true on the resolved snapshot)
// gets `prefer_local=false` with a `channel_disallowed` reason —
// regardless of device tier or shard freshness. This is the path
// `policy.Decide` exposes via the `AllowLocalRetrieval=false`
// branch; before this path was wired the reason was unreachable in
// production.
func TestDeviceFirst_ChannelDisallowed_StaysRemote(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "c1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	denyResolver := policy.ResolverFunc(func(_ context.Context, _, _ string) (policy.PolicySnapshot, error) {
		return policy.PolicySnapshot{
			EffectiveMode:      policy.PrivacyModeHybrid,
			DenyLocalRetrieval: true,
		}, nil
	})
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1, 2}},
		ShardVersionLookup: &fakeShardLookup{version: 9},
		PolicyResolver:     denyResolver,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:       "hello",
		PrivacyMode: "hybrid",
		DeviceTier:  "high",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PreferLocal {
		t.Fatalf("expected prefer_local=false for disallowed channel")
	}
	if got.PreferLocalReason != "channel_disallowed" {
		t.Fatalf("reason=%q (want channel_disallowed)", got.PreferLocalReason)
	}
	if got.LocalShardVersion != 9 {
		t.Fatalf("local_shard_version=%d (want echoed even when disallowed)", got.LocalShardVersion)
	}
}

func TestDeviceFirst_NoLookupConfigured_NoOp(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "c1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:       "hello",
		PrivacyMode: "hybrid",
		DeviceTier:  "high",
	})
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PreferLocal {
		t.Fatalf("no lookup configured should never prefer_local")
	}
	if got.PreferLocalReason != "no_local_shard" {
		t.Fatalf("reason=%q", got.PreferLocalReason)
	}
}
