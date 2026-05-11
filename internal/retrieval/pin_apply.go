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
func ApplyPins(hits []RetrieveHit, pins []Pin) []RetrieveHit {
	if len(pins) == 0 || len(hits) == 0 {
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
	out := make([]RetrieveHit, 0, len(hits)+len(pinned))
	pinIdx := 0
	src := 0
	for i := 0; i < len(hits)+len(pinned); i++ {
		if pinIdx < len(pinned) && pinned[pinIdx].Position == i {
			if h, ok := known[pinned[pinIdx].ChunkID]; ok {
				out = append(out, h)
			} else {
				out = append(out, RetrieveHit{ID: pinned[pinIdx].ChunkID, Score: 1.0})
			}
			pinIdx++
			continue
		}
		if src >= len(hits) {
			break
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
