//go:build e2e

// Round-24 Task 7 — end-to-end smoke for the Round-24 surface.
//
// Round 24 expands the catalog from 50 → 54 by adding quip,
// freshservice, pagerduty, and zoho_desk. This file mirrors the
// Round-20 e2e bundle:
//
//	(a) Registry now reports exactly 54 entries.
//	(b) A representative lifecycle (Quip) walks
//	    Validate → Connect → ListNamespaces → ListDocuments →
//	    FetchDocument → DeltaSync against a mock.
//	(c) connector.ErrRateLimited propagates through Connect
//	    when each new connector's upstream returns 429.
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
	"github.com/kennguy3n/hunting-fishball/internal/connector/freshservice"
	"github.com/kennguy3n/hunting-fishball/internal/connector/pagerduty"
	"github.com/kennguy3n/hunting-fishball/internal/connector/quip"
	zohodesk "github.com/kennguy3n/hunting-fishball/internal/connector/zoho_desk"
)

// TestRound24_RegistryCount verifies the catalog expansion
// contract: blank-imports from the e2e package populate exactly
// 54 connector entries after Round 24's four new connectors
// land.
func TestRound24_RegistryCount(t *testing.T) {
	t.Parallel()
	names := connector.ListSourceConnectors()
	if len(names) != 54 {
		t.Fatalf("registry: %d entries, want 54: %v", len(names), names)
	}
	required := []string{"quip", "freshservice", "pagerduty", "zoho_desk"}
	for _, name := range required {
		found := false
		for _, n := range names {
			if n == name {
				found = true

				break
			}
		}
		if !found {
			t.Fatalf("registry missing %q", name)
		}
	}
}

// TestRound24_QuipLifecycle walks the Quip connector end-to-end
// against an httptest mock and asserts the iterator, fetch, and
// bootstrap DeltaSync paths all succeed.
func TestRound24_QuipLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/1/users/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"u1"}`)
	})
	mux.HandleFunc("/1/threads/recent", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"abc":{"thread":{"id":"abc","title":"Spec","author_id":"u1","updated_usec":1717200000000000,"created_usec":1717100000000000},"html":"<p>hi</p>"}}`)
	})
	mux.HandleFunc("/1/threads/abc", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"thread":{"id":"abc","title":"Spec","author_id":"u1","updated_usec":1717200000000000,"created_usec":1717100000000000},"html":"<p>hi</p>"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := quip.New(quip.WithBaseURL(srv.URL), quip.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(quip.Credentials{AccessToken: "tok"})
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 {
		t.Fatalf("ListNamespaces ns=%v err=%v", ns, err)
	}
	it, err := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iterator err=%v", it.Err())
	}
	if len(ids) != 1 || ids[0] != "abc" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "threads", ID: "abc"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	_ = doc.Content.Close()
	_, cur, err := c.DeltaSync(context.Background(), conn, ns[0], "")
	if err != nil || cur == "" {
		t.Fatalf("DeltaSync bootstrap cur=%q err=%v", cur, err)
	}
}

// TestRound24_RateLimitSweep sweeps each Round-24 connector's
// Connect path through a 429 to assert the wrapping into
// connector.ErrRateLimited holds.
func TestRound24_RateLimitSweep(t *testing.T) {
	t.Parallel()
	rateLimited := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}
	cases := []struct {
		name string
		do   func(t *testing.T, baseURL string, client *http.Client)
	}{
		{"quip", func(t *testing.T, baseURL string, client *http.Client) {
			c := quip.New(quip.WithBaseURL(baseURL), quip.WithHTTPClient(client))
			creds, _ := json.Marshal(quip.Credentials{AccessToken: "tok"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("quip: expected ErrRateLimited, got %v", err)
			}
		}},
		{"freshservice", func(t *testing.T, baseURL string, client *http.Client) {
			c := freshservice.New(freshservice.WithBaseURL(baseURL), freshservice.WithHTTPClient(client))
			creds, _ := json.Marshal(freshservice.Credentials{Domain: "acme", APIKey: "k"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("freshservice: expected ErrRateLimited, got %v", err)
			}
		}},
		{"pagerduty", func(t *testing.T, baseURL string, client *http.Client) {
			c := pagerduty.New(pagerduty.WithBaseURL(baseURL), pagerduty.WithHTTPClient(client))
			creds, _ := json.Marshal(pagerduty.Credentials{APIKey: "k"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("pagerduty: expected ErrRateLimited, got %v", err)
			}
		}},
		{"zoho_desk", func(t *testing.T, baseURL string, client *http.Client) {
			c := zohodesk.New(zohodesk.WithBaseURL(baseURL), zohodesk.WithHTTPClient(client))
			creds, _ := json.Marshal(zohodesk.Credentials{AccessToken: "tok", OrgID: "1"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("zoho_desk: expected ErrRateLimited, got %v", err)
			}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(rateLimited))
			defer srv.Close()
			tc.do(t, srv.URL, srv.Client())
		})
	}
}
