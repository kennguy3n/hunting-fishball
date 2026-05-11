// reindex.go — Phase 8 / Task 7 reindex orchestrator.
//
// The reindex pipeline accepts a (tenant_id, source_id [, namespace_id])
// triple, walks the chunks table to enumerate the documents the
// (tenant, source [, namespace]) owns, and emits one EventReindex
// per document. The downstream consumer/coordinator path interprets
// EventReindex as "re-run Stages 2-4 (parse → embed → store) without
// re-fetching" — exactly the workload ARCHITECTURE.md §3.2 documents
// for the reindex topic.
//
// The orchestrator is a deliberately thin layer: it does not own the
// reindex stages themselves (those live in the existing coordinator).
// Its job is to fan a single admin click into N per-document events.
package pipeline

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// ReindexRequest is one admin reindex submission.
type ReindexRequest struct {
	TenantID    string
	SourceID    string
	NamespaceID string
	// DryRun, when true, enumerates the affected documents and
	// returns a summary without emitting any reindex events.
	// Round-6 Task 15.
	DryRun bool
}

// Validate enforces tenant_id + source_id are present.
func (r *ReindexRequest) Validate() error {
	if r == nil {
		return errors.New("reindex: nil request")
	}
	if r.TenantID == "" {
		return errors.New("reindex: missing tenant_id")
	}
	if r.SourceID == "" {
		return errors.New("reindex: missing source_id")
	}
	return nil
}

// ReindexEnumerator returns the document ids the reindex orchestrator
// must republish events for.
type ReindexEnumerator interface {
	ListDocuments(ctx context.Context, req ReindexRequest) ([]string, error)
}

// ReindexEmitter publishes one EventReindex per document.
type ReindexEmitter interface {
	EmitEvent(ctx context.Context, evt IngestEvent) error
}

// ReindexOrchestrator wires an enumerator + emitter and exposes
// Reindex(req) for the admin handler.
type ReindexOrchestrator struct {
	enum    ReindexEnumerator
	emitter ReindexEmitter
}

// NewReindexOrchestrator validates inputs.
func NewReindexOrchestrator(enum ReindexEnumerator, emitter ReindexEmitter) (*ReindexOrchestrator, error) {
	if enum == nil {
		return nil, errors.New("reindex: nil enumerator")
	}
	if emitter == nil {
		return nil, errors.New("reindex: nil emitter")
	}
	return &ReindexOrchestrator{enum: enum, emitter: emitter}, nil
}

// ReindexResult summarises a reindex run.
type ReindexResult struct {
	DocumentsEnumerated int
	EventsEmitted       int
	EmitErrors          int
	// DryRun mirrors the request flag; when true, EventsEmitted is
	// always zero and DocumentsEnumerated reports what *would*
	// have been emitted. Round-6 Task 15.
	DryRun bool
}

// Reindex enumerates documents and emits one EventReindex per row.
// Returns the number of events emitted; per-document emit failures
// are counted but do not abort the run so a single Kafka hiccup
// doesn't strand the rest of the source.
func (r *ReindexOrchestrator) Reindex(ctx context.Context, req ReindexRequest) (ReindexResult, error) {
	res := ReindexResult{DryRun: req.DryRun}
	if err := req.Validate(); err != nil {
		return res, err
	}
	docs, err := r.enum.ListDocuments(ctx, req)
	if err != nil {
		return res, fmt.Errorf("reindex: enumerate: %w", err)
	}
	res.DocumentsEnumerated = len(docs)
	if req.DryRun {
		return res, nil
	}
	for _, doc := range docs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		evt := IngestEvent{
			Kind:        EventReindex,
			TenantID:    req.TenantID,
			SourceID:    req.SourceID,
			NamespaceID: req.NamespaceID,
			DocumentID:  doc,
		}
		if err := r.emitter.EmitEvent(ctx, evt); err != nil {
			res.EmitErrors++
			continue
		}
		res.EventsEmitted++
	}
	return res, nil
}

// PostgresReindexEnumerator is the production *gorm.DB-backed
// ReindexEnumerator.
type PostgresReindexEnumerator struct{ db *gorm.DB }

// NewPostgresReindexEnumerator validates db.
func NewPostgresReindexEnumerator(db *gorm.DB) (*PostgresReindexEnumerator, error) {
	if db == nil {
		return nil, errors.New("reindex: nil db")
	}
	return &PostgresReindexEnumerator{db: db}, nil
}

// ListDocuments returns the distinct document_ids owned by the
// (tenant_id, source_id [, namespace_id]) triple in req.
func (e *PostgresReindexEnumerator) ListDocuments(ctx context.Context, req ReindexRequest) ([]string, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	q := e.db.WithContext(ctx).
		Model(&storage.Chunk{}).
		Where("tenant_id = ? AND source_id = ?", req.TenantID, req.SourceID).
		Distinct("document_id")
	if req.NamespaceID != "" {
		q = q.Where("namespace_id = ?", req.NamespaceID)
	}
	var docs []string
	if err := q.Pluck("document_id", &docs).Error; err != nil {
		return nil, fmt.Errorf("reindex: list documents: %w", err)
	}
	return docs, nil
}
