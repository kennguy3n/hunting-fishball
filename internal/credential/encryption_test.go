package credential_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/credential"
)

func TestGenerateDEK(t *testing.T) {
	t.Parallel()

	dek1, err := credential.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if len(dek1) != credential.DEKSize {
		t.Fatalf("DEK size: got %d, want %d", len(dek1), credential.DEKSize)
	}

	dek2, err := credential.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if bytes.Equal(dek1, dek2) {
		t.Fatalf("two consecutive DEKs must not be equal")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	t.Parallel()

	dek, err := credential.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty", []byte{}},
		{"short", []byte("hello")},
		{"ascii", []byte("the quick brown fox jumps over the lazy dog")},
		{"binary", append([]byte{0x00, 0xff, 0x10}, make([]byte, 4096)...)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ct, err := credential.Encrypt(tc.in, dek)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if len(ct) < credential.Overhead {
				t.Fatalf("ciphertext too short: %d bytes", len(ct))
			}

			pt, err := credential.Decrypt(ct, dek)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(pt, tc.in) {
				t.Fatalf("plaintext mismatch")
			}
		})
	}
}

func TestEncrypt_NonceUniqueness(t *testing.T) {
	t.Parallel()

	dek, err := credential.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain := []byte("same plaintext")

	const n = 64
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		ct, err := credential.Encrypt(plain, dek)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		nonce := string(ct[:credential.NonceSize])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce reused after %d encryptions", i+1)
		}
		seen[nonce] = struct{}{}
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	t.Parallel()

	dek1, _ := credential.GenerateDEK()
	dek2, _ := credential.GenerateDEK()

	ct, err := credential.Encrypt([]byte("secret"), dek1)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if _, err := credential.Decrypt(ct, dek2); !errors.Is(err, credential.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	t.Parallel()

	dek, _ := credential.GenerateDEK()

	ct, err := credential.Encrypt([]byte("secret"), dek)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	tampered := append([]byte(nil), ct...)
	// Flip a bit in the body, after the nonce.
	tampered[credential.NonceSize+1] ^= 0x40

	if _, err := credential.Decrypt(tampered, dek); !errors.Is(err, credential.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	t.Parallel()

	dek, _ := credential.GenerateDEK()

	if _, err := credential.Decrypt([]byte("nope"), dek); !errors.Is(err, credential.ErrCiphertextTooShort) {
		t.Fatalf("expected ErrCiphertextTooShort, got %v", err)
	}
}

func TestEncrypt_RejectsBadDEK(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		dek := make([]byte, n)
		_, _ = rand.Read(dek)
		if _, err := credential.Encrypt([]byte("x"), dek); !errors.Is(err, credential.ErrInvalidDEK) {
			t.Fatalf("Encrypt with %d-byte key: expected ErrInvalidDEK, got %v", n, err)
		}
		if _, err := credential.Decrypt(make([]byte, credential.Overhead+1), dek); !errors.Is(err, credential.ErrInvalidDEK) {
			t.Fatalf("Decrypt with %d-byte key: expected ErrInvalidDEK, got %v", n, err)
		}
	}
}

func TestEncryptCredential_RoundTrip(t *testing.T) {
	t.Parallel()

	dek, _ := credential.GenerateDEK()
	original := []byte(`{"client_id":"abc","client_secret":"hunter2"}`)

	cred := &credential.ConnectorCredential{
		TenantID:      "t-1",
		SourceID:      "s-1",
		ConnectorName: "google-drive",
		Plaintext:     append([]byte(nil), original...),
	}

	if err := credential.EncryptCredential(cred, dek); err != nil {
		t.Fatalf("EncryptCredential: %v", err)
	}
	if cred.Plaintext != nil {
		t.Fatalf("Plaintext must be cleared after EncryptCredential")
	}
	if len(cred.Ciphertext) < credential.Overhead {
		t.Fatalf("ciphertext too short")
	}

	if err := credential.DecryptCredential(cred, dek); err != nil {
		t.Fatalf("DecryptCredential: %v", err)
	}
	if !bytes.Equal(cred.Plaintext, original) {
		t.Fatalf("decrypted plaintext mismatch")
	}
	if cred.TenantID != "t-1" || cred.SourceID != "s-1" || cred.ConnectorName != "google-drive" {
		t.Fatalf("metadata clobbered")
	}
}

func TestEncryptCredential_NilAndEmpty(t *testing.T) {
	t.Parallel()

	dek, _ := credential.GenerateDEK()

	if err := credential.EncryptCredential(nil, dek); err == nil {
		t.Fatal("expected error for nil cred")
	}
	if err := credential.EncryptCredential(&credential.ConnectorCredential{}, dek); err == nil {
		t.Fatal("expected error for empty plaintext")
	}
	if err := credential.DecryptCredential(nil, dek); err == nil {
		t.Fatal("expected error for nil cred")
	}
	if err := credential.DecryptCredential(&credential.ConnectorCredential{}, dek); err == nil {
		t.Fatal("expected error for empty ciphertext")
	}
}

func TestDecryptCredential_TamperedFails(t *testing.T) {
	t.Parallel()

	dek, _ := credential.GenerateDEK()
	cred := &credential.ConnectorCredential{
		Plaintext: []byte("hunter2"),
	}
	if err := credential.EncryptCredential(cred, dek); err != nil {
		t.Fatalf("EncryptCredential: %v", err)
	}
	cred.Ciphertext[len(cred.Ciphertext)-1] ^= 0x01

	if err := credential.DecryptCredential(cred, dek); !errors.Is(err, credential.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
}
