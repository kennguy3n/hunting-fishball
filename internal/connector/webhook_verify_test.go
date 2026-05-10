package connector_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

func TestVerifyHMACSHA256_HappyPath(t *testing.T) {
	t.Parallel()
	secret := []byte("supersecret")
	payload := []byte(`{"event":"push"}`)
	sig := connector.SignHMACSHA256(secret, payload)
	if err := connector.VerifyHMACSHA256(secret, payload, sig); err != nil {
		t.Fatalf("expected verify ok, got %v", err)
	}
}

func TestVerifyHMACSHA256_InvalidSignature(t *testing.T) {
	t.Parallel()
	secret := []byte("supersecret")
	payload := []byte(`{"event":"push"}`)
	if err := connector.VerifyHMACSHA256(secret, payload, "sha256=00"); !errors.Is(err, connector.ErrWebhookSignatureInvalid) {
		t.Fatalf("expected invalid sig, got %v", err)
	}
}

func TestVerifyHMACSHA256_MissingHeader(t *testing.T) {
	t.Parallel()
	if err := connector.VerifyHMACSHA256([]byte("s"), []byte("p"), ""); !errors.Is(err, connector.ErrWebhookSignatureMissing) {
		t.Fatalf("expected missing, got %v", err)
	}
}

func TestVerifyHMACSHA256_EmptySecretSkipsVerification(t *testing.T) {
	t.Parallel()
	if err := connector.VerifyHMACSHA256(nil, []byte("p"), ""); err != nil {
		t.Fatalf("empty secret should bypass: %v", err)
	}
}

func TestVerifyHMACSHA256_TolersAcceptsRawHexAsWellAsPrefix(t *testing.T) {
	t.Parallel()
	secret := []byte("supersecret")
	payload := []byte(`{"event":"x"}`)
	sig := connector.SignHMACSHA256(secret, payload)
	stripped := strings.TrimPrefix(sig, "sha256=")
	if err := connector.VerifyHMACSHA256(secret, payload, stripped); err != nil {
		t.Fatalf("raw hex must verify: %v", err)
	}
}

func TestVerifyHMACSHA256_PayloadTamperingDetected(t *testing.T) {
	t.Parallel()
	secret := []byte("supersecret")
	payload := []byte(`{"event":"x"}`)
	sig := connector.SignHMACSHA256(secret, payload)
	tampered := []byte(`{"event":"y"}`)
	if err := connector.VerifyHMACSHA256(secret, tampered, sig); !errors.Is(err, connector.ErrWebhookSignatureInvalid) {
		t.Fatalf("expected mismatch on tampered payload, got %v", err)
	}
}

func TestVerifyHMACSHA1_HappyPath(t *testing.T) {
	t.Parallel()
	secret := []byte("supersecret")
	payload := []byte(`{"event":"push"}`)
	sig := connector.SignHMACSHA1(secret, payload)
	if err := connector.VerifyHMACSHA1(secret, payload, sig); err != nil {
		t.Fatalf("expected verify ok, got %v", err)
	}
}

func TestVerifyTokenEqual(t *testing.T) {
	t.Parallel()
	if err := connector.VerifyTokenEqual("token", "token"); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if err := connector.VerifyTokenEqual("token", "wrong"); !errors.Is(err, connector.ErrWebhookTokenInvalid) {
		t.Fatalf("expected mismatch, got %v", err)
	}
	if err := connector.VerifyTokenEqual("token", ""); !errors.Is(err, connector.ErrWebhookSignatureMissing) {
		t.Fatalf("expected missing, got %v", err)
	}
	if err := connector.VerifyTokenEqual("", ""); err != nil {
		t.Fatalf("empty secret bypass: %v", err)
	}
}
