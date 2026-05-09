SHELL := /bin/bash

GO          ?= go
GOFMT       ?= gofmt
PKGS        := ./...
BIN_DIR     := ./bin

PROTOC      ?= protoc
PROTO_DIR   := ./proto
PROTO_FILES := $(shell find $(PROTO_DIR) -name '*.proto' 2>/dev/null)

# Tools we install into $(GOPATH)/bin via `go install` for proto generation.
TOOLS_BIN   := $(shell $(GO) env GOPATH)/bin

.PHONY: all
all: build

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: build
build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/context-engine-ingest ./cmd/ingest
	$(GO) build -o $(BIN_DIR)/context-engine-api    ./cmd/api

.PHONY: test
test:
	$(GO) test -race -cover $(PKGS)

.PHONY: test-e2e
test-e2e:
	@echo "Bringing up storage plane (postgres, redis, kafka, qdrant)..."
	docker compose up -d --wait
	@echo "Running e2e smoke tests..."
	E2E_ENABLED=1 \
	CONTEXT_ENGINE_DATABASE_URL="host=localhost user=hf password=hf dbname=hunting_fishball port=5432 sslmode=disable" \
	CONTEXT_ENGINE_QDRANT_URL="http://localhost:6333" \
	$(GO) test -tags=e2e -race -count=1 -timeout 5m ./tests/e2e/...

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt:
	$(GOFMT) -s -w $(shell find . -name '*.go' -not -path './proto/*' -not -name '*.pb.go')

.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run $(PKGS); \
	else \
		echo "golangci-lint not installed; skipping. Install: https://golangci-lint.run/"; \
	fi

.PHONY: proto-tools
proto-tools:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

.PHONY: proto-gen
proto-gen: proto-tools
	@if [ -z "$(PROTO_FILES)" ]; then echo "No .proto files found"; exit 0; fi
	@for f in $(PROTO_FILES); do \
		echo "protoc $$f"; \
		PATH=$(TOOLS_BIN):$$PATH $(PROTOC) \
			--proto_path=$(PROTO_DIR) \
			--go_out=$(PROTO_DIR) \
			--go_opt=paths=source_relative \
			--go-grpc_out=$(PROTO_DIR) \
			--go-grpc_opt=paths=source_relative \
			$$f; \
	done

.PHONY: proto-check
proto-check: proto-gen
	@if ! git diff --exit-code -- $(PROTO_DIR); then \
		echo "Generated proto files are out of date. Run 'make proto-gen' and commit."; \
		exit 1; \
	fi

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html
