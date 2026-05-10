// Package config owns the startup-time environment validation shared
// by cmd/api and cmd/ingest (Phase 8 / Task 11).
//
// The two binaries differ in their hard requirements (only the API
// touches Qdrant + Redis at startup; only the ingest binary touches
// Kafka), so each has a dedicated validator. Both reuse a small set
// of generic field validators that produce structured ConfigError
// values listing every issue at once — operators see all missing /
// malformed env vars in a single error message instead of fixing
// them one at a time.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// FieldError captures one invalid env var.
type FieldError struct {
	Name    string
	Value   string
	Message string
}

func (f FieldError) Error() string {
	if f.Value == "" {
		return fmt.Sprintf("%s: %s", f.Name, f.Message)
	}
	return fmt.Sprintf("%s=%q: %s", f.Name, f.Value, f.Message)
}

// ConfigError aggregates one or more FieldError values.
type ConfigError struct {
	Fields []FieldError
}

func (c *ConfigError) Error() string {
	if c == nil || len(c.Fields) == 0 {
		return "config: ok"
	}
	parts := make([]string, 0, len(c.Fields))
	for _, f := range c.Fields {
		parts = append(parts, f.Error())
	}
	return "config: " + strings.Join(parts, "; ")
}

// Add appends a field error.
func (c *ConfigError) Add(name, value, message string) {
	c.Fields = append(c.Fields, FieldError{Name: name, Value: value, Message: message})
}

// HasErrors reports whether any field errors were recorded.
func (c *ConfigError) HasErrors() bool {
	return c != nil && len(c.Fields) > 0
}

// AsError returns nil when HasErrors() is false so callers can chain
// `if err := cfg.AsError(); err != nil { ... }`.
func (c *ConfigError) AsError() error {
	if !c.HasErrors() {
		return nil
	}
	return c
}

// Looker is the env-lookup contract; defaults to os.LookupEnv. Tests
// inject a map-backed lookup.
type Looker func(key string) (string, bool)

// OSLooker reads from process env.
func OSLooker(key string) (string, bool) { return os.LookupEnv(key) }

// MapLooker returns a Looker backed by the supplied map.
func MapLooker(m map[string]string) Looker {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// requireURL validates a required URL-shaped env var.
func requireURL(env *ConfigError, look Looker, name string, schemes ...string) {
	v, ok := look(name)
	if !ok || v == "" {
		env.Add(name, "", "required")
		return
	}
	u, err := url.Parse(v)
	if err != nil {
		env.Add(name, v, "not a valid URL")
		return
	}
	if u.Scheme == "" {
		env.Add(name, v, "missing scheme")
		return
	}
	if len(schemes) > 0 {
		match := false
		for _, s := range schemes {
			if strings.EqualFold(s, u.Scheme) {
				match = true
				break
			}
		}
		if !match {
			env.Add(name, v, fmt.Sprintf("scheme must be one of %s", strings.Join(schemes, ",")))
		}
	}
}

// requireNonEmpty validates a required non-empty env var.
func requireNonEmpty(env *ConfigError, look Looker, name string) {
	v, ok := look(name)
	if !ok || v == "" {
		env.Add(name, "", "required")
	}
}

// requireBrokerList validates a comma-separated host:port list.
func requireBrokerList(env *ConfigError, look Looker, name string) {
	v, ok := look(name)
	if !ok || v == "" {
		env.Add(name, "", "required")
		return
	}
	for _, broker := range strings.Split(v, ",") {
		broker = strings.TrimSpace(broker)
		if broker == "" {
			env.Add(name, v, "empty broker entry")
			return
		}
		host, port, ok := splitHostPort(broker)
		if !ok || host == "" || port == "" {
			env.Add(name, v, fmt.Sprintf("broker %q must be host:port", broker))
			return
		}
		if _, err := strconv.Atoi(port); err != nil {
			env.Add(name, v, fmt.Sprintf("broker %q port not an integer", broker))
			return
		}
	}
}

func splitHostPort(s string) (string, string, bool) {
	idx := strings.LastIndex(s, ":")
	if idx <= 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// ValidateAPI validates the env vars cmd/api/main.go reads at startup.
// Required: CONTEXT_ENGINE_DATABASE_URL, CONTEXT_ENGINE_QDRANT_URL.
// Recommended: CONTEXT_ENGINE_REDIS_URL — empty disables Redis-backed
// features but doesn't fail validation.
func ValidateAPI(look Looker) error {
	if look == nil {
		look = OSLooker
	}
	env := &ConfigError{}
	requireURL(env, look, "CONTEXT_ENGINE_DATABASE_URL", "postgres", "postgresql")
	requireURL(env, look, "CONTEXT_ENGINE_QDRANT_URL", "http", "https")
	if v, ok := look("CONTEXT_ENGINE_REDIS_URL"); ok && v != "" {
		requireURL(env, look, "CONTEXT_ENGINE_REDIS_URL", "redis", "rediss")
	}
	return env.AsError()
}

// ValidateIngest validates the env vars cmd/ingest/main.go reads at
// startup. Required: CONTEXT_ENGINE_DATABASE_URL, CONTEXT_ENGINE_KAFKA_BROKERS.
func ValidateIngest(look Looker) error {
	if look == nil {
		look = OSLooker
	}
	env := &ConfigError{}
	requireURL(env, look, "CONTEXT_ENGINE_DATABASE_URL", "postgres", "postgresql")
	requireBrokerList(env, look, "CONTEXT_ENGINE_KAFKA_BROKERS")
	requireNonEmpty(env, look, "CONTEXT_ENGINE_KAFKA_TOPICS")
	return env.AsError()
}

// IsConfigError reports whether err is a *ConfigError.
func IsConfigError(err error) bool {
	var c *ConfigError
	return errors.As(err, &c)
}
