package shard

import "context"

// VersionLookupRepo is the narrow contract VersionLookup needs from
// a shard repository. *Repository satisfies it via LatestVersion.
type VersionLookupRepo interface {
	LatestVersion(ctx context.Context, f ScopeFilter) (int64, error)
}

// VersionLookup adapts a shard repository to the retrieval
// package's ShardVersionLookup port: it converts the
// (tenantID, channelID, privacyMode) tuple into a ScopeFilter and
// delegates to LatestVersion.
//
// The adapter exists so the retrieval package can stay free of an
// import on internal/shard (which would create a cycle: shard's
// generation worker depends on retrieval-adjacent packages).
type VersionLookup struct {
	Repo VersionLookupRepo
}

// LatestShardVersion returns the freshest shard version for the
// supplied scope. Empty channel and privacy_mode broaden the
// search to the tenant-wide latest (matches LatestVersion's
// optional-field semantics). A repo error or a zero version both
// return 0.
func (l VersionLookup) LatestShardVersion(ctx context.Context, tenantID, channelID, privacyMode string) (int64, error) {
	if l.Repo == nil {
		return 0, nil
	}
	return l.Repo.LatestVersion(ctx, ScopeFilter{
		TenantID:    tenantID,
		ChannelID:   channelID,
		PrivacyMode: privacyMode,
	})
}
