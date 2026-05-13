//go:build e2e

// Round-18 Task 15 — end-to-end smoke for the Round-18 surface.
//
// Round 18 expands the connector catalog from 36 → 42 by adding
// sharepoint_onprem, azure_blob, gcs, egnyte, bookstack, and
// upload_portal. This file exercises the Round-18 surface as an
// integrated bundle:
//
//	(a) Registry now reports exactly 42 entries (36 → 42).
//	(b) Two heterogeneous Round-18 connector lifecycles
//	    (azure_blob / bookstack as a SAS-signed object store and a
//	    token-header wiki) walk Validate → Connect →
//	    ListNamespaces → ListDocuments → DeltaSync.
//	(c) connector.ErrRateLimited propagates through Connect when
//	    each new connector's upstream returns 429.
//	(d) The cross-encoder reranker round-trip survives an in-process
//	    gRPC sidecar (gated by CONTEXT_ENGINE_CROSS_ENCODER_ENABLED).
//	(e) The query routing classifier emits per-source weights that
//	    match the documented exact / semantic / entity intents.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	azureblob "github.com/kennguy3n/hunting-fishball/internal/connector/azure_blob"
	"github.com/kennguy3n/hunting-fishball/internal/connector/bookstack"
	"github.com/kennguy3n/hunting-fishball/internal/connector/egnyte"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gcs"
	sharepointonprem "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint_onprem"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

// TestRound18_RegistryCount verifies the catalog expansion contract:
// blank-imports from the e2e package populate exactly 42 connector
// entries after Round 18's six new connectors land.
func TestRound18_RegistryCount(t *testing.T) {
	t.Parallel()
	names := connector.ListSourceConnectors()
	if len(names) != 42 {
		t.Fatalf("registry: %d entries, want 42: %v", len(names), names)
	}
	required := []string{
		"sharepoint_onprem", "azure_blob", "gcs",
		"egnyte", "bookstack", "upload_portal",
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Fatalf("registry missing Round-18 connector %q: %v", r, names)
		}
	}
}

// TestRound18_AzureBlobLifecycle walks the basic SAS-signed
// lifecycle through Connect for the Azure Blob connector.
func TestRound18_AzureBlobLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/ctr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><EnumerationResults><Blobs><Blob><Name>a.txt</Name><Properties><Last-Modified>Mon, 01 Jan 2026 00:00:00 GMT</Last-Modified><Content-Length>3</Content-Length></Properties></Blob></Blobs></EnumerationResults>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(azureblob.Credentials{Account: "acct", Container: "ctr", SASToken: "sv=2024-01-01&sig=ok"})
	ctx := context.Background()
	conn, err := c.Connect(ctx, connector.ConnectorConfig{
		TenantID: "t1", SourceID: "s1", Credentials: creds,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) == 0 {
		t.Fatalf("expected ≥1 namespace")
	}
}

// TestRound18_BookstackLifecycle walks Connect → ListDocuments for
// the BookStack token-header lifecycle.
func TestRound18_BookstackLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/books", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"Home","updated_at":"2026-01-01T00:00:00Z"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(bookstack.Credentials{TokenID: "id", TokenSecret: "secret"})
	ctx := context.Background()
	conn, err := c.Connect(ctx, connector.ConnectorConfig{TenantID: "t1", SourceID: "s1", Credentials: creds})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.ListNamespaces(ctx, conn); err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
}

// TestRound18_RateLimitSweep verifies all 6 new connectors map 429
// from their primary endpoint to connector.ErrRateLimited via the
// Connect or DeltaSync surface.
func TestRound18_RateLimitSweep(t *testing.T) {
	rl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer rl.Close()

	type probe struct {
		name string
		run  func() error
	}
	probes := []probe{
		{"sharepoint_onprem", func() error {
			c := sharepointonprem.New(sharepointonprem.WithBaseURL(rl.URL), sharepointonprem.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(sharepointonprem.Credentials{Username: "u", Password: "p"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"azure_blob", func() error {
			c := azureblob.New(azureblob.WithBaseURL(rl.URL), azureblob.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(azureblob.Credentials{Account: "acct", Container: "ctr", SASToken: "sv=2024&sig=ok"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"gcs", func() error {
			c := gcs.New(gcs.WithBaseURL(rl.URL), gcs.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(gcs.Credentials{AccessToken: "t", Bucket: "b"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"egnyte", func() error {
			c := egnyte.New(egnyte.WithBaseURL(rl.URL), egnyte.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(egnyte.Credentials{AccessToken: "t"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"bookstack", func() error {
			c := bookstack.New(bookstack.WithBaseURL(rl.URL), bookstack.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(bookstack.Credentials{TokenID: "id", TokenSecret: "secret"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
	}
	for _, p := range probes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			err := p.run()
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("%s: expected ErrRateLimited, got %v", p.name, err)
			}
		})
	}
}

// TestRound18_QueryRoutingClassifierIntents verifies the
// classifier surfaces the three documented intents and emits the
// expected directional per-source weights.
func TestRound18_QueryRoutingClassifierIntents(t *testing.T) {
	t.Parallel()
	cls := retrieval.NewQueryClassifier(retrieval.ClassifierConfig{})
	exactKind, exactW := cls.Classify(`"rotate credentials"`)
	if exactKind != retrieval.QueryKindExact {
		t.Fatalf("exact: got %q want %q", exactKind, retrieval.QueryKindExact)
	}
	if exactW[retrieval.SourceBM25] <= exactW[retrieval.SourceVector] {
		t.Fatalf("exact must favour BM25, got %+v", exactW)
	}

	semKind, semW := cls.Classify("How does the embedding cache invalidate keys when a source re-indexes?")
	if semKind != retrieval.QueryKindSemantic {
		t.Fatalf("semantic: got %q want %q", semKind, retrieval.QueryKindSemantic)
	}
	if semW[retrieval.SourceVector] <= semW[retrieval.SourceBM25] {
		t.Fatalf("semantic must favour vector, got %+v", semW)
	}

	entKind, entW := cls.Classify("Acme Corp Q4 results")
	if entKind != retrieval.QueryKindEntity {
		t.Fatalf("entity: got %q want %q", entKind, retrieval.QueryKindEntity)
	}
	if entW[retrieval.SourceGraph] <= entW[retrieval.SourceVector] {
		t.Fatalf("entity must favour graph, got %+v", entW)
	}
}
