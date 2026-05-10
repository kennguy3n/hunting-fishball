// Package observability centralises tracing helpers for the
// hunting-fishball control plane.
//
// The package wraps go.opentelemetry.io/otel so the rest of the
// codebase imports `observability` rather than the deeper otel
// packages. This keeps the migration to a different tracing backend
// (Honeycomb, Tempo, etc.) localised to this package, and lets us
// inject test-time providers via a single `SetTracerProvider` call.
//
// Stages and backends call `StartSpan` to open a span with an
// idiomatic name (e.g. "pipeline.fetch", "retrieval.vector"), record
// attributes via the returned span, and call `End()` when done.
package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the otel tracer name used for the entire control
// plane. Callers should NOT pick their own — having a single tracer
// name means a sampling rule applied to "context-engine" filters all
// spans.
const TracerName = "context-engine"

// Tracer returns the package-level tracer.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// StartSpan opens a new span under the package tracer and returns
// the updated context + span. Callers MUST call span.End() — usually
// via defer.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx, span := Tracer().Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return ctx, span
}

// AttrTenantID is the canonical attribute key for the hunting-fishball
// tenant ID.
const AttrTenantID = attribute.Key("tenant_id")

// AttrDocumentID is the canonical attribute key for the document ID
// flowing through the pipeline.
const AttrDocumentID = attribute.Key("document_id")

// AttrBackend is the canonical attribute key for the retrieval
// backend (vector|bm25|graph|memory).
const AttrBackend = attribute.Key("backend")

// AttrHitCount is the canonical attribute key for the number of
// candidates returned by a retrieval backend.
const AttrHitCount = attribute.Key("hit_count")

// AttrLatencyMs records latency in milliseconds.
const AttrLatencyMs = attribute.Key("latency_ms")

// AttrStage is the canonical attribute key for a pipeline stage
// (fetch|parse|embed|store).
const AttrStage = attribute.Key("stage")

// RecordError marks the span as errored without re-panicking.
// Callers can pass nil safely.
func RecordError(span trace.Span, err error) {
	if err == nil || span == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// TraceID returns the trace_id of the span on ctx, or an empty
// string when no span is active. Used by retrieval handlers that
// echo trace_id back in their JSON responses.
func TraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return ""
	}
	return span.SpanContext().TraceID().String()
}
