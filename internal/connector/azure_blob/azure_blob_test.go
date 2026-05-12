package azureblob_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	azureblob "github.com/kennguy3n/hunting-fishball/internal/connector/azure_blob"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(azureblob.Credentials{Account: "acme", Container: "ctr", SharedKey: base64.StdEncoding.EncodeToString([]byte("k"))})

	return b
}

func TestAzureBlob_Validate(t *testing.T) {
	t.Parallel()
	c := azureblob.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"no account", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"container":"c","sas_token":"?s"}`)}, false},
		{"no auth", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"account":"a","container":"c"}`)}, false},
		{"bad key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"account":"a","container":"c","shared_key":"!!"}`)}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.Validate(context.Background(), tc.cfg)
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

func TestAzureBlob_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestAzureBlob_Lifecycle(t *testing.T) {
	t.Parallel()
	listXML := `<?xml version="1.0" encoding="utf-8"?>
<EnumerationResults>
  <Blobs>
    <Blob>
      <Name>readme.txt</Name>
      <Properties>
        <Last-Modified>Mon, 01 Jul 2024 12:00:00 GMT</Last-Modified>
        <Etag>"abc"</Etag>
        <Content-Type>text/plain</Content-Type>
        <Content-Length>5</Content-Length>
      </Properties>
    </Blob>
  </Blobs>
</EnumerationResults>`
	mux := http.NewServeMux()
	mux.HandleFunc("/ctr", func(w http.ResponseWriter, r *http.Request) {
		// Verify that we signed the request with a SharedKey header.
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "missing auth", http.StatusUnauthorized)

			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SharedKey ") {
			http.Error(w, "wrong auth", http.StatusUnauthorized)

			return
		}
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Query().Get("comp") != "list" {
				http.Error(w, "want comp=list", http.StatusBadRequest)

				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, listXML)
		}
	})
	mux.HandleFunc("/ctr/readme.txt", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "missing auth", http.StatusUnauthorized)

			return
		}
		w.Header().Set("Last-Modified", "Mon, 01 Jul 2024 12:00:00 GMT")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hello")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 {
		t.Fatalf("ns=%v err=%v", ns, err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var names []string
	for it.Next(context.Background()) {
		names = append(names, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iter err=%v", it.Err())
	}
	if len(names) != 1 || names[0] != "readme.txt" {
		t.Fatalf("names=%v", names)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "ctr", ID: "readme.txt"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hello" {
		t.Fatalf("body=%q", body)
	}
}

func TestAzureBlob_SASToken_NoAuthHeader(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/ctr", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "should not sign", http.StatusBadRequest)

			return
		}
		if !strings.Contains(r.URL.RawQuery, "sig=abc") {
			http.Error(w, "missing sas", http.StatusBadRequest)

			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(azureblob.Credentials{Account: "acme", Container: "ctr", SASToken: "?sv=2022-11-02&sig=abc"})
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestAzureBlob_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/ctr", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	fixed := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)
	c := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()), azureblob.WithNow(func() time.Time { return fixed }))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ctr"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur != "2024-07-01T12:00:00Z" {
		t.Fatalf("cursor=%q", cur)
	}
}

func TestAzureBlob_DeltaSync_StopsAtCursor(t *testing.T) {
	t.Parallel()
	listXML := `<?xml version="1.0" encoding="utf-8"?>
<EnumerationResults>
  <Blobs>
    <Blob>
      <Name>old.txt</Name>
      <Properties><Last-Modified>Mon, 01 Jul 2024 11:00:00 GMT</Last-Modified><Etag>"a"</Etag></Properties>
    </Blob>
    <Blob>
      <Name>new.txt</Name>
      <Properties><Last-Modified>Mon, 01 Jul 2024 13:00:00 GMT</Last-Modified><Etag>"b"</Etag></Properties>
    </Blob>
  </Blobs>
</EnumerationResults>`
	mux := http.NewServeMux()
	mux.HandleFunc("/ctr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		if r.Header.Get("If-Modified-Since") == "" {
			http.Error(w, "missing IMS", http.StatusBadRequest)

			return
		}
		_, _ = io.WriteString(w, listXML)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ctr"}, "2024-07-01T12:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "new.txt" {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestAzureBlob_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/ctr", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	creds, _ := json.Marshal(azureblob.Credentials{Account: "acme", Container: "ctr", SASToken: "?sv=2022-11-02&sig=abc"})
	c2 := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()))
	_, cerr := c2.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
	if !errors.Is(cerr, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited on Connect, got %v", cerr)
	}
}

func TestAzureBlob_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := azureblob.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
