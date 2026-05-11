package admin

// ab_test.go — Round-6 Task 10.
//
// Retrieval A/B testing config + persistence + router live here so
// the admin package (already imported by retrieval via
// explain.go) can own them without creating an import cycle. The
// retrieval handler accepts an ABTestRouter through a narrow
// interface declared in `internal/retrieval/types_round6.go`.

import (
	"errors"
	"hash/fnv"
	"sync"
	"time"
)

// ABTestStatus enumerates the lifecycle states of an experiment.
type ABTestStatus string

const (
	// ABTestStatusDraft is the initial state — saved but not
	// routing any traffic.
	ABTestStatusDraft ABTestStatus = "draft"
	// ABTestStatusActive routes requests according to the traffic
	// split.
	ABTestStatusActive ABTestStatus = "active"
	// ABTestStatusEnded stops routing; configs preserved for
	// audit.
	ABTestStatusEnded ABTestStatus = "ended"
)

// ABTestArm tags which arm a request was routed to.
type ABTestArm string

const (
	// ABTestArmControl is the baseline configuration.
	ABTestArmControl ABTestArm = "control"
	// ABTestArmVariant is the experimental configuration.
	ABTestArmVariant ABTestArm = "variant"
)

// ABTestConfig is the persisted shape of an experiment.
type ABTestConfig struct {
	TenantID            string         `json:"tenant_id"`
	ExperimentName      string         `json:"experiment_name"`
	Status              ABTestStatus   `json:"status"`
	TrafficSplitPercent int            `json:"traffic_split_percent"`
	ControlConfig       map[string]any `json:"control_config"`
	VariantConfig       map[string]any `json:"variant_config"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	StartAt             *time.Time     `json:"start_at,omitempty"`
	EndAt               *time.Time     `json:"end_at,omitempty"`
}

// Validate returns nil when cfg is well-formed.
func (c *ABTestConfig) Validate() error {
	if c == nil {
		return errors.New("ab test: nil config")
	}
	if c.TenantID == "" {
		return errors.New("ab test: missing tenant_id")
	}
	if c.ExperimentName == "" {
		return errors.New("ab test: missing experiment_name")
	}
	if c.TrafficSplitPercent < 0 || c.TrafficSplitPercent > 100 {
		return errors.New("ab test: traffic_split_percent must be 0..100")
	}
	switch c.Status {
	case ABTestStatusDraft, ABTestStatusActive, ABTestStatusEnded:
	default:
		return errors.New("ab test: invalid status")
	}
	return nil
}

// ABTestStore is the persistence port. The production
// implementation is a Postgres GORM repo; tests use the in-memory
// implementation below.
type ABTestStore interface {
	List(tenantID string) ([]*ABTestConfig, error)
	Get(tenantID, name string) (*ABTestConfig, error)
	Upsert(*ABTestConfig) error
	Delete(tenantID, name string) error
}

// InMemoryABTestStore is a goroutine-safe map-backed store useful
// for tests and local dev.
type InMemoryABTestStore struct {
	mu   sync.RWMutex
	rows map[string]*ABTestConfig
}

// NewInMemoryABTestStore builds an InMemoryABTestStore.
func NewInMemoryABTestStore() *InMemoryABTestStore {
	return &InMemoryABTestStore{rows: map[string]*ABTestConfig{}}
}

func (s *InMemoryABTestStore) key(tenantID, name string) string {
	return tenantID + "\x00" + name
}

// List implements ABTestStore.
func (s *InMemoryABTestStore) List(tenantID string) ([]*ABTestConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*ABTestConfig{}
	prefix := tenantID + "\x00"
	for k, v := range s.rows {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, v)
		}
	}
	return out, nil
}

// Get implements ABTestStore.
func (s *InMemoryABTestStore) Get(tenantID, name string) (*ABTestConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.rows[s.key(tenantID, name)]
	if !ok {
		return nil, errors.New("ab test: not found")
	}
	cp := *v
	return &cp, nil
}

// Upsert implements ABTestStore.
func (s *InMemoryABTestStore) Upsert(cfg *ABTestConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *cfg
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	cp.UpdatedAt = time.Now().UTC()
	s.rows[s.key(cfg.TenantID, cfg.ExperimentName)] = &cp
	return nil
}

// Delete implements ABTestStore.
func (s *InMemoryABTestStore) Delete(tenantID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, s.key(tenantID, name))
	return nil
}

// ABTestRouteOutcome describes the result of a routing decision.
type ABTestRouteOutcome struct {
	ExperimentName string
	Arm            ABTestArm
	Config         map[string]any
}

// ABTestRouter routes requests to control/variant arms by
// deterministic hash bucketing.
type ABTestRouter struct {
	store ABTestStore
}

// NewABTestRouter constructs an ABTestRouter.
func NewABTestRouter(store ABTestStore) *ABTestRouter {
	return &ABTestRouter{store: store}
}

// Route returns the experiment + arm for (tenantID, experimentName,
// bucketKey). If the experiment is missing or not active, returns
// (nil, nil) and routing falls back to the default config.
func (r *ABTestRouter) Route(tenantID, experimentName, bucketKey string) (*ABTestRouteOutcome, error) {
	if r == nil || experimentName == "" || tenantID == "" {
		return nil, nil
	}
	cfg, err := r.store.Get(tenantID, experimentName)
	if err != nil || cfg == nil {
		return nil, nil
	}
	if cfg.Status != ABTestStatusActive {
		return nil, nil
	}
	bucket := bucketHash(bucketKey)
	threshold := uint32(cfg.TrafficSplitPercent)
	arm := ABTestArmControl
	chosenCfg := cfg.ControlConfig
	if bucket < threshold {
		arm = ABTestArmVariant
		chosenCfg = cfg.VariantConfig
	}
	return &ABTestRouteOutcome{ExperimentName: experimentName, Arm: arm, Config: chosenCfg}, nil
}

// bucketHash returns 0..99 deterministically from key.
func bucketHash(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return h.Sum32() % 100
}
