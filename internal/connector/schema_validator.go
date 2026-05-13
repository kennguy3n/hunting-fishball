// schema_validator.go — Round-20/21 Task 16.
//
// Connector-credential schema validation. Each connector
// optionally exports a JSON Schema for its credential blob via
// the SchemaProvider optional interface. The preview handler
// (POST /v1/admin/sources/preview) calls Validate before
// delegating to the connector's Validate() so a malformed
// credentials blob fails fast with a precise field-level error.
//
// The schema dialect is a minimal subset of JSON-Schema-draft-7
// covering object types, required fields, string vs number vs
// boolean, and enum constraints. It's intentionally lightweight
// — pulling in github.com/santhosh-tekuri/jsonschema would
// inflate go.mod for what is, at this point, mostly an
// allow-list of credential keys.
package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CredentialSchemaProvider is the optional interface that
// connectors implement to expose their credential schema. The
// preview handler uses this to validate the credential blob
// before calling Validate().
type CredentialSchemaProvider interface {
	// CredentialSchema returns a JSON Schema (draft-7 subset)
	// describing the connector's expected credentials JSON. The
	// returned bytes must be a valid JSON object.
	CredentialSchema() []byte
}

// CredentialSchema is the parsed shape of a credential schema.
// Subset only — see ValidateCredentials for the supported subset.
type CredentialSchema struct {
	Type       string                       `json:"type"`
	Required   []string                     `json:"required"`
	Properties map[string]*CredentialSchema `json:"properties"`
	Enum       []any                        `json:"enum"`
	// AdditionalProperties controls whether unknown keys are
	// rejected. Defaults to true (additional keys allowed) which
	// matches the draft-7 default.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
}

// ValidateCredentials validates raw against schema. Returns the
// sorted list of validation errors, or nil if valid.
//
// The validator covers:
//   - top-level type=object with required keys + property types
//   - per-property type=string|number|boolean|integer
//   - per-property enum=[...] allow-list
//   - additionalProperties=false to reject unknown keys
//
// Anything outside that subset (oneOf, $ref, conditionals, etc.)
// is silently ignored — the connector's Validate() catches
// semantic problems the schema can't express.
func ValidateCredentials(raw []byte, schema []byte) []string {
	if len(schema) == 0 {
		// No schema means no validation — caller falls through
		// to the connector's Validate(). Not an error.
		return nil
	}
	if len(raw) == 0 {
		return []string{"credentials: empty body"}
	}
	var s CredentialSchema
	if err := json.Unmarshal(schema, &s); err != nil {
		return []string{fmt.Sprintf("schema: invalid JSON: %v", err)}
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []string{fmt.Sprintf("credentials: invalid JSON: %v", err)}
	}
	out := []string{}
	if s.Type != "" && s.Type != "object" {
		out = append(out, "schema: only object schemas are supported at the top level")
	}
	for _, req := range s.Required {
		if _, ok := doc[req]; !ok {
			out = append(out, fmt.Sprintf("credentials: missing required field %q", req))
		}
	}
	// AdditionalProperties default = true; only reject when explicit false.
	if s.AdditionalProperties != nil && !*s.AdditionalProperties {
		for k := range doc {
			if _, declared := s.Properties[k]; !declared {
				out = append(out, fmt.Sprintf("credentials: unknown field %q", k))
			}
		}
	}
	for name, prop := range s.Properties {
		val, ok := doc[name]
		if !ok {
			continue
		}
		if prop.Type != "" {
			if err := checkType(val, prop.Type); err != "" {
				out = append(out, fmt.Sprintf("credentials: field %q %s", name, err))
			}
		}
		if len(prop.Enum) > 0 {
			match := false
			for _, allowed := range prop.Enum {
				if allowed == val {
					match = true

					break
				}
			}
			if !match {
				out = append(out, fmt.Sprintf("credentials: field %q value %v not in enum", name, val))
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)

	return out
}

// checkType returns an empty string when v conforms to typ,
// otherwise a human-readable mismatch description.
func checkType(v any, typ string) string {
	switch typ {
	case "string":
		if _, ok := v.(string); !ok {
			return "must be a string"
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return "must be a boolean"
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return "must be a number"
		}
	case "integer":
		// JSON has no separate integer type; require float64
		// that is whole-valued.
		f, ok := v.(float64)
		if !ok {
			return "must be an integer"
		}
		if f != float64(int64(f)) {
			return "must be a whole integer"
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return "must be an array"
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return "must be an object"
		}
	}

	return ""
}

// ErrInvalidCredentials is returned by ValidateCredentialsErr
// when one or more validation errors are present.
var ErrInvalidCredentials = errors.New("connector: invalid credentials")

// ValidateCredentialsErr is the error-returning wrapper around
// ValidateCredentials, suitable for use from connector Validate
// implementations. Returns a wrapped ErrInvalidCredentials when
// validation fails so callers can errors.Is the sentinel.
func ValidateCredentialsErr(raw, schema []byte) error {
	errs := ValidateCredentials(raw, schema)
	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrInvalidCredentials, strings.Join(errs, "; "))
}
