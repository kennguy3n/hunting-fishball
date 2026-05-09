package connector

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ConnectorFactory builds a fresh SourceConnector instance. Callers
// should treat the returned value as throwaway — the registry never
// caches it.
type ConnectorFactory func() SourceConnector

// ErrAlreadyRegistered is returned by RegisterSourceConnector when a
// factory is already registered under the given name.
var ErrAlreadyRegistered = errors.New("connector: already registered")

// ErrNotRegistered is returned by GetSourceConnector when no factory is
// registered under the given name.
var ErrNotRegistered = errors.New("connector: not registered")

// registry is the process-global connector registry. Connectors register
// themselves from `init()`; binaries blank-import the connector packages
// they want to enable.
type registry struct {
	mu        sync.RWMutex
	factories map[string]ConnectorFactory
}

// global is the package-level singleton. Tests use the same instance —
// they restore state by deferring a call to clearForTest (test-only,
// defined in registry_test.go).
var global = &registry{factories: map[string]ConnectorFactory{}}

// RegisterSourceConnector registers a factory under name. Calling this
// twice with the same name returns ErrAlreadyRegistered, which mirrors
// database/sql.Register and lets misconfigured binaries fail loudly at
// boot.
//
// Typical usage from a connector package:
//
//	func init() {
//	    if err := connector.RegisterSourceConnector("google-drive", New); err != nil {
//	        panic(err)
//	    }
//	}
func RegisterSourceConnector(name string, factory ConnectorFactory) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidConfig)
	}
	if factory == nil {
		return fmt.Errorf("%w: nil factory", ErrInvalidConfig)
	}

	global.mu.Lock()
	defer global.mu.Unlock()

	if _, exists := global.factories[name]; exists {
		return fmt.Errorf("%w: %q", ErrAlreadyRegistered, name)
	}
	global.factories[name] = factory

	return nil
}

// GetSourceConnector looks up a factory by name. Returns ErrNotRegistered
// if the name is unknown.
func GetSourceConnector(name string) (ConnectorFactory, error) {
	global.mu.RLock()
	defer global.mu.RUnlock()

	f, ok := global.factories[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotRegistered, name)
	}

	return f, nil
}

// ListSourceConnectors returns a sorted snapshot of registered connector
// names. Useful for /v1/connectors enumeration in the admin portal.
func ListSourceConnectors() []string {
	global.mu.RLock()
	defer global.mu.RUnlock()

	names := make([]string, 0, len(global.factories))
	for name := range global.factories {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
