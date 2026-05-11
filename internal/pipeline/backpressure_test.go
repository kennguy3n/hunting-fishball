package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// stubFetch / stubParse / stubEmbed / stubStore are minimal
// implementations sufficient for back-pressure metric tests; they
// don't drive realistic data through the pipeline.
type stubFetch struct{}

func (stubFetch) FetchEvent(_ context.Context, evt IngestEvent) (*Document, error) {
	return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID}, nil
}

type stubParse struct{}

func (stubParse) Parse(_ context.Context, _ *Document) ([]Block, error) {
	return []Block{}, nil
}

type stubEmbed struct{}

func (stubEmbed) EmbedBlocks(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
	return make([][]float32, len(blocks)), "stub-model", nil
}

type stubStore struct{}

func (stubStore) Store(_ context.Context, _ *Document, _ []Block, _ [][]float32, _ string) error {
	return nil
}
func (stubStore) Delete(_ context.Context, _, _ string) error { return nil }

// gaugeValue reads the current value of a gauge by label.
func gaugeValue(t *testing.T, name, labelName, labelValue string) float64 {
	t.Helper()
	mfs, err := observability.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !labelMatch(m.GetLabel(), labelName, labelValue) {
				continue
			}
			if m.Gauge != nil {
				return m.Gauge.GetValue()
			}
		}
	}
	return 0
}

func labelMatch(labels []*dto.LabelPair, name, value string) bool {
	for _, l := range labels {
		if l.GetName() == name && l.GetValue() == value {
			return true
		}
	}
	return false
}

func TestPipelineChannelDepth_RecordedOnSubmit(t *testing.T) {
	observability.ResetForTest()
	cfg := CoordinatorConfig{
		Fetch:     stubFetch{},
		Parse:     stubParse{},
		Embed:     stubEmbed{},
		Store:     stubStore{},
		QueueSize: 32,
	}
	coord, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// Push 4 events; we don't run the coordinator so they all
	// remain buffered in stage1.
	for i := 0; i < 4; i++ {
		if err := coord.Submit(ctx, IngestEvent{
			TenantID:   "t",
			SourceID:   "s",
			DocumentID: "d",
			Kind:       EventDocumentChanged,
		}); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	got := gaugeValue(t, "context_engine_pipeline_channel_depth", "stage", "fetch")
	if got != 4 {
		t.Fatalf("expected fetch depth=4, got %v", got)
	}
}
