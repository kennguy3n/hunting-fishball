package retrieval

// abtest_apply.go — Round-6 Task 10 runtime glue.
//
// The retrieval handler routes requests through the configured
// `HandlerConfig.ABTests` at every public entrypoint (gin
// /v1/retrieve, RetrieveWithSnapshot, RetrieveWithSnapshotCached).
// This file contains the small helpers that handle the work each
// entrypoint shares: resolving the experiment, applying the
// variant arm's config overrides onto the request, and stamping
// the chosen arm onto the response envelope so analytics can
// attribute the result to the correct arm.

import "context"

// resolveExperiment runs the configured ABTestRouter (if any)
// against `req`'s experiment fields. It returns the routing
// decision (may be nil) and a request copy with any variant-arm
// config overrides applied. When `HandlerConfig.ABTests` is nil
// OR the request does not opt into an experiment OR the named
// experiment is missing / inactive, returns `(nil, req)` and the
// caller falls through to its default config.
//
// The bucket key defaults to `tenantID` when the request omits
// `ExperimentBucketKey` so the same tenant deterministically
// lands on the same arm across requests — matching the docstring
// on RetrieveRequest.ExperimentBucketKey.
func (h *Handler) resolveExperiment(tenantID string, req RetrieveRequest) (*ABTestRouteResult, RetrieveRequest) {
	if h.cfg.ABTests == nil || req.ExperimentName == "" || tenantID == "" {
		return nil, req
	}
	bucket := req.ExperimentBucketKey
	if bucket == "" {
		bucket = tenantID
	}
	route, err := h.cfg.ABTests.Route(tenantID, req.ExperimentName, bucket)
	if err != nil || route == nil {
		return nil, req
	}
	if route.Arm == "variant" && route.Config != nil {
		req = applyABTestConfig(req, route.Config)
	}
	return route, req
}

// applyABTestConfig overlays the variant arm's typed overrides
// onto the request. The variant config is a generic
// `map[string]any` (so an experiment can carry any JSON-shaped
// payload through the admin API), but the retrieval handler
// recognises a small, documented set of keys:
//
//   - "top_k"    (number) — overrides RetrieveRequest.TopK.
//   - "diversity" (number) — overrides RetrieveRequest.Diversity
//     (MMR lambda; see diversifier.go).
//
// Unknown keys are ignored so experiments can carry arbitrary
// payload for downstream analytics without breaking older
// handler versions. JSON unmarshals numeric values as `float64`,
// so we accept the int / float family explicitly rather than
// asserting a single concrete type.
func applyABTestConfig(req RetrieveRequest, cfg map[string]any) RetrieveRequest {
	if cfg == nil {
		return req
	}
	if v, ok := cfg["top_k"]; ok {
		if n, ok := numericAsInt(v); ok {
			req.TopK = n
		}
	}
	if v, ok := cfg["diversity"]; ok {
		if n, ok := numericAsFloat32(v); ok {
			req.Diversity = n
		}
	}
	return req
}

// numericAsInt converts a JSON-unmarshalled numeric `any` into
// an int. Returns ok=false for non-numeric values so callers can
// silently skip malformed overrides instead of panicking.
func numericAsInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// numericAsFloat32 mirrors numericAsInt for the float-typed
// override keys (currently `diversity`).
func numericAsFloat32(v any) (float32, bool) {
	switch n := v.(type) {
	case int:
		return float32(n), true
	case int32:
		return float32(n), true
	case int64:
		return float32(n), true
	case float32:
		return n, true
	case float64:
		return float32(n), true
	default:
		return 0, false
	}
}

// stampExperiment writes the route result onto resp.Policy. A nil
// route means "no experiment applied" and leaves the response
// fields zero-valued so non-experiment requests don't carry
// misleading attribution.
func stampExperiment(resp *RetrieveResponse, route *ABTestRouteResult) {
	if resp == nil || route == nil {
		return
	}
	resp.Policy.ExperimentName = route.ExperimentName
	resp.Policy.ExperimentArm = route.Arm
}

// expandQuery runs the configured QueryExpander (if any) against
// the user's query and returns the expanded form for fan-out.
// Errors and empty expansions degrade to the original query so a
// transient synonym-store failure cannot take retrieval offline.
//
// The embedding path stays on the ORIGINAL query — callers vec-
// embed before calling fan-out, and rerankers compare against
// the user-supplied query. Expansion only widens the BM25 /
// memory recall surface (Round-6 Task 4).
func (h *Handler) expandQuery(ctx context.Context, tenantID, query string) string {
	if h.cfg.QueryExpander == nil {
		return query
	}
	exp, err := h.cfg.QueryExpander.Expand(ctx, tenantID, query)
	if err != nil || exp == "" {
		return query
	}
	return exp
}
