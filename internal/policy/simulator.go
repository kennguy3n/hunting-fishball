package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Simulator is the Phase 4 policy what-if engine. It runs the
// retrieval pipeline twice — once against the live PolicySnapshot
// resolved from Postgres, and once against a draft snapshot supplied
// by the caller — and returns both result sets plus a structured
// diff (added, removed, changed). The simulator never touches live
// state: the draft is materialised as a copy-on-write view via
// PolicySnapshot.Clone, so a draft mutation can never leak into the
// resolver's cache.
//
// The narrow Retriever port lets the simulator live in the policy
// package without importing retrieval (which already imports policy
// — adding a back-edge would create an import cycle). The admin
// handler wires a RetrieverFunc that re-runs the existing retrieval
// fan-out / merge / rerank / filter pipeline against the supplied
// snapshot.
type Simulator struct {
	cfg SimulatorConfig
}

// SimulatorConfig configures a Simulator.
type SimulatorConfig struct {
	// LiveResolver returns the live PolicySnapshot for a
	// (tenant, channel). The simulator runs the first retrieval
	// against this snapshot. Required.
	LiveResolver PolicyResolver

	// Retriever runs a single retrieval against an explicit
	// PolicySnapshot. The admin handler wires this to the existing
	// retrieval pipeline. Required.
	Retriever RetrieverFunc
}

// NewSimulator validates cfg and returns a Simulator.
func NewSimulator(cfg SimulatorConfig) (*Simulator, error) {
	if cfg.LiveResolver == nil {
		return nil, errors.New("policy: simulator requires LiveResolver")
	}
	if cfg.Retriever == nil {
		return nil, errors.New("policy: simulator requires Retriever")
	}
	return &Simulator{cfg: cfg}, nil
}

// RetrieverFunc runs a retrieval against an explicit PolicySnapshot.
// Implementations MUST honour the snapshot — e.g. enforce its ACL
// and recipient policy when filtering hits — so the simulator's
// draft path observes the same gates as the live path. Returning an
// error short-circuits the simulation; partial results are not
// meaningful for diff.
type RetrieverFunc func(ctx context.Context, req SimRetrieveRequest, snapshot PolicySnapshot) ([]RetrieveHit, error)

// SimRetrieveRequest is the inner retrieval request the simulator
// passes to the RetrieverFunc. It mirrors the public retrieval
// request shape but lives in the policy package to keep the import
// graph one-way.
type SimRetrieveRequest struct {
	TenantID  string
	ChannelID string
	UserID    string
	SkillID   string
	Query     string
	TopK      int
}

// RetrieveHit is the policy-package mirror of retrieval.RetrieveHit.
// The simulator returns hits in this shape so callers do not have to
// import retrieval — and so the policy package stays free of
// retrieval-pipeline transitive deps. Field names match
// retrieval.RetrieveHit so callers can JSON-marshal a WhatIfResult
// without re-projecting.
type RetrieveHit struct {
	ID           string         `json:"id"`
	Score        float32        `json:"score"`
	TenantID     string         `json:"tenant_id"`
	SourceID     string         `json:"source_id,omitempty"`
	NamespaceID  string         `json:"namespace_id,omitempty"`
	DocumentID   string         `json:"document_id,omitempty"`
	BlockID      string         `json:"block_id,omitempty"`
	Title        string         `json:"title,omitempty"`
	URI          string         `json:"uri,omitempty"`
	Path         string         `json:"path,omitempty"`
	PrivacyLabel string         `json:"privacy_label,omitempty"`
	Connector    string         `json:"connector,omitempty"`
	Sources      []string       `json:"sources,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// WhatIfRequest is the public input to Simulator.WhatIf. The
// DraftPolicy is the proposed snapshot the simulator evaluates
// against; the live snapshot is fetched through LiveResolver at run
// time so the diff is always against the freshest live state.
type WhatIfRequest struct {
	TenantID    string
	ChannelID   string
	UserID      string
	SkillID     string
	Query       string
	TopK        int
	DraftPolicy PolicySnapshot
}

// WhatIfResult is the structured output of Simulator.WhatIf. Added,
// Removed, and Changed are computed from (LiveResults, DraftResults)
// keyed on RetrieveHit.ID — set membership for added/removed,
// per-chunk diff for Changed.
type WhatIfResult struct {
	// LiveResults are the hits the live policy would have returned.
	LiveResults []RetrieveHit `json:"live_results"`

	// DraftResults are the hits the draft policy would return.
	DraftResults []RetrieveHit `json:"draft_results"`

	// Added lists hits present in DraftResults but absent in
	// LiveResults — chunks the draft would unblock.
	Added []RetrieveHit `json:"added"`

	// Removed lists hits present in LiveResults but absent in
	// DraftResults — chunks the draft would block.
	Removed []RetrieveHit `json:"removed"`

	// Changed lists hits whose score or privacy_label differs
	// between live and draft, identified by chunk ID.
	Changed []HitDiff `json:"changed"`
}

// HitDiff captures the per-chunk delta between live and draft for a
// hit that appears in both result sets. The simulator surfaces
// changes in score (rerank impact) and privacy_label (mode
// promotion / demotion impact) — the two fields most often inspected
// by admins evaluating policy changes.
type HitDiff struct {
	ID                string  `json:"id"`
	LiveScore         float32 `json:"live_score"`
	DraftScore        float32 `json:"draft_score"`
	LivePrivacyLabel  string  `json:"live_privacy_label"`
	DraftPrivacyLabel string  `json:"draft_privacy_label"`
}

// WhatIf runs the retrieval pipeline twice and returns the hits and
// structured diff. The live snapshot is fetched through LiveResolver
// once per call; the draft snapshot is the caller's WhatIfRequest
// .DraftPolicy. Both snapshots are deep-cloned before being passed
// to the Retriever so a buggy implementation cannot mutate live
// state.
func (s *Simulator) WhatIf(ctx context.Context, req WhatIfRequest) (*WhatIfResult, error) {
	if req.TenantID == "" {
		return nil, errors.New("policy: WhatIf requires TenantID")
	}
	if req.Query == "" {
		return nil, errors.New("policy: WhatIf requires Query")
	}

	live, err := s.cfg.LiveResolver.Resolve(ctx, req.TenantID, req.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("policy: resolve live snapshot: %w", err)
	}

	inner := SimRetrieveRequest{
		TenantID:  req.TenantID,
		ChannelID: req.ChannelID,
		UserID:    req.UserID,
		SkillID:   req.SkillID,
		Query:     req.Query,
		TopK:      req.TopK,
	}

	liveHits, err := s.cfg.Retriever(ctx, inner, live.Clone())
	if err != nil {
		return nil, fmt.Errorf("policy: live retrieval: %w", err)
	}
	draftHits, err := s.cfg.Retriever(ctx, inner, req.DraftPolicy.Clone())
	if err != nil {
		return nil, fmt.Errorf("policy: draft retrieval: %w", err)
	}

	res := &WhatIfResult{
		LiveResults:  liveHits,
		DraftResults: draftHits,
	}
	res.Added, res.Removed, res.Changed = diffHits(liveHits, draftHits)
	return res, nil
}

// diffHits computes (added, removed, changed) between live and draft
// hit slices. Membership keys on RetrieveHit.ID. Changed surfaces
// hits present in both lists where score or privacy_label differs.
// Output ordering is deterministic (sorted by ID) so test assertions
// don't have to special-case map-iteration order.
func diffHits(live, draft []RetrieveHit) (added, removed []RetrieveHit, changed []HitDiff) {
	liveByID := make(map[string]RetrieveHit, len(live))
	for _, h := range live {
		liveByID[h.ID] = h
	}
	draftByID := make(map[string]RetrieveHit, len(draft))
	for _, h := range draft {
		draftByID[h.ID] = h
	}

	for _, h := range draft {
		lh, inLive := liveByID[h.ID]
		if !inLive {
			added = append(added, h)
			continue
		}
		if lh.Score != h.Score || lh.PrivacyLabel != h.PrivacyLabel {
			changed = append(changed, HitDiff{
				ID:                h.ID,
				LiveScore:         lh.Score,
				DraftScore:        h.Score,
				LivePrivacyLabel:  lh.PrivacyLabel,
				DraftPrivacyLabel: h.PrivacyLabel,
			})
		}
	}
	for _, h := range live {
		if _, ok := draftByID[h.ID]; !ok {
			removed = append(removed, h)
		}
	}

	sort.Slice(added, func(i, j int) bool { return added[i].ID < added[j].ID })
	sort.Slice(removed, func(i, j int) bool { return removed[i].ID < removed[j].ID })
	sort.Slice(changed, func(i, j int) bool { return changed[i].ID < changed[j].ID })
	return added, removed, changed
}
