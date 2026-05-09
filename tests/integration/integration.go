//go:build integration

// Package integration runs Phase 3 end-to-end integration tests
// against the Python ML microservices (Docling, embedding, Mem0)
// brought up by `docker compose up -d docling embedding memory`.
//
// Each test dials the real gRPC port (host network: 50051/50052/50053
// by default), executes the proto contract, and asserts that the
// Python side returns a sensible response. Heavy ML deps (torch,
// sentence-transformers, docling, mem0ai) only need to be installed
// inside the docker images — the Go side just speaks gRPC.
//
// Build tag `integration` keeps these out of `go test ./...` so a
// developer without docker compose can still run unit tests.
package integration

import (
	"os"
	"strings"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return def
}
