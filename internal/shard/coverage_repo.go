package shard

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// CoverageRepoGORM implements CoverageRepo against the canonical
// chunks table written by storage.PostgresStore. The handler's
// coverage endpoint divides the shard's `chunks_count` (numerator)
// by the tenant's total chunk count (denominator) returned here, so
// the on-device client can decide whether the local shard is
// authoritative enough to skip the remote round-trip — see
// `docs/contracts/local-first-retrieval.md` §"Coverage authority".
//
// The repo intentionally lives in package shard rather than package
// storage so the shard handler can declare a CoverageRepo
// dependency without pulling the broader storage package into its
// import graph (the shard package is a leaf the API binary wires
// late). Plumbing back through *gorm.DB keeps the production
// constructor a one-liner.
type CoverageRepoGORM struct {
	db *gorm.DB
}

// NewCoverageRepoGORM constructs a CoverageRepoGORM from a *gorm.DB.
// The db is used read-only; tenant scope is enforced on every query.
func NewCoverageRepoGORM(db *gorm.DB) *CoverageRepoGORM {
	return &CoverageRepoGORM{db: db}
}

// CorpusChunkCount returns the total number of chunks recorded for
// the supplied scope. The scope MUST carry a TenantID; we return
// ErrMissingTenantScope rather than counting the global table to
// guarantee a tenant filter is applied before any query runs.
//
// PrivacyMode, when present on the filter, narrows the count to
// chunks tagged with the matching `privacy_label`. The naming
// difference is deliberate: the policy/shard layer thinks of
// "privacy mode" while the storage layer persists the corresponding
// "privacy label" on each chunk row. Treating them as the same
// string mirrors the behaviour of the retrieval handler's privacy
// gate — see internal/retrieval/policy.go.
//
// A zero count is a valid result (a tenant that has never indexed
// anything yet); callers distinguish "no chunks" from "shard
// not authoritative" via the manifest's ShardVersion.
func (r *CoverageRepoGORM) CorpusChunkCount(ctx context.Context, f ScopeFilter) (int, error) {
	if f.TenantID == "" {
		return 0, ErrMissingTenantScope
	}
	if r.db == nil {
		return 0, errors.New("shard: nil db")
	}
	var n int64
	q := r.db.WithContext(ctx).Table("chunks").Where("tenant_id = ?", f.TenantID)
	if f.PrivacyMode != "" {
		q = q.Where("privacy_label = ?", f.PrivacyMode)
	}
	if err := q.Count(&n).Error; err != nil {
		return 0, fmt.Errorf("shard: corpus chunk count: %w", err)
	}
	return int(n), nil
}

// Compile-time check.
var _ CoverageRepo = (*CoverageRepoGORM)(nil)
