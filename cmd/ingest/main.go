// Command context-engine-ingest is the Kafka consumer / pipeline
// coordinator binary. The Phase 0 build is a stub: it imports the
// connector registry and the audit-log packages so blank-import side
// effects compile, but does not yet wire a Kafka consumer.
//
// Real wiring lands in Phase 1.
package main

import (
	"fmt"
	"os"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

func main() {
	fmt.Fprintln(os.Stderr, "context-engine-ingest: phase-0 stub")
	fmt.Fprintf(os.Stderr, "registered connectors: %v\n", connector.ListSourceConnectors())
}
