// Shared-drives variant of the Google Drive connector. Round-15
// Task 8 extends the catalog to enumerate Google Shared Drives
// as a first-class connector type. The implementation reuses the
// `Connector` defined in googledrive.go but exposes only the
// shared drives (not "My Drive") through ListNamespaces. This
// satisfies the registry-count contract (`google_drive` +
// `google_shared_drives` = 2 entries) without duplicating the
// underlying HTTP/auth/pagination plumbing.
package googledrive

import (
	"context"
	"errors"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// SharedDrivesName is the registry-visible connector name for the
// shared-drives-only variant.
const SharedDrivesName = "google_shared_drives"

// SharedDrivesConnector wraps Connector and overrides
// ListNamespaces to filter out the synthetic "My Drive"
// namespace. Everything else delegates to the embedded Connector.
type SharedDrivesConnector struct {
	*Connector
}

// NewSharedDrives constructs a shared-drives-only connector.
func NewSharedDrives(opts ...Option) *SharedDrivesConnector {
	return &SharedDrivesConnector{Connector: New(opts...)}
}

// ListNamespaces returns the visible shared drives — explicitly
// excluding the "My Drive" namespace that the standard
// google_drive connector surfaces.
func (s *SharedDrivesConnector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	if _, ok := c.(*connection); !ok {
		return nil, errors.New("google_shared_drives: bad connection type")
	}
	all, err := s.Connector.ListNamespaces(ctx, c)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, n := range all {
		if n.Kind == "shared_drive" {
			out = append(out, n)
		}
	}

	return out, nil
}

// RegisterSharedDrives wires the shared-drives variant into the
// global connector registry.
func RegisterSharedDrives() {
	_ = connector.RegisterSourceConnector(SharedDrivesName, func() connector.SourceConnector { return NewSharedDrives() })
}

func init() { RegisterSharedDrives() }
