package audit

import "context"

// ctxAlias is a tiny alias so the repoReader interface in handler.go can
// reuse it without forcing every test to import context. It stays in its
// own file so renaming is mechanical.
type ctxAlias = context.Context
