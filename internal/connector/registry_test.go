package connector

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// clearForTest resets the global registry. It is exposed only via this
// _test.go file so production code cannot depend on it.
func clearForTest() {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.factories = map[string]ConnectorFactory{}
}

func newStubFactory(name string) ConnectorFactory {
	// The stub returns a no-op connector; tests don't exercise its methods.
	return func() SourceConnector { return &registryStub{name: name} }
}

type registryStub struct {
	name string
}

// satisfy SourceConnector with default-error stubs

func (registryStub) Validate(_ context.Context, _ ConnectorConfig) error { return ErrNotSupported }
func (registryStub) Connect(_ context.Context, _ ConnectorConfig) (Connection, error) {
	return nil, ErrNotSupported
}
func (registryStub) ListNamespaces(_ context.Context, _ Connection) ([]Namespace, error) {
	return nil, ErrNotSupported
}
func (registryStub) ListDocuments(_ context.Context, _ Connection, _ Namespace, _ ListOpts) (DocumentIterator, error) {
	return nil, ErrNotSupported
}
func (registryStub) FetchDocument(_ context.Context, _ Connection, _ DocumentRef) (*Document, error) {
	return nil, ErrNotSupported
}
func (registryStub) Subscribe(_ context.Context, _ Connection, _ Namespace) (Subscription, error) {
	return nil, ErrNotSupported
}
func (registryStub) Disconnect(_ context.Context, _ Connection) error { return ErrNotSupported }

func TestRegistry_RegisterAndGet(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	if err := RegisterSourceConnector("alpha", newStubFactory("alpha")); err != nil {
		t.Fatalf("Register alpha: %v", err)
	}

	got, err := GetSourceConnector("alpha")
	if err != nil {
		t.Fatalf("Get alpha: %v", err)
	}
	if got() == nil {
		t.Fatalf("factory returned nil connector")
	}
}

func TestRegistry_RegisterRejectsEmptyName(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	err := RegisterSourceConnector("", newStubFactory("x"))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestRegistry_RegisterRejectsNilFactory(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	err := RegisterSourceConnector("alpha", nil)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestRegistry_DuplicateRegistration(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	if err := RegisterSourceConnector("alpha", newStubFactory("alpha")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := RegisterSourceConnector("alpha", newStubFactory("alpha"))
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("expected ErrAlreadyRegistered, got %v", err)
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	_, err := GetSourceConnector("nope")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("expected ErrNotRegistered, got %v", err)
	}
}

func TestRegistry_ListIsSorted(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	for _, n := range []string{"charlie", "alpha", "bravo"} {
		if err := RegisterSourceConnector(n, newStubFactory(n)); err != nil {
			t.Fatalf("Register %q: %v", n, err)
		}
	}
	got := ListSourceConnectors()
	want := []string{"alpha", "bravo", "charlie"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestRegistry_ConcurrentAccess hammers the registry from multiple
// goroutines to expose any data race; run with `go test -race`.
func TestRegistry_ConcurrentAccess(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	const goroutines = 32
	const opsPerGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				name := stubName(i, j)
				_ = RegisterSourceConnector(name, newStubFactory(name))
				_, _ = GetSourceConnector(name)
				_ = ListSourceConnectors()
			}
		}()
	}
	wg.Wait()
}

// TestBlankImportPattern documents how connector packages register
// themselves from init() — the registry mirrors database/sql.Register.
func TestBlankImportPattern(t *testing.T) {
	clearForTest()
	t.Cleanup(clearForTest)

	// Simulate a connector package's init().
	register := func() {
		if err := RegisterSourceConnector("blank-import", newStubFactory("blank-import")); err != nil {
			t.Fatalf("init register: %v", err)
		}
	}
	register()

	got, err := GetSourceConnector("blank-import")
	if err != nil {
		t.Fatalf("Get after blank import: %v", err)
	}
	if got().(*registryStub).name != "blank-import" {
		t.Fatalf("unexpected stub")
	}

	if names := ListSourceConnectors(); len(names) != 1 || names[0] != "blank-import" {
		t.Fatalf("unexpected names: %v", names)
	}
}

func stubName(i, j int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	a := letters[i%len(letters)]
	b := letters[(j/len(letters))%len(letters)]
	c := letters[j%len(letters)]

	return string([]byte{a, b, c})
}
