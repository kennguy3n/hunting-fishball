// Package pipeline implements the Phase 1 4-stage ingestion pipeline:
//
//	Stage 1 Fetch  → Stage 2 Parse (gRPC → Docling) →
//	Stage 3 Embed  (gRPC → embedding service) → Stage 4 Store
//
// The package is structured around a coordinator that wires goroutines
// per stage with channel-based back-pressure (errgroup). The Kafka
// consumer (consumer.go) is the upstream driver that maps Kafka
// messages to coordinator submissions; the storage worker (store.go)
// is the downstream sink that writes to Qdrant + PostgreSQL.
//
// All stages key idempotency on `(tenant_id, document_id, content_hash)`
// per ARCHITECTURE.md §3.4. Re-processing the same content is a no-op
// at Stage 4; the coordinator short-circuits before re-parsing /
// re-embedding when the content_hash is unchanged.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// EventKind enumerates the high-level pipeline events the Kafka
// consumer routes into the coordinator.
type EventKind string

const (
	// EventDocumentChanged is emitted when a connector observes an
	// upstream document upsert / change. The coordinator runs the full
	// 4-stage pipeline.
	EventDocumentChanged EventKind = "source.document_changed"

	// EventDocumentDeleted is emitted when a connector observes an
	// upstream delete. The coordinator routes straight to Stage 4 to
	// purge derived state.
	EventDocumentDeleted EventKind = "source.document_deleted"

	// EventReindex is emitted by an admin re-index workflow. The
	// coordinator runs the full pipeline regardless of content_hash.
	EventReindex EventKind = "source.reindex"

	// EventPurge is emitted by an admin purge workflow (e.g. tenant
	// off-boarding). The coordinator routes to Stage 4 only.
	EventPurge EventKind = "source.purge"
)

// DocumentContentType is the coarse multi-modal class for a
// Document. Stage 2 (Parse) routes image / audio / video content
// to the appropriate parser. Round-19 Task 22 wires this end-to-
// end; future rounds will plug in real image / audio parsers
// (Docling multimodal, Whisper).
type DocumentContentType string

const (
	// ContentTypeText is the default — a plain-text, HTML, Markdown,
	// or office document destined for Docling text parsing.
	ContentTypeText DocumentContentType = "text"

	// ContentTypeImage routes to a (future) image parser that
	// extracts captions / OCR / EXIF metadata.
	ContentTypeImage DocumentContentType = "image"

	// ContentTypeAudio routes to a (future) audio parser that
	// emits a transcript via Whisper-style ASR.
	ContentTypeAudio DocumentContentType = "audio"

	// ContentTypeVideo routes to a (future) video parser that
	// emits a transcript + per-frame captions.
	ContentTypeVideo DocumentContentType = "video"
)

// ContentTypeFromMIME maps a MIME type into the corresponding
// DocumentContentType. Unknown MIME types fall through to
// ContentTypeText so Stage 2 retains the legacy behaviour.
// Round-19 Task 22.
func ContentTypeFromMIME(mime string) DocumentContentType {
	switch {
	case mime == "":
		return ContentTypeText
	case len(mime) >= 6 && mime[:6] == "image/":
		return ContentTypeImage
	case len(mime) >= 6 && mime[:6] == "audio/":
		return ContentTypeAudio
	case len(mime) >= 6 && mime[:6] == "video/":
		return ContentTypeVideo
	default:
		return ContentTypeText
	}
}

// Document is the unit of work flowing through the pipeline. It mirrors
// the connector.Document shape but flattens the Reader into a byte
// slice the parse stage can submit over gRPC.
//
// Document is *not* the same struct as connector.Document — connector
// implementations stream bytes from the upstream API; this struct
// holds the materialized payload after Stage 1 fetched it.
type Document struct {
	// TenantID scopes the document into a tenant. Required.
	TenantID string

	// SourceID identifies the connector instance the document came from.
	SourceID string

	// DocumentID is the connector-native identifier (mirrors
	// connector.DocumentRef.ID).
	DocumentID string

	// NamespaceID scopes the document inside the source.
	NamespaceID string

	// Title is the human-readable title.
	Title string

	// MIMEType is the upstream content type.
	MIMEType string

	// ContentType is the coarse multi-modal class derived from
	// MIMEType. Stage 2 routes image / audio / video content to
	// the appropriate parser; the default is text. Round-19
	// Task 22 wires it through the coordinator + persists it in
	// chunks (migration 042).
	ContentType DocumentContentType

	// Content is the raw fetched bytes. Stage 1 populates this slice;
	// downstream stages treat it as immutable.
	Content []byte

	// ContentHash is the SHA-256 hex digest of Content. Set by Stage 1.
	// Stage 4 keys idempotency on (tenant_id, document_id, content_hash).
	ContentHash string

	// PreviousHash, when non-empty, is the last-known content_hash for
	// (tenant_id, document_id). The coordinator short-circuits the
	// pipeline when ContentHash == PreviousHash unless the event
	// requests a force re-index.
	PreviousHash string

	// PrivacyLabel is propagated through every stage and surfaces in
	// the retrieval API as `privacy_label` so clients can render the
	// privacy strip without a second round-trip.
	PrivacyLabel string

	// IngestedAt is set by the consumer when the message is dequeued.
	IngestedAt time.Time

	// Metadata is connector-specific extras the coordinator passes
	// through (URL, path, labels).
	Metadata map[string]string

	// Force, when true, disables the content-hash short-circuit. Used
	// by the re-index workflow.
	Force bool
}

// Block is one parsed block emitted by Stage 2 (Docling) and consumed
// by Stage 3. Mirrors doclingv1.ParsedBlock with the wire-protocol
// fields the Go pipeline actually uses.
type Block struct {
	// BlockID is the Docling-assigned identifier (used by retrieval to
	// surface block-level provenance).
	BlockID string

	// Text is the human-readable content of the block. Stage 3 embeds
	// this directly.
	Text string

	// Type names the block kind ("paragraph", "heading", "table", ...).
	Type string

	// HeadingLevel is non-zero for headings; zero otherwise.
	HeadingLevel int32

	// Page records the source page for paginated documents.
	Page int32

	// Position is the zero-based ordinal within the document.
	Position int32

	// PrivacyLabel propagates from the document through every block so
	// that Stage 4 can persist it alongside the chunk.
	PrivacyLabel string
}

// StageInput is the unit-of-work threaded through the coordinator
// channels. The Doc field is always populated; ParsedBlocks and
// Embeddings are populated by Stage 2 and Stage 3 respectively.
type StageInput struct {
	Doc           *Document
	ParsedBlocks  []Block
	Embeddings    [][]float32
	EmbeddingDims int
	ModelID       string

	// ContentType mirrors Doc.ContentType for fast access in
	// Stage 2 / Stage 3 routing decisions. Round-19 Task 22.
	ContentType DocumentContentType
}

// StageOutput is what each stage emits when called outside the
// coordinator pipeline (e.g. unit tests of a stage in isolation).
type StageOutput struct {
	Out *StageInput
	Err error
}

// IngestEvent is the deserialized Kafka envelope the consumer hands to
// the coordinator. The wire format is JSON for Phase 1 — it's small,
// debuggable in `kafkacat`, and matches the audit log outbox.
type IngestEvent struct {
	// Kind classifies the event.
	Kind EventKind `json:"kind"`

	// TenantID, SourceID, DocumentID identify the (tenant, source,
	// document) tuple. The Kafka partition key is `tenant_id || source_id`
	// per ARCHITECTURE.md §3.2.
	TenantID    string `json:"tenant_id"`
	SourceID    string `json:"source_id"`
	DocumentID  string `json:"document_id"`
	NamespaceID string `json:"namespace_id,omitempty"`

	// FetchURL is the Stage-1 Fetch input — typically an HTTP(S) URL
	// or an `s3://` scheme. The connector populates it before
	// publishing the event.
	FetchURL string `json:"fetch_url,omitempty"`

	// Title and MIMEType propagate through to retrieval.
	Title    string `json:"title,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`

	// PrivacyLabel is the channel/source privacy label the retrieval
	// API surfaces back to clients.
	PrivacyLabel string `json:"privacy_label,omitempty"`

	// PreviousHash, when set, lets Stage 1 short-circuit on no-change.
	PreviousHash string `json:"previous_hash,omitempty"`

	// Metadata is connector-specific context.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Force disables the content-hash short-circuit. Re-index events
	// set this to true.
	Force bool `json:"force,omitempty"`

	// InlineContent, when non-empty, lets a connector skip the Stage-1
	// HTTP fetch entirely. Useful for Slack messages and other
	// already-in-memory payloads.
	InlineContent []byte `json:"inline_content,omitempty"`

	// SyncMode marks the event as a backfill or steady-state ingest.
	// The coordinator paces backfill events so they don't starve the
	// steady-state stream. Empty defaults to steady.
	SyncMode SyncMode `json:"sync_mode,omitempty"`

	// RequestID is the X-Request-ID stamped onto the originating
	// HTTP request by observability.RequestIDMiddleware. The
	// producer copies it into the Kafka envelope so every pipeline
	// stage logs the same correlation id and the DLQ can echo it
	// back into the audit row. Empty when the event was emitted
	// from a non-HTTP source (cron scheduler, webhook receiver).
	RequestID string `json:"request_id,omitempty"`
}

// SyncMode classifies an IngestEvent as backfill (initial seed of a
// newly-connected source) or steady-state (live updates from
// Subscribe webhooks). The coordinator paces backfill events through
// a separate token bucket so they don't starve the steady-state
// stream — see internal/pipeline/coordinator.go and
// internal/pipeline/backfill.go.
type SyncMode string

const (
	// SyncModeSteady is the default. Steady-state events are
	// scheduled at the connector's natural rate.
	SyncModeSteady SyncMode = "steady"
	// SyncModeBackfill marks events emitted by the backfill
	// orchestrator. The coordinator throttles them to a configurable
	// docs/sec budget.
	SyncModeBackfill SyncMode = "backfill"
)

// IdempotencyKey returns the (tenant_id, document_id, content_hash)
// triple the coordinator and the storage worker key idempotency on.
//
// Per ARCHITECTURE.md §3.4: every stage output is keyed on this triple,
// so re-processing a document with the same content is a no-op at the
// storage plane.
func IdempotencyKey(tenantID, documentID, contentHash string) string {
	return tenantID + "|" + documentID + "|" + contentHash
}

// HashContent returns the SHA-256 hex digest of b. Stage 1 calls this
// after a successful fetch; the result is the canonical content_hash
// every downstream stage and the storage layer key idempotency on.
func HashContent(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// ErrPoisonMessage is returned by stages when a document is structurally
// invalid (missing tenant_id, malformed Kafka envelope, ...). The
// coordinator routes such messages to the DLQ rather than retrying.
var ErrPoisonMessage = errors.New("pipeline: poison message")

// ErrUnchanged is returned by Stage 1 when the fetched content_hash
// matches the previous_hash. The coordinator skips downstream stages
// and commits the offset.
var ErrUnchanged = errors.New("pipeline: content unchanged")

// drainAndClose reads to EOF and closes rc. Used by Stage 1 after we
// have already read the body into memory.
func drainAndClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
