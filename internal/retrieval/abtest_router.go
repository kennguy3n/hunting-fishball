package retrieval

// abtest_router.go — Round-6 Task 10 wiring adapter.
//
// The concrete A/B-test router lives in `internal/admin/abtest.go`
// and returns a typed `*admin.ABTestRouteOutcome` whose `Arm` is an
// `admin.ABTestArm` enum. The retrieval handler consumes routing
// decisions through the narrow `retrieval.ABTestRouter` interface
// declared in types_round6.go to avoid leaking the admin enum
// across package boundaries. This file is the thin glue: it lets
// `cmd/api` (or any wiring site) plug an `*admin.ABTestRouter`
// straight into `HandlerConfig.ABTests` without each call site
// re-implementing the type translation.
//
// Keep this file deliberately tiny — any real logic belongs in the
// admin router or the retrieval handler, not in the adapter.

import "github.com/kennguy3n/hunting-fishball/internal/admin"

// ABTestRouterAdapter wraps an `*admin.ABTestRouter` so it
// satisfies the retrieval-side `ABTestRouter` interface. The
// adapter is goroutine-safe iff the wrapped router is (the admin
// implementation is, by way of its store).
type ABTestRouterAdapter struct {
	Router *admin.ABTestRouter
}

// Route implements retrieval.ABTestRouter. It forwards to the
// admin router and converts the typed outcome into the
// package-neutral ABTestRouteResult the retrieval handler
// understands.
//
// A nil wrapped router, an empty experiment name, an error from
// the admin router, or a "no active experiment" decision all
// collapse to `(nil, nil)` so the caller falls back to its
// default config — matching the contract on
// `admin.ABTestRouter.Route`.
func (a ABTestRouterAdapter) Route(tenantID, experimentName, bucketKey string) (*ABTestRouteResult, error) {
	if a.Router == nil {
		return nil, nil
	}
	out, err := a.Router.Route(tenantID, experimentName, bucketKey)
	if err != nil || out == nil {
		return nil, err
	}
	return &ABTestRouteResult{
		ExperimentName: out.ExperimentName,
		Arm:            string(out.Arm),
		Config:         out.Config,
	}, nil
}
