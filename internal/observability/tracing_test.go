package observability_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

func setup(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec
}

func TestStartSpanRecordsAttributes(t *testing.T) {
	rec := setup(t)
	_, span := observability.StartSpan(context.Background(), "test.span",
		observability.AttrTenantID.String("t1"),
		observability.AttrStage.String("fetch"),
	)
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	got := spans[0]
	if got.Name() != "test.span" {
		t.Fatalf("name: %q", got.Name())
	}
	attrs := got.Attributes()
	found := map[attribute.Key]string{}
	for _, a := range attrs {
		found[a.Key] = a.Value.AsString()
	}
	if found[observability.AttrTenantID] != "t1" {
		t.Fatalf("tenant_id missing: %+v", attrs)
	}
}

func TestRecordError(t *testing.T) {
	rec := setup(t)
	_, span := observability.StartSpan(context.Background(), "err.span")
	observability.RecordError(span, errors.New("boom"))
	span.End()
	spans := rec.Ended()
	if len(spans) != 1 || len(spans[0].Events()) == 0 {
		t.Fatalf("expected error event, got: %+v", spans)
	}
}

func TestTraceID(t *testing.T) {
	setup(t)
	ctx, span := observability.StartSpan(context.Background(), "x")
	defer span.End()
	if got := observability.TraceID(ctx); got == "" {
		t.Fatalf("trace_id empty")
	}
}
