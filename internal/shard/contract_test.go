package shard_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

// fakeContract is an in-memory ShardClientContract for the
// contract-shape tests. The "real" implementation lives in the
// Rust knowledge core; here we only assert that the Go-side
// contract is implementable and that the input validation
// surfaces the documented errors.
type fakeContract struct {
	manifest      *shard.ShardManifest
	delta         shard.ShardDelta
	deltaApplied  bool
	retrieveResp  shard.LocalRetrievalResult
	retrieveErr   error
	forgetCount   int
	forgetTenants []string
}

func (f *fakeContract) SyncShard(_ context.Context, tenantID string, scope shard.ShardScope) (*shard.ShardManifest, error) {
	if tenantID == "" {
		return nil, errors.New("tenantID required")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return f.manifest, nil
}

func (f *fakeContract) ApplyDelta(_ context.Context, tenantID string, delta shard.ShardDelta) error {
	if tenantID == "" {
		return errors.New("tenantID required")
	}
	f.delta = delta
	f.deltaApplied = true
	return nil
}

func (f *fakeContract) LocalRetrieve(_ context.Context, tenantID string, q shard.LocalQuery) (shard.LocalRetrievalResult, error) {
	if tenantID == "" {
		return shard.LocalRetrievalResult{}, errors.New("tenantID required")
	}
	if q.Query == "" {
		return shard.LocalRetrievalResult{}, errors.New("query required")
	}
	if f.retrieveErr != nil {
		return shard.LocalRetrievalResult{}, f.retrieveErr
	}
	return f.retrieveResp, nil
}

func (f *fakeContract) CryptographicForget(_ context.Context, tenantID string) error {
	f.forgetCount++
	f.forgetTenants = append(f.forgetTenants, tenantID)
	return nil
}

func TestShardScope_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		scope   shard.ShardScope
		wantErr bool
	}{
		{"empty", shard.ShardScope{}, true},
		{"only-mode", shard.ShardScope{PrivacyMode: "internal"}, false},
		{"full", shard.ShardScope{UserID: "u", ChannelID: "c", PrivacyMode: "internal"}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.scope.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestShardClientContract_SyncShard(t *testing.T) {
	t.Parallel()
	manifest := &shard.ShardManifest{ID: "01", TenantID: "t", PrivacyMode: "internal", ShardVersion: 7}
	c := &fakeContract{manifest: manifest}

	if _, err := c.SyncShard(context.Background(), "", shard.ShardScope{PrivacyMode: "internal"}); err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
	if _, err := c.SyncShard(context.Background(), "t", shard.ShardScope{}); err == nil {
		t.Fatalf("expected error for empty privacy mode")
	}
	got, err := c.SyncShard(context.Background(), "t", shard.ShardScope{PrivacyMode: "internal"})
	if err != nil {
		t.Fatalf("SyncShard: %v", err)
	}
	if got != manifest {
		t.Fatalf("manifest mismatch")
	}
}

func TestShardClientContract_ApplyDelta_OrderingDocumented(t *testing.T) {
	t.Parallel()
	c := &fakeContract{}
	delta := shard.ShardDelta{
		From: 1, To: 2,
		Operations: []shard.DeltaOp{
			{Kind: shard.DeltaAdd, ChunkID: "new"},
			{Kind: shard.DeltaRemove, ChunkID: "old"},
		},
	}
	if err := c.ApplyDelta(context.Background(), "t", delta); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !c.deltaApplied {
		t.Fatalf("expected applied")
	}
	// The contract documents that adds are applied before removes.
	// We assert the wire ordering is preserved so an implementation
	// can rely on that ordering.
	if c.delta.Operations[0].Kind != shard.DeltaAdd {
		t.Fatalf("first op want add, got %v", c.delta.Operations[0].Kind)
	}
	if c.delta.Operations[1].Kind != shard.DeltaRemove {
		t.Fatalf("second op want remove, got %v", c.delta.Operations[1].Kind)
	}
}

func TestShardClientContract_LocalRetrieve(t *testing.T) {
	t.Parallel()
	c := &fakeContract{retrieveResp: shard.LocalRetrievalResult{
		Hits:          []shard.LocalHit{{ID: "1", Score: 0.9}},
		ShardVersion:  3,
		CoverageRatio: 0.8,
	}}
	if _, err := c.LocalRetrieve(context.Background(), "t", shard.LocalQuery{}); err == nil {
		t.Fatalf("expected error for empty query")
	}
	got, err := c.LocalRetrieve(context.Background(), "t", shard.LocalQuery{Query: "hello"})
	if err != nil {
		t.Fatalf("LocalRetrieve: %v", err)
	}
	if len(got.Hits) != 1 || got.Hits[0].ID != "1" {
		t.Fatalf("hits: %+v", got.Hits)
	}
	if got.CoverageRatio != 0.8 {
		t.Fatalf("coverage: %v", got.CoverageRatio)
	}
}

func TestShardClientContract_CryptographicForget_Idempotent(t *testing.T) {
	t.Parallel()
	c := &fakeContract{}
	for i := 0; i < 3; i++ {
		if err := c.CryptographicForget(context.Background(), "t"); err != nil {
			t.Fatalf("forget: %v", err)
		}
	}
	if c.forgetCount != 3 {
		t.Fatalf("count: %d", c.forgetCount)
	}
	for _, got := range c.forgetTenants {
		if got != "t" {
			t.Fatalf("tenant: %s", got)
		}
	}
}

// staticAssertContractImpl pins fakeContract to ShardClientContract
// at compile time. If the interface drifts, this line stops
// compiling and the contract change surfaces in code review.
var _ shard.ShardClientContract = (*fakeContract)(nil)
