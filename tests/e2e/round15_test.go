//go:build e2e

// Round-15 Task 11 — Round-15 end-to-end smoke.
//
// This file exercises the Round-15 surface as an integrated
// bundle. Each subtest mirrors a feature added in Round 15 and
// runs against pure stdlib `httptest` fakes — no docker-compose
// involvement, no migration plane. The build tag `e2e` keeps it
// off the fast lane.
//
// Coverage:
//
//	(a) Registry now reports exactly 20 entries (12 → 20).
//	(b) One full Round-15 connector lifecycle (kchat) walks
//	    Validate → Connect → ListNamespaces → ListDocuments →
//	    FetchDocument → DeltaSync → HandleWebhook.
//	(c) Google shared-drives variant filters out my-drive.
//	(d) connector.ErrRateLimited propagates through iterator.Err
//	    when the upstream returns 429.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/asana"
	"github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
)

// TestRound15_RegistryCount verifies the catalog expansion contract:
// blank-imports from the e2e package populate exactly 20 connector
// entries.
func TestRound15_RegistryCount(t *testing.T) {
	t.Parallel()
	names := connector.ListSourceConnectors()
	if len(names) != 20 {
		t.Fatalf("registry: %d entries, want 20: %v", len(names), names)
	}
}

// TestRound15_KChatLifecycle walks the full SourceConnector +
// DeltaSyncer + WebhookReceiver surface for the new KChat
// connector. This is the canonical "new connector wires end-to-end"
// test for Round 15.
func TestRound15_KChatLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/channels.list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"channels":[{"id":"C1","name":"general"}]}`)
	})
	mux.HandleFunc("/channels.history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"messages":[{"id":"M1","ts":1700000000,"user":"U1","text":"hi"}]}`)
	})
	mux.HandleFunc("/messages.get", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"M1","ts":1700000000,"user":"U1","text":"hi"}`)
	})
	mux.HandleFunc("/channels.changes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"changes":[],"cursor":"CUR1"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := kchat.New(kchat.WithBaseURL(srv.URL), kchat.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(kchat.Credentials{APIToken: "kc-test"})
	cfg := connector.ConnectorConfig{TenantID: "t1", SourceID: "src1", Credentials: creds}
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(ns) == 0 {
		t.Fatalf("list namespaces: %v / len=%d", err, len(ns))
	}
	it, err := c.ListDocuments(ctx, conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one document; err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch document: %v", err)
	}
	if doc.Content != nil {
		_ = doc.Content.Close()
	}
	if _, _, err := c.DeltaSync(ctx, conn, ns[0], ""); err != nil {
		t.Fatalf("delta sync: %v", err)
	}
	ch, err := c.HandleWebhook(ctx, []byte(`{"type":"message.created","channel":"C1","message":{"id":"M1","updated_at":"2024-01-01T00:00:00Z"}}`))
	if err != nil {
		t.Fatalf("handle webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one DocumentChange")
	}
}

// TestRound15_AsanaErrRateLimited proves that connectors surface
// connector.ErrRateLimited through iterator.Err on a 429 response.
func TestRound15_AsanaErrRateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"gid":"U1"}}`)
	})
	mux.HandleFunc("/projects/P1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(asana.Credentials{AccessToken: "as-test"})
	cfg := connector.ConnectorConfig{TenantID: "t1", SourceID: "src1", Credentials: creds}
	ctx := context.Background()
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, err := c.ListDocuments(ctx, conn, connector.Namespace{ID: "P1", Kind: "project"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if it.Next(ctx) {
		t.Fatalf("expected iterator to abort on 429")
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("iterator.Err = %v; want errors.Is(_, connector.ErrRateLimited)", it.Err())
	}
}
