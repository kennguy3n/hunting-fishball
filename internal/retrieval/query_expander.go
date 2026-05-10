package retrieval

// query_expander.go — Round-6 Task 4.
//
// The query expander rewrites a user's query into one or more
// equivalent forms before fan-out so BM25 / memory backends can hit
// chunks the literal phrasing missed. The default
// SynonymExpander draws from a per-tenant synonym map that the admin
// portal maintains via POST /v1/admin/synonyms (see synonyms_handler.go).
//
// The expander is intentionally additive: it appends synonym-rich
// variants to the original query rather than replacing it. The
// merger is rank-based (RRF), so adding a synonym only ever helps —
// it cannot displace an exact-match hit unless the synonym actually
// matches more chunks.

import (
	"context"
	"strings"
	"sync"
	"unicode"
)

// SynonymStore is the read-side seam the SynonymExpander uses to
// look up per-tenant synonym sets. The default in-memory store
// (NewInMemorySynonymStore) is suitable for tests and small
// deployments; a Redis-backed store is wired by cmd/api when
// CONTEXT_ENGINE_SYNONYMS_REDIS is set.
type SynonymStore interface {
	Get(ctx context.Context, tenantID string) (map[string][]string, error)
	Set(ctx context.Context, tenantID string, synonyms map[string][]string) error
}

// SynonymExpander expands a query by appending each token's synonyms
// (limited to AppendLimit per token to avoid unbounded query bloat).
type SynonymExpander struct {
	store       SynonymStore
	appendLimit int
}

// NewSynonymExpander constructs a SynonymExpander. appendLimit caps
// the number of synonyms appended per token (default 4).
func NewSynonymExpander(store SynonymStore, appendLimit int) *SynonymExpander {
	if appendLimit <= 0 {
		appendLimit = 4
	}
	if store == nil {
		store = NewInMemorySynonymStore()
	}
	return &SynonymExpander{store: store, appendLimit: appendLimit}
}

// Expand implements QueryExpander. Returns the original query
// unchanged when no synonyms are configured for the tenant or when
// no token in the query has a synonym entry.
func (e *SynonymExpander) Expand(ctx context.Context, tenantID, query string) (string, error) {
	if e == nil || tenantID == "" || strings.TrimSpace(query) == "" {
		return query, nil
	}
	syns, err := e.store.Get(ctx, tenantID)
	if err != nil || len(syns) == 0 {
		return query, err
	}
	tokens := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	added := 0
	additions := make([]string, 0, e.appendLimit)
	for _, tok := range tokens {
		key := strings.ToLower(tok)
		alts, ok := syns[key]
		if !ok {
			continue
		}
		for _, alt := range alts {
			if added >= e.appendLimit {
				break
			}
			if alt == "" || strings.EqualFold(alt, key) {
				continue
			}
			additions = append(additions, alt)
			added++
		}
		if added >= e.appendLimit {
			break
		}
	}
	if len(additions) == 0 {
		return query, nil
	}
	return query + " " + strings.Join(additions, " "), nil
}

// InMemorySynonymStore is a process-local synonym store keyed by
// tenant_id. Values are deep-copied on read/write to keep the map
// safe under concurrent access.
type InMemorySynonymStore struct {
	mu   sync.RWMutex
	data map[string]map[string][]string
}

// NewInMemorySynonymStore constructs an empty InMemorySynonymStore.
func NewInMemorySynonymStore() *InMemorySynonymStore {
	return &InMemorySynonymStore{data: map[string]map[string][]string{}}
}

// Get returns a deep copy of the tenant's synonym map (empty when
// none configured).
func (s *InMemorySynonymStore) Get(_ context.Context, tenantID string) (map[string][]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.data[tenantID]
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		cp := append([]string(nil), v...)
		out[k] = cp
	}
	return out, nil
}

// Set replaces the tenant's synonym map atomically.
func (s *InMemorySynonymStore) Set(_ context.Context, tenantID string, synonyms map[string][]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if synonyms == nil {
		delete(s.data, tenantID)
		return nil
	}
	cp := make(map[string][]string, len(synonyms))
	for k, v := range synonyms {
		nv := append([]string(nil), v...)
		cp[strings.ToLower(strings.TrimSpace(k))] = nv
	}
	s.data[tenantID] = cp
	return nil
}
