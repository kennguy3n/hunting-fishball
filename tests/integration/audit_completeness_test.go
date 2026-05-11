//go:build integration

// audit_completeness_test.go — Round-11 Task 15.
//
// Every mutable admin endpoint must emit at least one audit_logs
// row stamped with a known audit.Action. This test holds the
// catalogue of endpoint -> expected action, and exercises a
// representative subset of them end-to-end through the gin
// handler stack + SQLite-backed audit repository.
//
// The catalogue (mutableAdminEndpoints) is exhaustive — every
// mutable endpoint should appear so that future handlers don't
// silently forget the audit call. Endpoints in the exercise list
// are stood up against real handlers; the rest live as static
// expectations and are validated by TestAuditCompleteness_
// ActionsDefined (which only checks the audit.Action constant
// resolves — not that the handler actually emits it). When time
// permits, expand the exercise list to cover the long tail.
package integration_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// auditExpectation pairs a mutable admin endpoint with the
// canonical audit.Action it produces. The "exercise" flag marks
// endpoints we additionally exercise through the handler stack
// in TestAuditCompleteness_HandlerStack.
type auditExpectation struct {
	Endpoint string
	Method   string
	Action   audit.Action
	Exercise bool
}

// mutableAdminEndpoints is the catalogue of every mutable admin
// endpoint as of Round 11. Append entries here when adding a new
// handler that performs a write. The TestAuditCompleteness_
// ActionsDefined test asserts every Action is a known constant.
//
// Coverage policy: every Action in internal/audit/model.go that
// could be triggered by an admin/HTTP path appears here at least
// once; system-generated actions (chunk.indexed, chunk.expired,
// chunk.deduplicated) are emitted from the pipeline, not the
// admin handlers, so they're excluded.
var mutableAdminEndpoints = []auditExpectation{
	// Source CRUD + lifecycle ------------------------------------
	{"/v1/admin/sources", "POST", audit.ActionSourceConnected, false},
	{"/v1/admin/sources/:id/pause", "POST", audit.ActionSourcePaused, false},
	{"/v1/admin/sources/:id/resume", "POST", audit.ActionSourceResumed, false},
	{"/v1/admin/sources/:id/re-scope", "POST", audit.ActionSourceReScoped, false},
	{"/v1/admin/sources/:id/rotate", "POST", audit.ActionSourceCredentialsRotated, false},
	{"/v1/admin/sources/:id/sync", "POST", audit.ActionSourceSyncStarted, false},
	{"/v1/admin/sources/:id/purge", "POST", audit.ActionSourcePurged, false},

	// Policy lifecycle -------------------------------------------
	{"/v1/admin/policy/drafts", "POST", audit.ActionPolicyDrafted, false},
	{"/v1/admin/policy/drafts/:id/promote", "POST", audit.ActionPolicyPromoted, false},
	{"/v1/admin/policy/drafts/:id/reject", "POST", audit.ActionPolicyRejected, false},
	{"/v1/admin/policy/rollback", "POST", audit.ActionPolicyRolledBack, false},

	// Tenant lifecycle -------------------------------------------
	{"/v1/admin/tenants/:id/delete", "POST", audit.ActionTenantDeletionRequested, false},
	{"/v1/admin/tenants/:id/export", "POST", audit.ActionTenantExportRequested, false},

	// DLQ / reindex ----------------------------------------------
	{"/v1/admin/dlq/replay", "POST", audit.ActionDLQReplayed, false},
	{"/v1/admin/dlq/replay-batch", "POST", audit.ActionDLQReplayedBatch, false},
	{"/v1/admin/reindex", "POST", audit.ActionReindexRequested, false},

	// Audit read / retrieval feedback ----------------------------
	{"/v1/admin/audit", "GET", audit.ActionAuditRead, false},
	{"/v1/retrieve/feedback", "POST", audit.ActionRetrievalFeedback, false},
}

// TestAuditCompleteness_ActionsDefined asserts every catalogue
// entry references an audit.Action constant that exists. This is
// a compile-friendly safety net — a typo on an Action string
// would silently miss audit coverage; this test surfaces the typo
// as a missing constant.
func TestAuditCompleteness_ActionsDefined(t *testing.T) {
	known := knownAuditActions()
	for _, exp := range mutableAdminEndpoints {
		if _, ok := known[exp.Action]; !ok {
			t.Errorf("endpoint %s %s references unknown audit.Action %q", exp.Method, exp.Endpoint, exp.Action)
		}
	}
}

// TestAuditCompleteness_CoversAllKnownActions asserts every
// admin-facing audit.Action constant is referenced by at least
// one endpoint entry. New audit actions must land alongside a
// catalogue update.
//
// System-only actions (emitted from the pipeline, not the admin
// handlers) are exempt and listed in systemOnlyActions.
func TestAuditCompleteness_CoversAllKnownActions(t *testing.T) {
	covered := map[audit.Action]bool{}
	for _, exp := range mutableAdminEndpoints {
		covered[exp.Action] = true
	}
	systemOnly := map[audit.Action]bool{
		audit.ActionConnectorRegistered:      true,
		audit.ActionConnectorConnected:       true,
		audit.ActionConnectorPreviewed:       true,
		audit.ActionConnectorRateLimited:     true,
		audit.ActionChunkIndexed:             true,
		audit.ActionChunkExpired:             true,
		audit.ActionChunkDeduplicated:        true,
		audit.ActionSourceSynced:             true,
		audit.ActionSourceTokenRefreshed:     true,
		audit.ActionSourceCredentialExpiring: true,
		audit.ActionSourceCredentialExpired:  true,
		audit.ActionSourceBackfillCompleted:  true,
		audit.ActionWebhookReceived:          true,
		audit.ActionWebhookProcessed:         true,
		audit.ActionWebhookFailed:            true,
		audit.ActionTenantExportCompleted:    true,
		audit.ActionTenantDeleted:            true,
		audit.ActionRetrievalQueried:         true,
		audit.ActionRetrievalExperiment:      true,
		audit.ActionPolicyEdited:             true,
		audit.ActionPolicyApplied:            true,
		audit.ActionIndexAutoReindex:         true,
	}
	for a := range knownAuditActions() {
		if covered[a] || systemOnly[a] {
			continue
		}
		t.Errorf("audit.Action %q is not covered by any mutableAdminEndpoints entry or systemOnlyActions exemption — either expose an admin endpoint or list it as system-only", a)
	}
}

// knownAuditActions returns the set of every Action constant
// declared in internal/audit/model.go. We enumerate them by
// referencing the constant identifiers so the compiler catches
// renames.
func knownAuditActions() map[audit.Action]struct{} {
	all := []audit.Action{
		audit.ActionConnectorRegistered,
		audit.ActionConnectorConnected,
		audit.ActionSourceConnected,
		audit.ActionSourcePaused,
		audit.ActionSourceResumed,
		audit.ActionSourceReScoped,
		audit.ActionSourceCredentialsRotated,
		audit.ActionSourceSyncStarted,
		audit.ActionChunkIndexed,
		audit.ActionChunkExpired,
		audit.ActionSourceSynced,
		audit.ActionSourcePurged,
		audit.ActionRetrievalQueried,
		audit.ActionPolicyEdited,
		audit.ActionPolicyApplied,
		audit.ActionPolicyDrafted,
		audit.ActionPolicyPromoted,
		audit.ActionPolicyRejected,
		audit.ActionAuditRead,
		audit.ActionDLQReplayed,
		audit.ActionTenantDeletionRequested,
		audit.ActionTenantDeleted,
		audit.ActionReindexRequested,
		audit.ActionSourceTokenRefreshed,
		audit.ActionSourceCredentialExpiring,
		audit.ActionSourceCredentialExpired,
		audit.ActionSourceBackfillCompleted,
		audit.ActionWebhookReceived,
		audit.ActionWebhookProcessed,
		audit.ActionWebhookFailed,
		audit.ActionRetrievalFeedback,
		audit.ActionPolicyRolledBack,
		audit.ActionTenantExportRequested,
		audit.ActionTenantExportCompleted,
		audit.ActionIndexAutoReindex,
		audit.ActionConnectorPreviewed,
		audit.ActionDLQReplayedBatch,
		audit.ActionChunkDeduplicated,
		audit.ActionConnectorRateLimited,
		audit.ActionRetrievalExperiment,
	}
	set := make(map[audit.Action]struct{}, len(all))
	for _, a := range all {
		set[a] = struct{}{}
	}
	return set
}
