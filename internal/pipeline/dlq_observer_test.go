package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// readMetricCount returns the current value of the
// `context_engine_dlq_messages_total` counter for the supplied
// `original_topic`. We pull the value directly off the Prometheus
// registry rather than wrapping a fake collector — a wrapper would
// hide bugs in the production label tuple. tenant_id is
// deliberately not a Prometheus label (cardinality risk on a
// multi-tenant fleet); per-tenant breakdowns live in the structured
// log lines this test also asserts on.
func readMetricCount(t *testing.T, originalTopic string) float64 {
	t.Helper()
	mfs, err := observability.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "context_engine_dlq_messages_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["original_topic"] == originalTopic {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestDLQObserver_LogsAndCounts(t *testing.T) {
	t.Cleanup(observability.ResetForTest)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	obs, err := pipeline.NewDLQObserver(pipeline.DLQObserverConfig{
		Group:  fakeConsumerGroup{},
		Topic:  "ingest.dlq",
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewDLQObserver: %v", err)
	}

	envelope := pipeline.DLQEnvelope{
		Event: pipeline.IngestEvent{
			TenantID:   "tenant-a",
			SourceID:   "src-1",
			DocumentID: "doc-1",
		},
		Error:        "boom",
		FailedAt:     time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
		AttemptCount: 3,
	}
	body := pipeline.EncodeForTest(envelope)
	msg := &sarama.ConsumerMessage{
		Topic: "ingest.dlq",
		Value: body,
		Headers: []*sarama.RecordHeader{
			{Key: []byte("original_topic"), Value: []byte("ingest")},
		},
		Partition: 7,
		Offset:    42,
	}

	pipeline.ObserveMessageForTest(obs, msg)

	got := readMetricCount(t, "ingest")
	if got != 1 {
		t.Fatalf("counter=%v want 1", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, `"tenant_id":"tenant-a"`) {
		t.Errorf("log missing tenant_id: %s", logged)
	}
	if !strings.Contains(logged, `"document_id":"doc-1"`) {
		t.Errorf("log missing document_id: %s", logged)
	}
	if !strings.Contains(logged, `"original_topic":"ingest"`) {
		t.Errorf("log missing original_topic: %s", logged)
	}
	if !strings.Contains(logged, `"attempt_count":3`) {
		t.Errorf("log missing attempt_count: %s", logged)
	}
}

func TestDLQObserver_FallsBackToDLQTopicLabel(t *testing.T) {
	t.Cleanup(observability.ResetForTest)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	obs, err := pipeline.NewDLQObserver(pipeline.DLQObserverConfig{
		Group:  fakeConsumerGroup{},
		Topic:  "ingest.dlq",
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewDLQObserver: %v", err)
	}

	envelope := pipeline.DLQEnvelope{
		Event: pipeline.IngestEvent{TenantID: "tenant-z"},
		Error: "x",
	}
	body, _ := json.Marshal(envelope)
	msg := &sarama.ConsumerMessage{Topic: "ingest.dlq", Value: body}

	pipeline.ObserveMessageForTest(obs, msg)

	if got := readMetricCount(t, "ingest.dlq"); got != 1 {
		t.Fatalf("counter for fallback topic=%v want 1", got)
	}
}

func TestDLQObserver_UndecodableMessageStillIncrementsCounter(t *testing.T) {
	t.Cleanup(observability.ResetForTest)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	obs, err := pipeline.NewDLQObserver(pipeline.DLQObserverConfig{
		Group:  fakeConsumerGroup{},
		Topic:  "ingest.dlq",
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewDLQObserver: %v", err)
	}

	pipeline.ObserveMessageForTest(obs, &sarama.ConsumerMessage{
		Topic: "ingest.dlq",
		Value: []byte("not-json"),
	})

	if got := readMetricCount(t, "ingest.dlq"); got != 1 {
		t.Fatalf("counter for poison message=%v want 1", got)
	}
	if !strings.Contains(buf.String(), "undecodable envelope") {
		t.Errorf("log missing undecodable warning: %s", buf.String())
	}
}

func TestNewDLQObserver_ValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     pipeline.DLQObserverConfig
		wantErr string
	}{
		{
			name:    "missing group",
			cfg:     pipeline.DLQObserverConfig{Topic: "t", Logger: slog.Default()},
			wantErr: "nil Group",
		},
		{
			name:    "missing topic",
			cfg:     pipeline.DLQObserverConfig{Group: fakeConsumerGroup{}, Logger: slog.Default()},
			wantErr: "empty Topic",
		},
		{
			name:    "missing logger",
			cfg:     pipeline.DLQObserverConfig{Group: fakeConsumerGroup{}, Topic: "t"},
			wantErr: "nil Logger",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := pipeline.NewDLQObserver(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}

// fakeConsumerGroup satisfies sarama.ConsumerGroup with no-ops; the
// observer construction validates non-nil but the unit tests
// exercise observeMessage directly so the consumer never runs.
type fakeConsumerGroup struct{}

func (fakeConsumerGroup) Consume(_ context.Context, _ []string, _ sarama.ConsumerGroupHandler) error {
	return nil
}

func (fakeConsumerGroup) Errors() <-chan error        { return nil }
func (fakeConsumerGroup) Close() error                { return nil }
func (fakeConsumerGroup) Pause(_ map[string][]int32)  {}
func (fakeConsumerGroup) Resume(_ map[string][]int32) {}
func (fakeConsumerGroup) PauseAll()                   {}
func (fakeConsumerGroup) ResumeAll()                  {}
