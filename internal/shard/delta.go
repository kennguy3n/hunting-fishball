package shard

import "sort"

// DeltaKind classifies a delta operation.
type DeltaKind string

const (
	// DeltaAdd means the chunk is in the newer shard but was absent
	// from the prior version.
	DeltaAdd DeltaKind = "add"

	// DeltaRemove means the chunk was in the prior version but is
	// no longer present in the newer shard.
	DeltaRemove DeltaKind = "remove"
)

// DeltaOp is a single chunk-level change emitted by the delta
// endpoint. Clients apply adds first, removes second, to land at the
// server's current version.
type DeltaOp struct {
	Kind    DeltaKind `json:"kind"`
	ChunkID string    `json:"chunk_id"`
}

// ComputeDelta produces the ordered list of add/remove operations
// that turn `prev` into `curr`. Both slices are interpreted as sets
// (duplicates collapsed). The output is deterministic — sorted by
// (kind, chunk_id) — so two clients seeing the same input observe the
// same ordering.
func ComputeDelta(prev, curr []string) []DeltaOp {
	prevSet := make(map[string]struct{}, len(prev))
	for _, id := range prev {
		if id == "" {
			continue
		}
		prevSet[id] = struct{}{}
	}
	currSet := make(map[string]struct{}, len(curr))
	for _, id := range curr {
		if id == "" {
			continue
		}
		currSet[id] = struct{}{}
	}

	ops := make([]DeltaOp, 0)
	for id := range currSet {
		if _, in := prevSet[id]; !in {
			ops = append(ops, DeltaOp{Kind: DeltaAdd, ChunkID: id})
		}
	}
	for id := range prevSet {
		if _, in := currSet[id]; !in {
			ops = append(ops, DeltaOp{Kind: DeltaRemove, ChunkID: id})
		}
	}

	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Kind != ops[j].Kind {
			// "add" before "remove" so clients add the new chunks
			// first, then prune the gone ones — matters when an
			// add and a remove share the same chunk_id (shouldn't
			// happen, but the ordering guarantee is cheap).
			return ops[i].Kind == DeltaAdd
		}
		return ops[i].ChunkID < ops[j].ChunkID
	})
	return ops
}
