package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DataFlowDiff captures how a draft policy would change *where data
// goes* relative to the current live policy. Counts are bucketed by
// privacy_label (== compute tier: local-only, local-api, hybrid,
// remote, or any future label the privacy framework introduces).
//
// The simulator builds this by running the same query through both
// the live and draft policy snapshots and aggregating the result
// hits by their privacy_label. Delta is `Draft - Live` per bucket;
// positive means more chunks would route through that tier under
// the draft. A human-readable Summary surfaces the largest delta
// in percentage terms, suitable for embedding in an admin-portal
// confirmation step.
type DataFlowDiff struct {
	// Live is the per-tier count of chunks the live policy returned.
	Live map[string]int `json:"live"`

	// Draft is the per-tier count of chunks the draft policy
	// returned.
	Draft map[string]int `json:"draft"`

	// Delta is `Draft - Live` per tier. Tiers absent from one side
	// surface as the other side's signed value.
	Delta map[string]int `json:"delta"`

	// LiveTotal / DraftTotal are convenience aggregates. Returning
	// the total avoids forcing every UI client to re-sum the maps.
	LiveTotal  int `json:"live_total"`
	DraftTotal int `json:"draft_total"`

	// Summary is a human-readable narrative of the largest tier
	// shift, e.g. "draft routes 12% more matches through 'remote'."
	// Empty when no shift exceeds the rounding threshold.
	Summary string `json:"summary,omitempty"`
}

// SimulateDataFlow runs the live and draft retrievals and returns
// the per-privacy_label diff. The implementation mirrors WhatIf —
// resolver fetches the live snapshot, both runs go through the
// configured Retriever — but aggregates by tier rather than per
// chunk, since admins evaluating data-flow changes care about
// totals, not which specific chunks moved.
func (s *Simulator) SimulateDataFlow(ctx context.Context, req WhatIfRequest) (*DataFlowDiff, error) {
	if req.TenantID == "" {
		return nil, errors.New("policy: SimulateDataFlow requires TenantID")
	}
	if req.Query == "" {
		return nil, errors.New("policy: SimulateDataFlow requires Query")
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

	return BuildDataFlowDiff(liveHits, draftHits), nil
}

// BuildDataFlowDiff computes the per-tier counts and delta from two
// hit lists. Exposed as a free function (rather than only through
// SimulateDataFlow) so callers that already have hit lists in hand
// can compute the diff without re-running the retrieval — useful
// for the admin handler that reuses WhatIf's results.
func BuildDataFlowDiff(live, draft []RetrieveHit) *DataFlowDiff {
	d := &DataFlowDiff{
		Live:       map[string]int{},
		Draft:      map[string]int{},
		Delta:      map[string]int{},
		LiveTotal:  len(live),
		DraftTotal: len(draft),
	}
	for _, h := range live {
		d.Live[normalizeTier(h.PrivacyLabel)]++
	}
	for _, h := range draft {
		d.Draft[normalizeTier(h.PrivacyLabel)]++
	}

	tiers := map[string]struct{}{}
	for k := range d.Live {
		tiers[k] = struct{}{}
	}
	for k := range d.Draft {
		tiers[k] = struct{}{}
	}
	for tier := range tiers {
		d.Delta[tier] = d.Draft[tier] - d.Live[tier]
	}
	d.Summary = summarizeDataFlow(d)
	return d
}

// normalizeTier folds an empty privacy_label into the "unknown"
// bucket so the diff doesn't lose hits that ingest forgot to label.
// Callers can detect un-labelled hits by inspecting the bucket
// directly.
func normalizeTier(label string) string {
	if label == "" {
		return "unknown"
	}
	return label
}

// summarizeDataFlow renders the largest absolute-value delta as a
// human-readable line. Returns an empty string when every tier is
// within +/- 1 chunk (the rounding threshold) — at small sample
// sizes the percentage can otherwise mislead.
func summarizeDataFlow(d *DataFlowDiff) string {
	if d == nil {
		return ""
	}
	type entry struct {
		tier  string
		delta int
	}
	entries := make([]entry, 0, len(d.Delta))
	for tier, dl := range d.Delta {
		if dl == 0 {
			continue
		}
		entries = append(entries, entry{tier: tier, delta: dl})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		ai, bi := absInt(entries[i].delta), absInt(entries[j].delta)
		if ai != bi {
			return ai > bi
		}
		// Tie-break by preferring the tier where a percentage is
		// computable (Live > 0). Without this, two equal-magnitude
		// deltas pick the alphabetically-first tier — which is
		// often the brand-new bucket where Live == 0 and the
		// summary collapses to a raw count, hiding the intent of
		// the policy change.
		ip := d.Live[entries[i].tier] > 0
		jp := d.Live[entries[j].tier] > 0
		if ip != jp {
			return ip
		}
		return entries[i].tier < entries[j].tier
	})
	top := entries[0]
	if absInt(top.delta) <= 1 && d.LiveTotal+d.DraftTotal > 1 {
		return ""
	}

	pct := percentChange(d.Live[top.tier], d.Draft[top.tier])
	direction := "more"
	if top.delta < 0 {
		direction = "fewer"
	}
	var b strings.Builder
	if pct == "" {
		fmt.Fprintf(&b, "draft routes %d %s matches through %q", absInt(top.delta), direction, top.tier)
	} else {
		fmt.Fprintf(&b, "draft routes %s %s matches through %q", pct, direction, top.tier)
	}
	return b.String()
}

// percentChange formats `(draft - live) / live * 100` rounded to the
// nearest whole percent. Returns "" when live == 0 (every percent
// expression collapses to "infinite") so the caller falls back to
// raw counts.
func percentChange(live, draft int) string {
	if live == 0 {
		return ""
	}
	delta := draft - live
	if delta < 0 {
		delta = -delta
	}
	pct := (delta * 100) / live
	return fmt.Sprintf("%d%%", pct)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
