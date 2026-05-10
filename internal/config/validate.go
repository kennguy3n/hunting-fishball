// Package config holds startup-time validation of CONTEXT_ENGINE_*
// environment variables. cmd/api and cmd/ingest call ValidateAPI /
// ValidateIngest before any other initialisation so an obvious
// misconfiguration (missing DATABASE_URL, unparseable URL,
// out-of-range numeric) fails the process loudly with a single
// batch of issues rather than producing nine pages of cryptic dial
// errors deeper in the stack.
//
// The validator is intentionally pure-stdlib: callers pass a
// `func(string) string` "looker" so unit tests can drive it
// without setenv juggling. The production wiring uses os.Getenv
// directly via OSLooker().
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Looker is the env-vars getter signature used by the validators.
// It mirrors os.Getenv so production code can pass os.Getenv
// directly. Tests use MapLooker.
type Looker func(string) string

// OSLooker is a Looker backed by os.Getenv. Production wiring
// passes this directly to ValidateAPI / ValidateIngest.
var OSLooker Looker = func(k string) string { return getenv(k) }

// MapLooker is the test helper: returns a Looker that reads from
// the supplied map (returning "" for missing keys).
func MapLooker(m map[string]string) Looker {
	return func(k string) string { return m[k] }
}

// Issue is one validation problem.
type Issue struct {
	Name    string
	Value   string
	Message string
}

// ConfigError aggregates every Issue surfaced by ValidateAPI /
// ValidateIngest. Empty == "no issues" — call AsError() to get a
// nil error in that case.
type ConfigError struct {
	Issues []Issue
}

// Add appends a new Issue.
func (c *ConfigError) Add(name, value, msg string) {
	c.Issues = append(c.Issues, Issue{Name: name, Value: value, Message: msg})
}

// Error implements error. Format is `config: N issue(s); ...`
// joined by '; '. The N prefix lets log scanners spot config
// failures unambiguously.
func (c *ConfigError) Error() string {
	if len(c.Issues) == 0 {
		return "config: no issues"
	}
	parts := make([]string, 0, len(c.Issues))
	for _, iss := range c.Issues {
		if iss.Value == "" {
			parts = append(parts, fmt.Sprintf("%s: %s", iss.Name, iss.Message))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%q: %s", iss.Name, iss.Value, iss.Message))
		}
	}
	return fmt.Sprintf("config: %d issue(s); %s", len(c.Issues), strings.Join(parts, "; "))
}

// AsError returns nil when there are no issues, otherwise c.
func (c *ConfigError) AsError() error {
	if c == nil || len(c.Issues) == 0 {
		return nil
	}
	return c
}

// IsConfigError reports whether err is (or wraps) a *ConfigError.
func IsConfigError(err error) bool {
	if err == nil {
		return false
	}
	var ce *ConfigError
	return errors.As(err, &ce)
}

// ValidateAPI runs the cmd/api boot rules.
func ValidateAPI(get Looker) error {
	c := &ConfigError{}

	requireURL(c, get, "CONTEXT_ENGINE_DATABASE_URL", []string{"postgres", "postgresql"})
	requireURL(c, get, "CONTEXT_ENGINE_QDRANT_URL", []string{"http", "https"})

	optionalURL(c, get, "CONTEXT_ENGINE_REDIS_URL", []string{"redis", "rediss"})

	optionalHostPort(c, get, "CONTEXT_ENGINE_EMBEDDING_TARGET")
	optionalHostPort(c, get, "CONTEXT_ENGINE_MEMORY_TARGET")
	optionalHostPort(c, get, "CONTEXT_ENGINE_LISTEN_ADDR")

	optionalIntInRange(c, get, "CONTEXT_ENGINE_VECTOR_SIZE", 1, 16384)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_PG_MAX_OPEN", 1, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_PG_MAX_IDLE", 0, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_QDRANT_POOL_SIZE", 1, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_REDIS_POOL_SIZE", 1, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_API_RATE_LIMIT", 0, 100000)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_API_RATE_BURST", 0, 100000)

	return c.AsError()
}

// ValidateIngest runs the cmd/ingest boot rules.
func ValidateIngest(get Looker) error {
	c := &ConfigError{}

	requireURL(c, get, "CONTEXT_ENGINE_DATABASE_URL", []string{"postgres", "postgresql"})
	requireHostPortList(c, get, "CONTEXT_ENGINE_KAFKA_BROKERS")
	requireNonEmpty(c, get, "CONTEXT_ENGINE_KAFKA_TOPICS")

	optionalURL(c, get, "CONTEXT_ENGINE_QDRANT_URL", []string{"http", "https"})
	optionalHostPort(c, get, "CONTEXT_ENGINE_PARSE_TARGET")
	optionalHostPort(c, get, "CONTEXT_ENGINE_EMBEDDING_TARGET")

	optionalIntInRange(c, get, "CONTEXT_ENGINE_VECTOR_SIZE", 1, 16384)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_FETCH_WORKERS", 1, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_PARSE_WORKERS", 1, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_EMBED_WORKERS", 1, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_STORE_WORKERS", 1, 1024)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_SHUTDOWN_TIMEOUT_SECONDS", 1, 600)
	optionalIntInRange(c, get, "CONTEXT_ENGINE_DLQ_MAX_ATTEMPTS", 1, 100)

	return c.AsError()
}

// ----- helpers ---------------------------------------------------

func requireNonEmpty(c *ConfigError, get Looker, name string) {
	if strings.TrimSpace(get(name)) == "" {
		c.Add(name, "", "required but unset")
	}
}

func requireURL(c *ConfigError, get Looker, name string, schemes []string) {
	v := strings.TrimSpace(get(name))
	if v == "" {
		c.Add(name, "", "required but unset")
		return
	}
	if err := checkURL(v, schemes); err != nil {
		c.Add(name, v, err.Error())
	}
}

func optionalURL(c *ConfigError, get Looker, name string, schemes []string) {
	v := strings.TrimSpace(get(name))
	if v == "" {
		return
	}
	if err := checkURL(v, schemes); err != nil {
		c.Add(name, v, err.Error())
	}
}

func checkURL(v string, schemes []string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("not a URL: %v", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return errors.New("URL must include scheme and host")
	}
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			return nil
		}
	}
	return fmt.Errorf("scheme must be one of %v, got %q", schemes, u.Scheme)
}

func requireHostPortList(c *ConfigError, get Looker, name string) {
	v := strings.TrimSpace(get(name))
	if v == "" {
		c.Add(name, "", "required but unset")
		return
	}
	if err := checkHostPortList(v); err != nil {
		c.Add(name, v, err.Error())
	}
}

func optionalHostPort(c *ConfigError, get Looker, name string) {
	v := strings.TrimSpace(get(name))
	if v == "" {
		return
	}
	if err := checkHostPortList(v); err != nil {
		c.Add(name, v, err.Error())
	}
}

func checkHostPortList(v string) error {
	for _, hp := range strings.Split(v, ",") {
		hp = strings.TrimSpace(hp)
		if hp == "" {
			return errors.New("empty entry in list")
		}
		// Allow leading colon for ":8080" listen addresses; otherwise
		// require host:port.
		if strings.HasPrefix(hp, ":") {
			if _, err := strconv.Atoi(hp[1:]); err != nil {
				return fmt.Errorf("port not numeric: %q", hp)
			}
			continue
		}
		i := strings.LastIndex(hp, ":")
		if i < 0 {
			return fmt.Errorf("expected host:port, got %q", hp)
		}
		if _, err := strconv.Atoi(hp[i+1:]); err != nil {
			return fmt.Errorf("port not numeric in %q", hp)
		}
	}
	return nil
}

func optionalIntInRange(c *ConfigError, get Looker, name string, lo, hi int) {
	v := strings.TrimSpace(get(name))
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		c.Add(name, v, "not an integer")
		return
	}
	if n < lo || n > hi {
		c.Add(name, v, fmt.Sprintf("out of range [%d-%d]", lo, hi))
	}
}

// getenv is split out so OSLooker can call os.Getenv without
// importing os at top of file (avoids a stale import when the
// helper module gets unused).
func getenv(k string) string { return osGetenv(k) }
