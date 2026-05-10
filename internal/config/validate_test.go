package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/config"
)

func TestValidateAPI_HappyPath(t *testing.T) {
	t.Parallel()
	look := config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL": "postgres://u:p@h:5432/d",
		"CONTEXT_ENGINE_QDRANT_URL":   "http://qdrant:6333",
		"CONTEXT_ENGINE_REDIS_URL":    "redis://redis:6379/0",
	})
	if err := config.ValidateAPI(look); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateAPI_MissingRequiredFields(t *testing.T) {
	t.Parallel()
	err := config.ValidateAPI(config.MapLooker(map[string]string{}))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !config.IsConfigError(err) {
		t.Fatalf("expected ConfigError, got %T", err)
	}
	msg := err.Error()
	for _, want := range []string{"CONTEXT_ENGINE_DATABASE_URL", "CONTEXT_ENGINE_QDRANT_URL"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %s in %s", want, msg)
		}
	}
}

func TestValidateAPI_BadSchemeRejected(t *testing.T) {
	t.Parallel()
	err := config.ValidateAPI(config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL": "mysql://h/d",
		"CONTEXT_ENGINE_QDRANT_URL":   "ftp://q",
	}))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "scheme must be") {
		t.Fatalf("expected scheme error: %v", err)
	}
}

func TestValidateAPI_OptionalRedisOK(t *testing.T) {
	t.Parallel()
	err := config.ValidateAPI(config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL": "postgres://h/d",
		"CONTEXT_ENGINE_QDRANT_URL":   "http://qdrant:6333",
	}))
	if err != nil {
		t.Fatalf("redis is optional; got %v", err)
	}
}

func TestValidateAPI_BadRedisURLRejected(t *testing.T) {
	t.Parallel()
	err := config.ValidateAPI(config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL": "postgres://h/d",
		"CONTEXT_ENGINE_QDRANT_URL":   "http://qdrant:6333",
		"CONTEXT_ENGINE_REDIS_URL":    "http://redis:6379",
	}))
	if err == nil || !strings.Contains(err.Error(), "REDIS_URL") {
		t.Fatalf("expected redis scheme rejection: %v", err)
	}
}

func TestValidateIngest_HappyPath(t *testing.T) {
	t.Parallel()
	err := config.ValidateIngest(config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL":  "postgres://h/d",
		"CONTEXT_ENGINE_KAFKA_BROKERS": "kafka1:9092,kafka2:9092",
		"CONTEXT_ENGINE_KAFKA_TOPICS":  "ingest",
	}))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateIngest_MissingBrokersRejected(t *testing.T) {
	t.Parallel()
	err := config.ValidateIngest(config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL": "postgres://h/d",
		"CONTEXT_ENGINE_KAFKA_TOPICS": "ingest",
	}))
	if err == nil || !strings.Contains(err.Error(), "KAFKA_BROKERS") {
		t.Fatalf("expected brokers required: %v", err)
	}
}

func TestValidateIngest_MalformedBrokerRejected(t *testing.T) {
	t.Parallel()
	err := config.ValidateIngest(config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL":  "postgres://h/d",
		"CONTEXT_ENGINE_KAFKA_BROKERS": "not-a-broker",
		"CONTEXT_ENGINE_KAFKA_TOPICS":  "ingest",
	}))
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("expected broker host:port error: %v", err)
	}
}

func TestValidateIngest_NonNumericPortRejected(t *testing.T) {
	t.Parallel()
	err := config.ValidateIngest(config.MapLooker(map[string]string{
		"CONTEXT_ENGINE_DATABASE_URL":  "postgres://h/d",
		"CONTEXT_ENGINE_KAFKA_BROKERS": "kafka:abc",
		"CONTEXT_ENGINE_KAFKA_TOPICS":  "ingest",
	}))
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected port int error: %v", err)
	}
}

func TestConfigError_AsErrorNilWhenEmpty(t *testing.T) {
	t.Parallel()
	c := &config.ConfigError{}
	if err := c.AsError(); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestIsConfigError_Wrapped(t *testing.T) {
	t.Parallel()
	c := &config.ConfigError{}
	c.Add("X", "y", "msg")
	wrapped := errors.New("outer: " + c.Error())
	if config.IsConfigError(wrapped) {
		t.Fatalf("plain string-wrapped error should not be a ConfigError")
	}
	if !config.IsConfigError(c) {
		t.Fatalf("direct ConfigError should match")
	}
}
