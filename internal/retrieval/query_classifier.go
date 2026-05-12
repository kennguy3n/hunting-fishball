package retrieval

// Round-18 Task 10 — intelligent query routing.
//
// Classifies an incoming query into one of three high-level
// retrieval intents and emits per-source weights for the merger:
//
//   - QueryKindExact    — short, lexical, quote-bearing queries.
//                         Weight BM25 high, vector low.
//   - QueryKindSemantic — long-form / conceptual / question-shaped
//                         queries. Weight vector high, BM25 low.
//   - QueryKindEntity   — capitalized-noun-dominant queries that
//                         mention identifiable entities. Weight
//                         graph high, vector medium, BM25 low.
//
// The classifier is deliberately heuristic — the goal is to nudge
// the merger toward the right backend without a remote model on
// the hot path. The weights are passed into MergerConfig.PerSourceWeight
// when CONTEXT_ENGINE_QUERY_ROUTING_ENABLED is set; the default
// behaviour without the flag is to leave every weight at 1.0.

import (
	"os"
	"strings"
	"unicode"
)

// QueryKind identifies a classified intent.
type QueryKind string

const (
	QueryKindExact    QueryKind = "exact"
	QueryKindSemantic QueryKind = "semantic"
	QueryKindEntity   QueryKind = "entity"
)

// ClassifierConfig configures the weights emitted per intent.
// Each field maps the classified kind to per-source weights
// passed into MergerConfig.PerSourceWeight.
type ClassifierConfig struct {
	// ExactWeights overrides the default per-source weight when
	// the query is classified as QueryKindExact. Missing entries
	// fall back to defaults: BM25 = 1.5, vector = 0.5, graph =
	// 0.7, memory = 0.7.
	ExactWeights map[string]float32

	// SemanticWeights overrides the default per-source weight when
	// the query is classified as QueryKindSemantic. Missing
	// entries fall back to: vector = 1.5, BM25 = 0.5, graph = 0.7,
	// memory = 0.8.
	SemanticWeights map[string]float32

	// EntityWeights overrides the default per-source weight when
	// the query is classified as QueryKindEntity. Missing entries
	// fall back to: graph = 1.5, vector = 0.9, BM25 = 0.7,
	// memory = 0.7.
	EntityWeights map[string]float32

	// ExactWordCap is the upper bound on a query's word count for
	// it to be considered exact. Defaults to 4.
	ExactWordCap int

	// EntityCapWordRatio is the fraction of words that must start
	// with an uppercase letter for the query to be classified as
	// entity-driven. Defaults to 0.5.
	EntityCapWordRatio float64
}

// QueryClassifier is the high-level router. Construct with
// NewQueryClassifier and call Classify per request.
type QueryClassifier struct {
	cfg ClassifierConfig
}

// NewQueryClassifier returns a classifier with sensible defaults
// applied for any zero-valued field.
func NewQueryClassifier(cfg ClassifierConfig) *QueryClassifier {
	if cfg.ExactWeights == nil {
		cfg.ExactWeights = map[string]float32{
			SourceBM25:   1.5,
			SourceVector: 0.5,
			SourceGraph:  0.7,
			SourceMemory: 0.7,
		}
	}
	if cfg.SemanticWeights == nil {
		cfg.SemanticWeights = map[string]float32{
			SourceVector: 1.5,
			SourceBM25:   0.5,
			SourceGraph:  0.7,
			SourceMemory: 0.8,
		}
	}
	if cfg.EntityWeights == nil {
		cfg.EntityWeights = map[string]float32{
			SourceGraph:  1.5,
			SourceVector: 0.9,
			SourceBM25:   0.7,
			SourceMemory: 0.7,
		}
	}
	if cfg.ExactWordCap == 0 {
		cfg.ExactWordCap = 4
	}
	if cfg.EntityCapWordRatio == 0 {
		cfg.EntityCapWordRatio = 0.5
	}

	return &QueryClassifier{cfg: cfg}
}

// QueryRoutingEnabled returns true when the env feature flag is
// set. Retrieval handler call sites should treat the merger
// weights as-is when this is false.
func QueryRoutingEnabled() bool {
	v := os.Getenv("CONTEXT_ENGINE_QUERY_ROUTING_ENABLED")
	switch v {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true
	}

	return false
}

// Classify returns the intent and the corresponding per-source
// weight map. The weight map is safe to pass directly into
// MergerConfig.PerSourceWeight.
func (q *QueryClassifier) Classify(query string) (QueryKind, map[string]float32) {
	kind := classifyQuery(query, q.cfg.ExactWordCap, q.cfg.EntityCapWordRatio)
	switch kind {
	case QueryKindExact:
		return kind, cloneFloatMap(q.cfg.ExactWeights)
	case QueryKindEntity:
		return kind, cloneFloatMap(q.cfg.EntityWeights)
	default:
		return QueryKindSemantic, cloneFloatMap(q.cfg.SemanticWeights)
	}
}

// classifyQuery is a pure function so the classifier behaviour is
// trivially tested. The heuristics:
//
//   - Quote-bearing queries are exact-match (e.g. "exact phrase").
//   - Queries with ≤ ExactWordCap words and no question word are
//     exact-match.
//   - Queries whose words are ≥ EntityCapWordRatio capitalized are
//     entity queries.
//   - Everything else is semantic.
func classifyQuery(query string, exactWordCap int, entityCapRatio float64) QueryKind {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return QueryKindSemantic
	}
	if hasQuotes(trimmed) {
		return QueryKindExact
	}
	words := splitWords(trimmed)
	if len(words) == 0 {
		return QueryKindSemantic
	}
	lower := strings.ToLower(trimmed)
	hasQuestion := strings.HasSuffix(trimmed, "?") || startsWithQuestionWord(lower)

	// Check entity first — a short capitalized phrase like
	// "Acme Corp Q4 results" should route to the graph, not the
	// BM25 lane, even though it has few words.
	capCount := 0
	for _, w := range words {
		if startsUpper(w) && !isQuestionWord(strings.ToLower(w)) {
			capCount++
		}
	}
	ratio := float64(capCount) / float64(len(words))
	if !hasQuestion && ratio >= entityCapRatio && len(words) >= 2 {
		return QueryKindEntity
	}

	if !hasQuestion && len(words) <= exactWordCap {
		return QueryKindExact
	}

	return QueryKindSemantic
}

func hasQuotes(s string) bool {
	return strings.Contains(s, "\"") || strings.Contains(s, "“") || strings.Contains(s, "”")
}

func splitWords(s string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';'
	}) {
		f = strings.Trim(f, ".,;:!?\"'")
		if f != "" {
			out = append(out, f)
		}
	}

	return out
}

func startsUpper(s string) bool {
	if s == "" {
		return false
	}
	r := []rune(s)[0]

	return unicode.IsUpper(r)
}

var questionWords = map[string]bool{
	"what": true, "why": true, "how": true, "when": true, "where": true,
	"who": true, "which": true, "explain": true, "summarize": true,
	"describe": true, "compare": true, "list": true,
}

func startsWithQuestionWord(lower string) bool {
	for w := range questionWords {
		if strings.HasPrefix(lower, w+" ") {
			return true
		}
	}

	return false
}

func isQuestionWord(lower string) bool {
	return questionWords[lower]
}

func cloneFloatMap(in map[string]float32) map[string]float32 {
	out := make(map[string]float32, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}
