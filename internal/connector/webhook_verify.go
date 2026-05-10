// webhook_verify.go — Phase 8 / Task 9 webhook signature verification.
//
// Each `WebhookReceiver` (Jira, GitHub, GitLab, Teams, …) presents
// its own signature header convention; this file provides the small
// set of HMAC + token primitives the receivers compose. The plain
// helpers do not assume a specific provider so connectors can drop
// in a one-liner that matches their docs:
//
//	GitHub:  VerifyHMACSHA256(secret, payload, headers.Get("X-Hub-Signature-256"))
//	GitLab:  VerifyTokenEqual(secret, headers.Get("X-Gitlab-Token"))
//	Jira:    VerifyHMACSHA256(secret, payload, headers.Get("X-Hub-Signature-256"))
//	Teams:   VerifyHMACSHA256(secret, payload, headers.Get("Authorization"))
//
// The helpers all rely on `hmac.Equal` for constant-time comparison
// to keep the verification path free of timing oracles.
package connector

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is required by some legacy webhook providers (e.g. GitHub `X-Hub-Signature`).
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"strings"
)

// ErrWebhookSignatureMissing is returned when the receiver was
// configured with a secret but the request omitted the signature
// header. Receivers should reject the request with HTTP 401.
var ErrWebhookSignatureMissing = errors.New("webhook: signature header missing")

// ErrWebhookSignatureInvalid is returned when the signature header
// is present but does not match the computed digest. Receivers
// should reject the request with HTTP 401.
var ErrWebhookSignatureInvalid = errors.New("webhook: signature mismatch")

// ErrWebhookTokenInvalid is returned when a non-HMAC token check
// (e.g. GitLab's plain `X-Gitlab-Token`) fails.
var ErrWebhookTokenInvalid = errors.New("webhook: token mismatch")

// VerifyHMACSHA256 validates `header` (typically `X-Hub-Signature-256`
// or `X-Signature`) against the SHA-256 HMAC of `payload` keyed with
// `secret`. The function strips an optional `sha256=` prefix that
// GitHub/Jira embed.
//
// When secret is empty the function returns nil so connectors can
// disable verification by leaving the secret unset (development
// flow). Callers that want strict mode should check for an empty
// secret themselves before calling.
func VerifyHMACSHA256(secret []byte, payload []byte, header string) error {
	return verifyHMAC(secret, payload, header, sha256.New, "sha256=")
}

// VerifyHMACSHA1 is the SHA-1 variant required by legacy GitHub
// webhooks that send `X-Hub-Signature` instead of the SHA-256 header.
//
//nolint:gosec // SHA-1 is required for legacy GitHub webhooks.
func VerifyHMACSHA1(secret []byte, payload []byte, header string) error {
	return verifyHMAC(secret, payload, header, sha1.New, "sha1=")
}

// VerifyTokenEqual is constant-time-compares a shared-secret header
// (e.g. GitLab's `X-Gitlab-Token`).
func VerifyTokenEqual(secret string, header string) error {
	if secret == "" {
		return nil
	}
	if header == "" {
		return ErrWebhookSignatureMissing
	}
	if !hmac.Equal([]byte(secret), []byte(header)) {
		return ErrWebhookTokenInvalid
	}
	return nil
}

func verifyHMAC(secret []byte, payload []byte, header string, h func() hash.Hash, prefix string) error {
	if len(secret) == 0 {
		return nil
	}
	if header == "" {
		return ErrWebhookSignatureMissing
	}
	header = strings.TrimSpace(header)
	header = strings.TrimPrefix(header, prefix)
	expected, err := hex.DecodeString(header)
	if err != nil {
		return ErrWebhookSignatureInvalid
	}
	mac := hmac.New(h, secret)
	if _, err := mac.Write(payload); err != nil {
		return err
	}
	if !hmac.Equal(expected, mac.Sum(nil)) {
		return ErrWebhookSignatureInvalid
	}
	return nil
}

// SignHMACSHA256 returns the prefixed hex-encoded SHA-256 HMAC of
// payload keyed by secret. Used by tests to mint a valid signature.
func SignHMACSHA256(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// SignHMACSHA1 returns the prefixed hex-encoded SHA-1 HMAC. Used by
// tests when validating legacy GitHub webhooks.
func SignHMACSHA1(secret, payload []byte) string {
	mac := hmac.New(sha1.New, secret) //nolint:gosec // see VerifyHMACSHA1
	mac.Write(payload)
	return "sha1=" + hex.EncodeToString(mac.Sum(nil))
}

// FirstHeader returns the first value for `name` from the supplied
// header map performing a case-insensitive key lookup. Returns "" if
// missing. Useful for connectors that need to read provider-specific
// signature headers from the WebhookVerifier interface map.
func FirstHeader(headers map[string][]string, name string) string {
	if vals, ok := headers[name]; ok && len(vals) > 0 {
		return vals[0]
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
