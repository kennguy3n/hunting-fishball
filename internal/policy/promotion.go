package policy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// AuditWriter is the narrow contract the promotion workflow needs
// from the audit repository. Real wiring uses *audit.Repository;
// tests inject a recorder that runs Validate() so a missing-ID
// AuditLog cannot slip through.
type AuditWriter interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// LiveStore is the narrow contract the promotion workflow needs to
// apply a draft snapshot to the live policy tables. The
// implementation in cmd/api/main.go is GORM-backed; tests inject a
// recorder. ApplySnapshot MUST run inside the supplied tx so the
// promotion is atomic with the audit row + the draft state flip.
type LiveStore interface {
	ApplySnapshot(ctx context.Context, tx *gorm.DB, tenantID, channelID string, snap PolicySnapshot) error
}

// ErrPromotionBlocked is returned by PromoteDraft when the draft
// has any severity=error conflicts. The error wraps the conflict
// list so handlers can render it for the admin portal.
type ErrPromotionBlocked struct {
	Conflicts []PolicyConflict
}

// Error implements the error contract.
func (e *ErrPromotionBlocked) Error() string {
	return fmt.Sprintf("policy: draft promotion blocked by %d conflict(s)", len(e.Conflicts))
}

// PromotionConfig configures a Promoter.
type PromotionConfig struct {
	Drafts    *DraftRepository
	LiveStore LiveStore
	Audit     AuditWriter
}

// Promoter coordinates the four-step draft → live transition:
//
//  1. Load the draft, verify status=draft.
//  2. Run conflict detection — if any severity=error conflicts,
//     return ErrPromotionBlocked without mutating any state.
//  3. Apply the draft's PolicySnapshot to the live policy tables.
//  4. Mark draft status=promoted, set promoted_at and promoted_by.
//  5. Emit a policy.promoted audit event.
//
// Steps 3, 4, 5 run inside the same Postgres transaction so a crash
// between them leaves the system in either the pre-promotion state
// or the post-promotion state — never half-applied.
type Promoter struct {
	cfg PromotionConfig
}

// NewPromoter validates cfg and returns a Promoter.
func NewPromoter(cfg PromotionConfig) (*Promoter, error) {
	if cfg.Drafts == nil {
		return nil, errors.New("policy: promoter requires Drafts")
	}
	if cfg.LiveStore == nil {
		return nil, errors.New("policy: promoter requires LiveStore")
	}
	if cfg.Audit == nil {
		return nil, errors.New("policy: promoter requires Audit")
	}
	return &Promoter{cfg: cfg}, nil
}

// PromoteDraft is the public promotion entry point. Returns the
// post-flip Draft row on success.
func (p *Promoter) PromoteDraft(ctx context.Context, tenantID, draftID, actorID string) (*Draft, error) {
	if tenantID == "" {
		return nil, errors.New("policy: PromoteDraft requires tenantID")
	}
	if draftID == "" {
		return nil, errors.New("policy: PromoteDraft requires draftID")
	}

	d, err := p.cfg.Drafts.Get(ctx, tenantID, draftID)
	if err != nil {
		return nil, err
	}
	if d.Status != DraftStatusDraft {
		return nil, ErrDraftTerminal
	}

	conflicts := DetectConflicts(d.Payload.Snapshot)
	if HasErrors(conflicts) {
		return nil, &ErrPromotionBlocked{Conflicts: conflicts}
	}

	var promoted *Draft
	err = p.cfg.Drafts.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := p.cfg.LiveStore.ApplySnapshot(ctx, tx, d.TenantID, d.ChannelID, d.Payload.Snapshot); err != nil {
			return fmt.Errorf("policy: apply snapshot: %w", err)
		}
		updated, err := p.cfg.Drafts.MarkPromoted(ctx, tx, tenantID, draftID, actorID)
		if err != nil {
			return err
		}
		promoted = updated

		// audit.NewAuditLog mints a ULID and stamps CreatedAt; a
		// bare struct literal would fail audit.Repository.Create's
		// Validate step (missing ID), and the swallowed error
		// would silently drop every policy.promoted event in
		// production — exactly the bug PR #5 patched in the
		// forget worker.
		log := audit.NewAuditLog(
			tenantID,
			actorID,
			audit.ActionPolicyPromoted,
			"policy_draft",
			draftID,
			audit.JSONMap{
				"channel_id":  d.ChannelID,
				"created_by":  d.CreatedBy,
				"promoted_at": time.Now().UTC().Format(time.RFC3339Nano),
				"warnings":    len(conflicts),
			},
			"",
		)
		return p.cfg.Audit.Create(ctx, log)
	})
	if err != nil {
		return nil, err
	}
	return promoted, nil
}

// RejectDraft flips a draft to rejected status and emits a
// policy.rejected audit event. The reason is preserved on both the
// row and the audit metadata so an admin reviewing the audit feed
// sees why a draft was abandoned.
func (p *Promoter) RejectDraft(ctx context.Context, tenantID, draftID, actorID, reason string) (*Draft, error) {
	if tenantID == "" {
		return nil, errors.New("policy: RejectDraft requires tenantID")
	}
	if draftID == "" {
		return nil, errors.New("policy: RejectDraft requires draftID")
	}

	var rejected *Draft
	err := p.cfg.Drafts.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updated, err := p.cfg.Drafts.MarkRejected(ctx, tx, tenantID, draftID, actorID, reason)
		if err != nil {
			return err
		}
		rejected = updated

		log := audit.NewAuditLog(
			tenantID,
			actorID,
			audit.ActionPolicyRejected,
			"policy_draft",
			draftID,
			audit.JSONMap{
				"channel_id": updated.ChannelID,
				"reason":     reason,
			},
			"",
		)
		return p.cfg.Audit.Create(ctx, log)
	})
	if err != nil {
		return nil, err
	}
	return rejected, nil
}
