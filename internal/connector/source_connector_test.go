package connector_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// mockConnection implements connector.Connection.
type mockConnection struct {
	tenantID string
	sourceID string
}

func (m *mockConnection) TenantID() string { return m.tenantID }
func (m *mockConnection) SourceID() string { return m.sourceID }

// mockIterator implements connector.DocumentIterator over an in-memory
// slice of refs.
type mockIterator struct {
	refs   []connector.DocumentRef
	idx    int
	closed bool
}

func (m *mockIterator) Next(_ context.Context) bool {
	if m.idx >= len(m.refs) {
		return false
	}
	m.idx++

	return true
}

func (m *mockIterator) Doc() connector.DocumentRef {
	if m.idx == 0 || m.idx > len(m.refs) {
		return connector.DocumentRef{}
	}

	return m.refs[m.idx-1]
}

func (m *mockIterator) Err() error { return nil }

func (m *mockIterator) Close() error {
	m.closed = true

	return nil
}

// mockSubscription implements connector.Subscription.
type mockSubscription struct {
	events chan connector.DocumentChange
	err    error
}

func (m *mockSubscription) Events() <-chan connector.DocumentChange { return m.events }
func (m *mockSubscription) Err() error                              { return m.err }
func (m *mockSubscription) Close() error {
	close(m.events)

	return nil
}

// mockConnector is a minimal connector that satisfies SourceConnector
// without making any external calls. It exists to assert at compile-time
// that the interface is implementable from outside the package.
type mockConnector struct {
	validateErr error
}

func (m *mockConnector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
	if m.validateErr != nil {
		return m.validateErr
	}
	if cfg.Name == "" {
		return connector.ErrInvalidConfig
	}

	return nil
}

func (m *mockConnector) Connect(_ context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	return &mockConnection{tenantID: cfg.TenantID, sourceID: cfg.SourceID}, nil
}

func (m *mockConnector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "ns-1", Name: "Default", Kind: "drive"}}, nil
}

func (m *mockConnector) ListDocuments(
	_ context.Context, _ connector.Connection, _ connector.Namespace, _ connector.ListOpts,
) (connector.DocumentIterator, error) {
	return &mockIterator{
		refs: []connector.DocumentRef{
			{NamespaceID: "ns-1", ID: "doc-1", UpdatedAt: time.Unix(0, 0)},
			{NamespaceID: "ns-1", ID: "doc-2", UpdatedAt: time.Unix(1, 0)},
		},
	}, nil
}

func (m *mockConnector) FetchDocument(
	_ context.Context, _ connector.Connection, ref connector.DocumentRef,
) (*connector.Document, error) {
	return &connector.Document{
		Ref:      ref,
		MIMEType: "text/plain",
		Title:    "mock " + ref.ID,
		Content:  io_NopCloser("mock body"),
	}, nil
}

func (m *mockConnector) Subscribe(
	_ context.Context, _ connector.Connection, _ connector.Namespace,
) (connector.Subscription, error) {
	return &mockSubscription{events: make(chan connector.DocumentChange, 1)}, nil
}

func (m *mockConnector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// io_NopCloser avoids importing io just for ioutil.NopCloser; we wrap a
// strings.Reader. Keeps the test file self-contained.
type readCloser struct {
	*strings.Reader
}

func (readCloser) Close() error { return nil }

func io_NopCloser(s string) readCloser { return readCloser{strings.NewReader(s)} }

func TestMockConnectorImplementsSourceConnector(t *testing.T) {
	t.Parallel()

	var _ connector.SourceConnector = (*mockConnector)(nil)

	c := &mockConnector{}
	cfg := connector.ConnectorConfig{
		Name:     "mock",
		TenantID: "tenant-1",
		SourceID: "source-1",
	}
	ctx := context.Background()

	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "tenant-1" || conn.SourceID() != "source-1" {
		t.Fatalf("Connection identifiers not propagated: %+v", conn)
	}

	nss, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(nss) != 1 {
		t.Fatalf("ListNamespaces: %v %d", err, len(nss))
	}

	it, err := c.ListDocuments(ctx, conn, nss[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	count := 0
	for it.Next(ctx) {
		count++
		if it.Doc().ID == "" {
			t.Fatalf("empty doc ref at idx %d", count)
		}
	}
	if err := it.Close(); err != nil {
		t.Fatalf("iterator Close: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 docs, got %d", count)
	}

	doc, err := c.FetchDocument(ctx, conn, connector.DocumentRef{NamespaceID: "ns-1", ID: "doc-1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	if doc.MIMEType != "text/plain" {
		t.Fatalf("unexpected MIME: %q", doc.MIMEType)
	}
	if err := doc.Content.Close(); err != nil {
		t.Fatalf("doc Close: %v", err)
	}

	sub, err := c.Subscribe(ctx, conn, nss[0])
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if sub.Events() == nil {
		t.Fatalf("nil events channel")
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Subscription Close: %v", err)
	}

	if err := c.Disconnect(ctx, conn); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestValidateRejectsEmptyName(t *testing.T) {
	t.Parallel()

	c := &mockConnector{}
	err := c.Validate(context.Background(), connector.ConnectorConfig{})
	if !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestSentinelErrors(t *testing.T) {
	t.Parallel()

	for _, e := range []error{
		connector.ErrInvalidConfig,
		connector.ErrNotSupported,
		connector.ErrEndOfPage,
	} {
		if e == nil || e.Error() == "" {
			t.Fatalf("sentinel error has empty message")
		}
	}
}
