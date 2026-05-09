// Package credential implements the AES-256-GCM envelope-encryption
// pattern used by hunting-fishball to protect connector credentials at
// rest. The shape mirrors uneycom/ai-agent-platform's
// pkg/crypto/encryption package: a master KEK derives per-context DEKs,
// individual records are sealed with their DEK, and the DEK itself is
// stored alongside the ciphertext as an opaque blob.
//
// The package exposes two layers:
//
//  1. A low-level pair of `Encrypt` / `Decrypt` / `GenerateDEK` functions
//     that operate on raw byte slices. These are what callers writing
//     their own envelope schemes should use.
//
//  2. A higher-level `EncryptCredential` / `DecryptCredential` pair that
//     operates on a `ConnectorCredential` struct — the shape persisted in
//     the platform-backend's connector_credentials table.
//
// Output format for ciphertext is `[12-byte nonce][ciphertext][16-byte
// auth tag]`, identical to the platform-backend AES-GCM impl so payloads
// are wire-compatible across services.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	// DEKSize is 32 bytes — AES-256.
	DEKSize = 32
	// NonceSize is 12 bytes (96 bits) — NIST SP 800-38D recommended GCM
	// nonce size.
	NonceSize = 12
	// TagSize is 16 bytes (128 bits) — GCM authentication tag.
	TagSize = 16
	// Overhead is the total bytes added to plaintext: nonce + tag.
	Overhead = NonceSize + TagSize
)

var (
	// ErrInvalidDEK is returned when a DEK is the wrong size or empty.
	ErrInvalidDEK = errors.New("credential: DEK must be exactly 32 bytes")

	// ErrCiphertextTooShort is returned when a ciphertext is too short to
	// even contain a nonce + auth tag.
	ErrCiphertextTooShort = errors.New("credential: ciphertext shorter than nonce+tag")

	// ErrDecryptionFailed is returned when GCM authentication fails:
	// wrong DEK, tampered ciphertext, or truncated payload. We never
	// surface the underlying GCM error so we don't leak detail to
	// timing-side-channel observers.
	ErrDecryptionFailed = errors.New("credential: decryption failed")
)

// GenerateDEK returns a freshly-generated 32-byte DEK from
// crypto/rand. Callers should wrap the DEK with the platform KEK
// (HKDF-derived from the master key) before persisting it.
func GenerateDEK() ([]byte, error) {
	dek := make([]byte, DEKSize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("credential: read random DEK: %w", err)
	}

	return dek, nil
}

// Encrypt seals plaintext with the supplied DEK using AES-256-GCM and a
// fresh random nonce. The returned slice is `nonce || ciphertext || tag`.
//
// Encrypt is safe to call concurrently with the same DEK; each call
// allocates its own GCM AEAD.
func Encrypt(plaintext, dek []byte) ([]byte, error) {
	if len(dek) != DEKSize {
		return nil, ErrInvalidDEK
	}

	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credential: read nonce: %w", err)
	}

	// Seal appends ciphertext + tag to the nonce slice.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt. Returns ErrDecryptionFailed if the DEK is
// wrong, the nonce was tampered with, or the auth tag failed to verify.
func Decrypt(ciphertext, dek []byte) ([]byte, error) {
	if len(dek) != DEKSize {
		return nil, ErrInvalidDEK
	}
	if len(ciphertext) < Overhead {
		return nil, ErrCiphertextTooShort
	}

	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}

	nonce := ciphertext[:NonceSize]
	body := ciphertext[NonceSize:]

	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plain, nil
}

func newGCM(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("credential: new aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential: new gcm: %w", err)
	}

	return gcm, nil
}

// ConnectorCredential is the persisted shape for a single connector's
// credentials. Only Plaintext and Ciphertext are mutated by
// EncryptCredential / DecryptCredential — everything else is set by
// callers and copied through.
//
// On disk: only Ciphertext is non-nil.
// In memory after DecryptCredential: only Plaintext is non-nil.
type ConnectorCredential struct {
	// TenantID is the multi-tenant scope. Persisted alongside the
	// ciphertext so a leaked row cannot be used outside its tenant.
	TenantID string

	// SourceID identifies the specific source instance these credentials
	// are bound to.
	SourceID string

	// ConnectorName matches the registry name (e.g. "google-drive").
	ConnectorName string

	// Plaintext holds the decrypted credentials in memory. Callers MUST
	// zero this slice (e.g. via crypto/subtle.ConstantTimeCompare's
	// constructive cousins) when they're done with it.
	Plaintext []byte

	// Ciphertext is what gets persisted to the connector_credentials
	// table. Layout: [12-byte nonce][ciphertext][16-byte tag].
	Ciphertext []byte
}

// EncryptCredential encrypts cred.Plaintext into cred.Ciphertext using
// dek and clears cred.Plaintext on success. If encryption fails,
// cred.Plaintext is left intact so the caller can retry.
func EncryptCredential(cred *ConnectorCredential, dek []byte) error {
	if cred == nil {
		return errors.New("credential: nil credential")
	}
	if len(cred.Plaintext) == 0 {
		return errors.New("credential: empty plaintext")
	}

	ct, err := Encrypt(cred.Plaintext, dek)
	if err != nil {
		return err
	}
	cred.Ciphertext = ct

	// Zero the plaintext so it doesn't leak into stale memory or logs.
	for i := range cred.Plaintext {
		cred.Plaintext[i] = 0
	}
	cred.Plaintext = nil

	return nil
}

// DecryptCredential is the inverse of EncryptCredential. On success
// cred.Plaintext holds the recovered bytes and cred.Ciphertext is
// preserved (so callers can re-persist without re-encrypting).
func DecryptCredential(cred *ConnectorCredential, dek []byte) error {
	if cred == nil {
		return errors.New("credential: nil credential")
	}
	if len(cred.Ciphertext) == 0 {
		return errors.New("credential: empty ciphertext")
	}

	pt, err := Decrypt(cred.Ciphertext, dek)
	if err != nil {
		return err
	}
	cred.Plaintext = pt

	return nil
}
