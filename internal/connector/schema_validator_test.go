package connector_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

func TestValidateCredentials_NoSchema(t *testing.T) {
	t.Parallel()
	if errs := connector.ValidateCredentials([]byte(`{}`), nil); errs != nil {
		t.Fatalf("nil schema should pass: %v", errs)
	}
}

func TestValidateCredentials_EmptyBody(t *testing.T) {
	t.Parallel()
	if errs := connector.ValidateCredentials(nil, []byte(`{}`)); len(errs) != 1 {
		t.Fatalf("expected single empty-body error, got %v", errs)
	}
}

// representative schemas covering the 4 connector families used
// throughout the codebase: api-key+basic-auth (Freshdesk),
// bearer-token (Zendesk-style API token + email + subdomain),
// instance+basic (ServiceNow), and workspace+app-password
// (Bitbucket).
const (
	freshdeskSchema = `{
		"type": "object",
		"required": ["api_key", "domain"],
		"additionalProperties": false,
		"properties": {
			"api_key": {"type": "string"},
			"domain":  {"type": "string"}
		}
	}`
	zendeskSchema = `{
		"type": "object",
		"required": ["subdomain", "email", "api_token"],
		"additionalProperties": false,
		"properties": {
			"subdomain": {"type": "string"},
			"email":     {"type": "string"},
			"api_token": {"type": "string"},
			"auth_type": {"type": "string", "enum": ["api_token", "oauth"]}
		}
	}`
	servicenowSchema = `{
		"type": "object",
		"required": ["instance", "username", "password"],
		"properties": {
			"instance": {"type": "string"},
			"username": {"type": "string"},
			"password": {"type": "string"},
			"timeout_seconds": {"type": "integer"}
		}
	}`
	bitbucketSchema = `{
		"type": "object",
		"required": ["username", "app_password", "workspace", "repo"],
		"properties": {
			"username": {"type": "string"},
			"app_password": {"type": "string"},
			"workspace": {"type": "string"},
			"repo": {"type": "string"}
		}
	}`
)

func TestValidateCredentials_ValidConfigs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, schema, doc string
	}{
		{"freshdesk", freshdeskSchema, `{"api_key":"k","domain":"acme"}`},
		{"zendesk", zendeskSchema, `{"subdomain":"x","email":"a@b","api_token":"t","auth_type":"oauth"}`},
		{"servicenow", servicenowSchema, `{"instance":"x","username":"u","password":"p","timeout_seconds":10}`},
		{"bitbucket", bitbucketSchema, `{"username":"u","app_password":"p","workspace":"w","repo":"r"}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if errs := connector.ValidateCredentials([]byte(tc.doc), []byte(tc.schema)); errs != nil {
				t.Fatalf("valid doc failed: %v", errs)
			}
		})
	}
}

func TestValidateCredentials_InvalidConfigs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, schema, doc, contains string
	}{
		{"freshdesk_missing_required", freshdeskSchema, `{"api_key":"k"}`, "missing required field \"domain\""},
		{"freshdesk_unknown_field", freshdeskSchema, `{"api_key":"k","domain":"x","ENV":"prod"}`, "unknown field \"ENV\""},
		{"zendesk_bad_enum", zendeskSchema, `{"subdomain":"x","email":"a","api_token":"t","auth_type":"basic"}`, "not in enum"},
		{"zendesk_wrong_type", zendeskSchema, `{"subdomain":"x","email":"a","api_token":123}`, "must be a string"},
		{"servicenow_wrong_integer", servicenowSchema, `{"instance":"x","username":"u","password":"p","timeout_seconds":10.5}`, "must be a whole integer"},
		{"bitbucket_missing", bitbucketSchema, `{"username":"u","app_password":"p"}`, "missing required field"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := connector.ValidateCredentials([]byte(tc.doc), []byte(tc.schema))
			if len(errs) == 0 {
				t.Fatalf("expected errors")
			}
			joined := strings.Join(errs, "|")
			if !strings.Contains(joined, tc.contains) {
				t.Fatalf("missing expected %q, got %q", tc.contains, joined)
			}
		})
	}
}

func TestValidateCredentialsErr_WrapsSentinel(t *testing.T) {
	t.Parallel()
	err := connector.ValidateCredentialsErr([]byte(`{}`), []byte(freshdeskSchema))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestValidateCredentials_InvalidSchemaJSON(t *testing.T) {
	t.Parallel()
	if errs := connector.ValidateCredentials([]byte(`{}`), []byte(`not json`)); len(errs) == 0 {
		t.Fatalf("expected schema parse error")
	}
}

func TestValidateCredentials_InvalidDocJSON(t *testing.T) {
	t.Parallel()
	if errs := connector.ValidateCredentials([]byte(`not json`), []byte(`{"type":"object"}`)); len(errs) == 0 {
		t.Fatalf("expected doc parse error")
	}
}
