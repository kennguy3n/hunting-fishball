package pipedrive_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/pipedrive"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(pipedrive.Credentials{APIToken: "tok", CompanyDomain: "acme"})

	return b
}

func TestPipedrive_Validate(t *testing.T) {
	t.Parallel()
	p := pipedrive.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"company_domain":"x"}`)}, false},
		{"missing domain", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"api_token":"k"}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := p.Validate(context.Background(), tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("expected error")
				}
				if !errors.Is(err, connector.ErrInvalidConfig) {
					t.Fatalf("expected ErrInvalidConfig, got %v", err)
				}
			}
		})
	}
}

func TestPipedrive_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	_, err := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestPipedrive_ListNamespaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	conn, _ := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := p.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 3 {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestPipedrive_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	pages := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/deals", func(w http.ResponseWriter, _ *http.Request) {
		pages++
		if pages == 1 {
			_, _ = io.WriteString(w, `{"data":[{"id":1,"update_time":"2024-01-01 00:00:00"}],"additional_data":{"pagination":{"more_items_in_collection":true,"next_start":100}}}`)

			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":2,"update_time":"2024-01-02 00:00:00"}],"additional_data":{"pagination":{"more_items_in_collection":false}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	conn, _ := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := p.ListDocuments(context.Background(), conn, connector.Namespace{ID: "deals"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 {
		t.Fatalf("ids=%v", ids)
	}
}

func TestPipedrive_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/deals", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	conn, _ := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := p.ListDocuments(context.Background(), conn, connector.Namespace{ID: "deals"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestPipedrive_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/deals/1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"id":1,"title":"Big deal","add_time":"2024-01-01 00:00:00","update_time":"2024-01-02 00:00:00","owner_name":"alice"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	conn, _ := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := p.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "deals", ID: "1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Big deal" || doc.Author != "alice" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Big deal") {
		t.Fatalf("body=%q", body)
	}
}

func TestPipedrive_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var gotSince string
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/deals", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sort") == "update_time DESC" {
			_, _ = io.WriteString(w, `{"data":[{"update_time":"2024-01-05 00:00:00"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	mux.HandleFunc("/recents", func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.URL.Query().Get("since_timestamp")
		_, _ = io.WriteString(w, `{"data":[{"item":"deal","data":{"id":2,"update_time":"2024-01-10 00:00:00"}}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	conn, _ := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := p.DeltaSync(context.Background(), conn, connector.Namespace{ID: "deals"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-01-05 00:00:00" {
		t.Fatalf("initial cur=%q changes=%v", cur, changes)
	}
	changes, newCur, err := p.DeltaSync(context.Background(), conn, connector.Namespace{ID: "deals"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if gotSince != cur {
		t.Fatalf("since_timestamp=%q want %q", gotSince, cur)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "2" || newCur != "2024-01-10 00:00:00" {
		t.Fatalf("changes=%v newCur=%q", changes, newCur)
	}
}

func TestPipedrive_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/deals", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	mux.HandleFunc("/recents", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	conn, _ := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := p.DeltaSync(context.Background(), conn, connector.Namespace{ID: "deals"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	_, _, err = p.DeltaSync(context.Background(), conn, connector.Namespace{ID: "deals"}, "2024-01-01 00:00:00")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
}

// TestPipedrive_NetworkErrorDoesNotLeakAPIToken regresses against a Round-16
// review finding: the do() helper used to embed the api_token in the URL
// passed to fmt.Errorf, which leaked the credential to structured logs on
// every transient network failure.
func TestPipedrive_NetworkErrorDoesNotLeakAPIToken(t *testing.T) {
	t.Parallel()
	const secretToken = "super-secret-leakable-token-xyz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Hijack the connection and close it abruptly so http.Client.Do
		// returns a transport-level error instead of an HTTP status.
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = c.Close()
	}))
	defer srv.Close()
	creds, _ := json.Marshal(pipedrive.Credentials{APIToken: secretToken, CompanyDomain: "acme"})
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	_, err := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("api_token leaked into error string: %v", err)
	}
}

func TestPipedrive_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	p := pipedrive.New()
	_, err := p.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

// TestPipedrive_DeltaSync_ActivitiesNamespaceEmitsChanges regresses against a
// Round-16 review finding: DeltaSync used strings.TrimSuffix(ns.ID, "s") to
// derive the per-record "item" key for filtering /recents responses. That
// produces "activitie" for the "activities" namespace, while Pipedrive
// returns item="activity", so every activity change was silently dropped.
// This test runs the steady-state DeltaSync path against an "activities"
// namespace and asserts the change is emitted.
func TestPipedrive_DeltaSync_ActivitiesNamespaceEmitsChanges(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/recents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"item":"activity","data":{"id":42,"update_time":"2024-02-01 12:00:00"}}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	conn, _ := p.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, newCur, err := p.DeltaSync(context.Background(), conn, connector.Namespace{ID: "activities"}, "2024-01-01 00:00:00")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "42" {
		t.Fatalf("expected 1 activity change with id=42, got %+v", changes)
	}
	if changes[0].Kind != connector.ChangeUpserted {
		t.Fatalf("expected ChangeUpserted, got %v", changes[0].Kind)
	}
	if newCur != "2024-02-01 12:00:00" {
		t.Fatalf("newCur=%q want %q", newCur, "2024-02-01 12:00:00")
	}
}
