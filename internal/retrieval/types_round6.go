package retrieval

// types_round6.go declares the cross-file interfaces introduced in
// Round-6. Implementations live in dedicated files (chunk_acl.go,
// query_expander.go, ab_test.go) but the interface declarations sit
// here so the handler can reference them without depending on the
// implementation order.

import "context"

// ChunkACLEvaluator filters matches by per-chunk ACL tags after the
// snapshot ACL has been applied. Implementations are expected to be
// tenant-scoped via the supplied context (which carries the policy
// snapshot's chunk-ACL set). See chunk_acl.go for the concrete
// implementation.
//
// Round-6 Task 6.
type ChunkACLEvaluator interface {
	Filter(ctx context.Context, tenantID string, matches []*Match) (allowed []*Match, blocked int)
}

// QueryExpander rewrites a query into one or more equivalent forms
// before fan-out (Round-6 Task 4). The expander returns the original
// query unchanged when no synonym set applies.
type QueryExpander interface {
	Expand(ctx context.Context, tenantID, query string) (expanded string, err error)
}

// ABTestRouteResult is the routing decision the retrieval handler
// records on the response. The concrete admin.ABTestRouter
// (Round-6 Task 10) returns a richer struct with the variant
// config; the handler-side interface intentionally only exposes
// the arm name + experiment name so the retrieval package does
// not depend on `internal/admin`.
type ABTestRouteResult struct {
	ExperimentName string
	Arm            string
	Config         map[string]any
}

// ABTestRouter routes a retrieval request through either the
// control or the variant configuration based on a deterministic
// bucketing of (tenant_id, experiment_name, bucket_key). The
// concrete implementation lives in `internal/admin/ab_test.go`
// (Round-6 Task 10) and is adapted to this interface at wiring
// time.
//
// A nil result signals "no routing decision applies; use the
// caller's default config".
type ABTestRouter interface {
	Route(tenantID, experimentName, bucketKey string) (*ABTestRouteResult, error)
}
