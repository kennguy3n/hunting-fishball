package googledrive_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
)

func TestGoogleSharedDrives_ListNamespaces_FiltersMyDrive(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/drives", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"drives":[{"id":"D1","name":"Engineering"},{"id":"D2","name":"Ops"}]}`))
	})
	srv := newDriveServer(t, mux)
	s := googledrive.NewSharedDrives(googledrive.WithBaseURL(srv.URL), googledrive.WithHTTPClient(srv.Client()))
	conn, err := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := s.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	// Only shared drives — no my-drive synthetic namespace.
	if len(ns) != 2 {
		t.Fatalf("expected 2 shared-drives, got %d: %+v", len(ns), ns)
	}
	for _, n := range ns {
		if n.Kind != "shared_drive" {
			t.Fatalf("unexpected kind %q", n.Kind)
		}
	}
}

func TestGoogleSharedDrives_Registered(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(googledrive.SharedDrivesName); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
