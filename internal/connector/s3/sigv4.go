// Package s3 — sigv4.go implements the minimum AWS Signature
// Version 4 signing we need to talk to S3 (and S3-compatible
// stores like MinIO, Backblaze B2, R2) over plain net/http
// without pulling in the AWS SDK. See
// https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html
// for the canonical algorithm; we implement the request-signing
// path only (no presigned URLs, no STS chained credentials).
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	sigV4Service   = "s3"
	emptyPayload   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// signRequestV4 mutates req to add the AWS SigV4 Authorization
// header (and the required x-amz-* headers). bodyHash is the
// hex-encoded SHA-256 of the request body (use emptyPayload when
// the body is empty). region is the AWS region (e.g. "us-east-1").
//
// We always set X-Amz-Content-Sha256, X-Amz-Date, and Host before
// signing so the canonical request matches what the server will
// compute. The function intentionally does NOT modify the URL or
// payload — callers are responsible for shaping the URL exactly
// the way the canonical request expects.
func signRequestV4(req *http.Request, accessKeyID, secretKey, region, bodyHash string, now time.Time) {
	if bodyHash == "" {
		bodyHash = emptyPayload
	}
	if region == "" {
		region = "us-east-1"
	}
	t := now.UTC()
	amzDate := t.Format("20060102T150405Z")
	shortDate := t.Format("20060102")

	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)

	canonHeaders, signedHeaders := canonicalHeaders(req)
	canonRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonHeaders,
		signedHeaders,
		bodyHash,
	}, "\n")
	canonHash := sha256Hex([]byte(canonRequest))

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", shortDate, region, sigV4Service)
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		credentialScope,
		canonHash,
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(shortDate))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(sigV4Service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	auth := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm,
		accessKeyID,
		credentialScope,
		signedHeaders,
		signature,
	)
	req.Header.Set("Authorization", auth)
}

// canonicalHeaders returns the canonical-headers block and the
// semicolon-separated SignedHeaders list. We sign Host,
// X-Amz-Content-Sha256, and X-Amz-Date — the minimum required by
// the S3 endpoint.
func canonicalHeaders(req *http.Request) (string, string) {
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		val := req.Header.Get(n)
		if n == "host" && val == "" {
			val = req.URL.Host
		}
		b.WriteString(n)
		b.WriteString(":")
		b.WriteString(strings.TrimSpace(val))
		b.WriteString("\n")
	}

	return b.String(), strings.Join(names, ";")
}

// canonicalURI returns the path component of the URL, URI-encoded
// per RFC 3986. Empty paths become "/".
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}

	return u.EscapedPath()
}

// canonicalQuery returns the canonical query string: keys sorted
// lexicographically, every key+value RFC3986-percent-encoded.
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(q))
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, encodeRFC3986(k)+"="+encodeRFC3986(v))
		}
	}

	return strings.Join(parts, "&")
}

// encodeRFC3986 percent-encodes a string per RFC 3986 / AWS SigV4:
// unreserved characters (A-Z, a-z, 0-9, -, _, ., ~) pass through,
// everything else is %XX hex-escaped (uppercase).
func encodeRFC3986(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			for _, c := range []byte(string(r)) {
				b.WriteString(fmt.Sprintf("%%%02X", c))
			}
		}
	}

	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)

	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)

	return hex.EncodeToString(h[:])
}
