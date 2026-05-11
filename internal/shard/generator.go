package shard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// ChunkScope is the per-chunk context the generator needs to ask the
// PolicyResolver "is this chunk eligible for the shard?". Mirrors
// the fields the retrieval handler's policy filter consumes so the
// generator and the live read path apply the same gates.
type ChunkScope struct {
	ChunkID      string
	DocumentID   string
	NamespaceID  string
	SourceID     string
	URI          string
	PrivacyLabel string
	Connector    string

	// Tags is the chunk's tag set, drawn from source metadata.
	// Used by the per-chunk ACL gate (Round-11 Task 8) to honour
	// chunk_acl rows with TagPrefix matchers at shard
	// pre-generation time. Empty when the chunk has no tags.
	Tags []string
}

// ChunkSource enumerates the chunks visible under a given (tenant,
// channel) scope. The generator iterates these and filters by the
// supplied PolicySnapshot. *storage.PostgresStore satisfies it
// through a small adapter in cmd/api/main.go and cmd/ingest/main.go.
type ChunkSource interface {
	ListChunks(ctx context.Context, tenantID string) ([]ChunkScope, error)
}

// EmbeddingSource fetches the per-chunk vectors. nil is acceptable —
// the generator just emits the chunk-ID set in that case.
type EmbeddingSource interface {
	FetchEmbeddings(ctx context.Context, tenantID string, chunkIDs []string) (map[string][]float32, error)
}

// PolicyGate is the narrow contract the generator needs from the
// policy package. *policy.LiveResolverGORM satisfies it directly.
type PolicyGate interface {
	Resolve(ctx context.Context, tenantID, channelID string) (policy.PolicySnapshot, error)
}

// GeneratorConfig configures the shard generator worker.
type GeneratorConfig struct {
	Repo       *Repository
	Chunks     ChunkSource
	Embeddings EmbeddingSource
	Policy     PolicyGate
}

// Generator produces a new shard manifest given a (tenant, user,
// channel, privacy_mode) scope. The struct is stateless apart from
// the configured ports.
type Generator struct {
	cfg GeneratorConfig
}

// NewGenerator validates cfg and returns a Generator.
func NewGenerator(cfg GeneratorConfig) (*Generator, error) {
	if cfg.Repo == nil {
		return nil, errors.New("shard: nil Repo")
	}
	if cfg.Chunks == nil {
		return nil, errors.New("shard: nil Chunks")
	}
	if cfg.Policy == nil {
		return nil, errors.New("shard: nil Policy")
	}
	return &Generator{cfg: cfg}, nil
}

// GenerateRequest narrows the work the generator does for a single
// run. PrivacyMode is required; UserID and ChannelID are optional
// (empty = tenant-wide).
type GenerateRequest struct {
	TenantID    string
	UserID      string
	ChannelID   string
	PrivacyMode string
}

// GenerateResult is the outcome of a successful Generate call.
type GenerateResult struct {
	Manifest   *ShardManifest
	ChunkIDs   []string
	Embeddings map[string][]float32
}

// Generate walks the configured ChunkSource, asks the PolicyGate
// which chunks are eligible for the (tenant, channel, privacy_mode)
// scope, persists a new manifest + chunk_id rows, and returns the
// freshly-minted manifest.
//
// The returned manifest is in `ready` state. Older manifests for the
// same scope are flipped to `superseded`.
func (g *Generator) Generate(ctx context.Context, req GenerateRequest) (*GenerateResult, error) {
	if req.TenantID == "" {
		return nil, ErrMissingTenantScope
	}
	if req.PrivacyMode == "" {
		return nil, errors.New("shard: PrivacyMode required")
	}
	scope := ScopeFilter{
		TenantID:    req.TenantID,
		UserID:      req.UserID,
		ChannelID:   req.ChannelID,
		PrivacyMode: req.PrivacyMode,
	}

	// Resolve the policy snapshot once for the whole shard scope.
	snap, err := g.cfg.Policy.Resolve(ctx, req.TenantID, req.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("shard: resolve policy: %w", err)
	}

	// Pull every candidate chunk for the tenant. The chunk source
	// returns chunks across every (channel, source) — the policy
	// filter narrows to the shard scope.
	candidates, err := g.cfg.Chunks.ListChunks(ctx, req.TenantID)
	if err != nil {
		return nil, fmt.Errorf("shard: list chunks: %w", err)
	}

	eligibleIDs := make([]string, 0, len(candidates))
	for _, ch := range candidates {
		if !privacyAllows(req.PrivacyMode, ch.PrivacyLabel) {
			continue
		}
		if !aclAllows(snap, ch) {
			continue
		}
		// Round-11 Task 8: consult chunk_acl (per-chunk tag
		// rules) AFTER the source-level allow/deny list so a
		// single chunk inside an otherwise-allowed source can
		// be denied (or vice-versa). The retrieval-time gate
		// reapplies this check, but filtering at the shard
		// generator avoids materialising chunks the live path
		// would later strip.
		if !chunkACLAllows(snap, ch) {
			continue
		}
		eligibleIDs = append(eligibleIDs, ch.ChunkID)
	}
	sort.Strings(eligibleIDs)

	// Allocate the next version under this scope.
	prevVersion, err := g.cfg.Repo.LatestVersion(ctx, scope)
	if err != nil {
		return nil, err
	}
	nextVersion := prevVersion + 1

	manifest := &ShardManifest{
		ID:           ulid.Make().String(),
		TenantID:     req.TenantID,
		UserID:       req.UserID,
		ChannelID:    req.ChannelID,
		PrivacyMode:  req.PrivacyMode,
		ShardVersion: nextVersion,
		ChunksCount:  len(eligibleIDs),
		Status:       ShardStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := g.cfg.Repo.Create(ctx, manifest); err != nil {
		return nil, fmt.Errorf("shard: create manifest: %w", err)
	}
	if err := g.cfg.Repo.SetChunkIDs(ctx, req.TenantID, manifest.ID, eligibleIDs); err != nil {
		_ = g.cfg.Repo.MarkFailed(ctx, req.TenantID, manifest.ID)
		return nil, fmt.Errorf("shard: persist chunk ids: %w", err)
	}
	if err := g.cfg.Repo.MarkReady(ctx, req.TenantID, manifest.ID, len(eligibleIDs)); err != nil {
		return nil, err
	}
	manifest.Status = ShardStatusReady
	manifest.ChunksCount = len(eligibleIDs)

	// Mark prior versions superseded so the API surface only
	// returns the freshest manifest.
	if err := g.cfg.Repo.MarkSuperseded(ctx, scope, nextVersion); err != nil {
		return nil, err
	}

	var embeddings map[string][]float32
	if g.cfg.Embeddings != nil && len(eligibleIDs) > 0 {
		embeddings, err = g.cfg.Embeddings.FetchEmbeddings(ctx, req.TenantID, eligibleIDs)
		if err != nil {
			// Embeddings are optional — log but don't fail the run.
			embeddings = nil
		}
	}

	return &GenerateResult{
		Manifest:   manifest,
		ChunkIDs:   eligibleIDs,
		Embeddings: embeddings,
	}, nil
}

// privacyAllows returns true when a chunk's PrivacyLabel is permitted
// under the supplied PrivacyMode. The encoding mirrors
// internal/policy/privacy_mode.go.
//
// Each chunk's privacy_label states the strictest privacy regime
// the chunk is willing to flow under (e.g. "no_ai" — only carry me
// if the shard itself is no_ai). A shard for `mode` admits a chunk
// only when `mode` is AT LEAST AS STRICT as `label`.
//
// Empty chunk labels are treated as permissive — they fall through
// the gate. Empty mode (shouldn't happen given the upstream
// validation) also defaults to permissive.
//
// Both names ("internal", "no_ai", ...) and canonical privacy-mode
// values ("local-only", "hybrid", ...) are accepted because the
// chunk privacy_label column historically allowed both forms.
func privacyAllows(mode, label string) bool {
	if mode == "" || label == "" {
		return true
	}
	// Lower number = stricter.
	rank := map[string]int{
		// Canonical PrivacyMode values from
		// internal/policy/privacy_mode.go (strict → permissive).
		"no-ai":      0,
		"no_ai":      0,
		"local-only": 1,
		"local":      1,
		"local-api":  2,
		"hybrid":     3,
		"internal":   3,
		"remote":     4,
		"public":     4,
	}
	modeRank, modeOK := rank[mode]
	labelRank, labelOK := rank[label]
	if !modeOK || !labelOK {
		// Unknown values fall through permissively to avoid bricking
		// shards on a mislabeled chunk; the retrieval-time gate
		// re-applies the strict check.
		return true
	}
	// A chunk is admitted when the shard's mode is AT LEAST as
	// strict as the chunk's required label — i.e. the shard never
	// widens a chunk's policy.
	return modeRank <= labelRank
}

// aclAllows asks the PolicySnapshot's allow/deny list whether the
// chunk fits the shard scope. Recipient policy is not consulted here
// because the shard generator does not know the recipient skill — it
// ships every chunk eligible by ACL and the retrieval-time gate
// handles per-skill filtering.
func aclAllows(snap policy.PolicySnapshot, ch ChunkScope) bool {
	if snap.ACL == nil {
		return true
	}
	verdict := snap.ACL.Evaluate(policy.ChunkAttrs{
		SourceID:    ch.SourceID,
		NamespaceID: ch.NamespaceID,
		Path:        ch.URI,
	})
	return verdict.Allowed
}

// chunkACLAllows asks the PolicySnapshot's per-chunk ACL whether
// the chunk fits the shard scope (Round-11 Task 8). Mirrors the
// retrieval handler's chunk-level gate so the shard pre-generator
// never ships a chunk the live read path would later block.
//
// Returns true when:
//   - snap.ChunkACL is nil (no per-chunk rules wired), OR
//   - snap.ChunkACL has no rules touching the chunk (default allow), OR
//   - a matching rule returned ChunkACLDecisionAllow.
//
// Returns false when a matching rule explicitly denies the chunk.
func chunkACLAllows(snap policy.PolicySnapshot, ch ChunkScope) bool {
	if snap.ChunkACL == nil {
		return true
	}
	decision := snap.ChunkACL.Evaluate(policy.ChunkACLAttrs{
		ChunkID: ch.ChunkID,
		Tags:    ch.Tags,
	})
	return decision != policy.ChunkACLDecisionDeny
}
