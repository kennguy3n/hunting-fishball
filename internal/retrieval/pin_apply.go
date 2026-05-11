// pin_apply.go — Round-7 Task 17.
//
// ApplyPins merges operator-pinned chunks into the merged hit
// list at their configured positions. The retrieval handler
// invokes this after policy filtering and before diversity /
// topK truncation.
//
// To avoid an import cycle with the admin package (which owns
// the pin CRUD store), the retrieval side defines its own
// minimal `Pin` shape and works in terms of (chunk_id, position)
// tuples — the cmd/api wiring layer translates between
// `admin.PinnedResult` and `retrieval.Pin`.
package retrieval

import "sort"

// Pin is the minimal projection of admin.PinnedResult that the
// retrieval merge function needs.
type Pin struct {
	ChunkID  string
	Position int
}

// ApplyPins merges pins into hits, displacing dynamic hits when
// the position overlaps. Pinned chunks that are also present in
// the hit list at a different position are kept only at the pin
// position to avoid duplication.
//
// An empty hit list is intentionally NOT a short-circuit: pinning
// is most valuable in cold-start scenarios (empty corpora, no-match
// queries) where the dynamic backends return zero hits. The
// synthetic RetrieveHit branch below handles pins that aren't in
// `known` without requiring the hit list to be populated.
//
// The merge loop iterates up to max(len(hits)+len(pinned),
// lastPinPosition+1) so pins configured at sparse positions
// (e.g. Position=5 with len(hits)=0) still emit at their
// configured slot. Slots between the last dynamic hit and the
// next pin are skipped without padding so callers don't see
// zero-valued RetrieveHits — pinned ordering, not contiguous
// indices, is the invariant.
func ApplyPins(hits []RetrieveHit, pins []Pin) []RetrieveHit {
	if len(pins) == 0 {
		return hits
	}
	known := map[string]RetrieveHit{}
	for _, h := range hits {
		known[h.ID] = h
	}
	pinned := make([]Pin, len(pins))
	copy(pinned, pins)
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].Position < pinned[j].Position })
	pinSet := map[string]int{}
	for _, p := range pinned {
		pinSet[p.ChunkID] = p.Position
	}
	maxIdx := len(hits) + len(pinned)
	if last := pinned[len(pinned)-1].Position + 1; last > maxIdx {
		maxIdx = last
	}
	out := make([]RetrieveHit, 0, maxIdx)
	pinIdx := 0
	src := 0
	for i := 0; i < maxIdx; i++ {
		if pinIdx < len(pinned) && pinned[pinIdx].Position == i {
			if h, ok := known[pinned[pinIdx].ChunkID]; ok {
				out = append(out, h)
			} else {
				out = append(out, RetrieveHit{ID: pinned[pinIdx].ChunkID, Score: 1.0})
			}
			pinIdx++
			continue
		}
		// Both sides exhausted — nothing left to emit.
		if pinIdx >= len(pinned) && src >= len(hits) {
			break
		}
		// Dynamic side exhausted but more pins remain at later
		// positions; skip ahead without emitting padding.
		if src >= len(hits) {
			continue
		}
		for src < len(hits) {
			h := hits[src]
			if pos, ok := pinSet[h.ID]; ok && pos != i {
				src++
				continue
			}
			out = append(out, h)
			src++
			break
		}
	}
	return out
}
