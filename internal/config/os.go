package config

import "os"

// osGetenv is a thin shim so validate.go can stay testable without
// importing os directly. The wrapper exists so a future caller can
// substitute a different lookup (e.g. a Vault client) without
// touching the validation rules.
func osGetenv(k string) string { return os.Getenv(k) }
