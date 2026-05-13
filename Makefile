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
	docker compose up -d --wait postgres redis kafka qdrant
	@echo "Running e2e smoke tests..."
	E2E_ENABLED=1 \
	CONTEXT_ENGINE_DATABASE_URL="host=localhost user=hf password=hf dbname=hunting_fishball port=5432 sslmode=disable" \
	CONTEXT_ENGINE_QDRANT_URL="http://localhost:6333" \
	$(GO) test -tags=e2e -race -count=1 -timeout 5m ./tests/e2e/...

.PHONY: test-connector-smoke
test-connector-smoke:
	@echo "Running connector e2e smoke tests (no docker dependencies)..."
	$(GO) test -tags=e2e -race -count=1 -timeout 2m -run '^TestConnectorSmoke' ./tests/e2e/...

.PHONY: bench-e2e
bench-e2e:
	@echo "Running end-to-end P95 benchmark..."
	$(GO) test -tags=e2e -count=1 -timeout 10m -run '^TestE2E_RetrieveP95' ./tests/benchmark/...

.PHONY: test-integration
test-integration:
	@echo "Bringing up Phase 3 ML services (docling, embedding, memory) + falkordb..."
	docker compose up -d --wait docling embedding memory falkordb
	@echo "Running Go ↔ Python integration tests..."
	DOCLING_TARGET=localhost:50051 \
	EMBEDDING_TARGET=localhost:50052 \
	MEMORY_TARGET=localhost:50053 \
	FALKORDB_ADDR=localhost:6380 \
	$(GO) test -tags=integration -race -count=1 -timeout 10m ./tests/integration/...

.PHONY: bench
bench:
	$(GO) test -bench . -benchmem -benchtime=3s ./tests/benchmark/...

.PHONY: capacity-test
capacity-test:
	$(GO) test -count=1 -timeout 10m -v ./tests/capacity/...

.PHONY: services-test
services-test:
	@echo "Running unit tests for Python ML services..."
	cd services && python -m pytest -q --import-mode=importlib

.PHONY: services-protos
services-protos:
	bash services/gen_protos.sh

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

# Round-18 Task 20 — dependency CVE scan. `govulncheck` queries
# the Go vulnerability database for every module + function we
# call. It catches a class of supply-chain failures the test
# suite cannot: a transitive dependency advisory landing
# overnight between a `make test` green run and a deploy.
# Wired into the fast lane via .github/workflows/ci.yml.
.PHONY: vulncheck
vulncheck:
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck ./...; \
	else \
		$(GO) install golang.org/x/vuln/cmd/govulncheck@latest && \
		$$($(GO) env GOPATH)/bin/govulncheck ./...; \
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

.PHONY: alerts-check
alerts-check:
	@echo "Validating Prometheus alert + recording YAML..."
	$(GO) run ./internal/observability/alertcheck \
		deploy/alerts.yaml \
		deploy/recording-rules.yaml \
		deploy/alerts/pipeline_backpressure.yaml \
		deploy/alerts/slo_burn_rate.yaml

.PHONY: eval
eval:
	@echo "Running retrieval eval CI gate against tests/eval/golden_corpus.json..."
	$(GO) test -tags=eval -count=1 -timeout 5m ./tests/eval/...

.PHONY: fuzz
fuzz:
	$(GO) test -run='^$$' -fuzz='^FuzzRetrieveRequestDecode$$' -fuzztime=30s ./internal/retrieval/
	$(GO) test -run='^$$' -fuzz='^FuzzACLEvaluate$$' -fuzztime=30s ./internal/retrieval/
	$(GO) test -run='^$$' -fuzz='^FuzzPrivacyModeAllowed$$' -fuzztime=30s ./internal/retrieval/
	$(GO) test -run='^$$' -fuzz='^FuzzABTestConfigDecode$$' -fuzztime=30s ./internal/admin/
	$(GO) test -run='^$$' -fuzz='^FuzzConnectorTemplateDecode$$' -fuzztime=30s ./internal/admin/
	$(GO) test -run='^$$' -fuzz='^FuzzNotificationPreferenceDecode$$' -fuzztime=30s ./internal/admin/
	# Round-12 Task 12: four new native fuzz targets.
	$(GO) test -run='^$$' -fuzz='^FuzzParsePartitionKey$$' -fuzztime=30s ./internal/pipeline/
	$(GO) test -run='^$$' -fuzz='^FuzzEffectiveMode$$' -fuzztime=30s ./internal/policy/
	$(GO) test -run='^$$' -fuzz='^FuzzDeltaDiff$$' -fuzztime=30s ./internal/shard/
	$(GO) test -run='^$$' -fuzz='^FuzzQueryHash$$' -fuzztime=30s ./internal/admin/
	# Round-14 Task 11: Round-13 input fuzzers.
	$(GO) test -run='^$$' -fuzz='^FuzzAPIKeyRowDecode$$' -fuzztime=30s ./internal/admin/
	$(GO) test -run='^$$' -fuzz='^FuzzStageBreakerConcurrent$$' -fuzztime=30s ./internal/pipeline/
	$(GO) test -run='^$$' -fuzz='^FuzzHealthSummaryRequest$$' -fuzztime=30s ./internal/admin/
	$(GO) test -run='^$$' -fuzz='^FuzzSlowQueryThreshold$$' -fuzztime=30s ./internal/retrieval/
	# Round-24 Task 10: credential-decode fuzzers for the new connectors.
	$(GO) test -run='^$$' -fuzz='^FuzzQuipCredentialsDecode$$' -fuzztime=30s ./internal/connector/quip/
	$(GO) test -run='^$$' -fuzz='^FuzzFreshserviceCredentialsDecode$$' -fuzztime=30s ./internal/connector/freshservice/
	$(GO) test -run='^$$' -fuzz='^FuzzPagerDutyCredentialsDecode$$' -fuzztime=30s ./internal/connector/pagerduty/
	$(GO) test -run='^$$' -fuzz='^FuzzZohoDeskCredentialsDecode$$' -fuzztime=30s ./internal/connector/zoho_desk/

# Round-12 Task 7: tenant isolation smoke. Runs the e2e isolation
# manifest against the live storage plane.
.PHONY: test-isolation
test-isolation:
	@echo "Running tenant isolation smoke e2e..."
	E2E_ENABLED=1 \
	$(GO) test -tags=e2e -race -count=1 -timeout 5m -run '^TestIsolationSmoke' ./tests/e2e/...

# Round-12 Task 9: migration dry-run gate. Drives migrate.Runner in
# DryRun mode against a fresh SQLite database so SQL syntax errors
# land at PR time, not deploy time.
.PHONY: migrate-dry-run
migrate-dry-run:
	@echo "Running migration dry-run against fresh SQLite database..."
	$(GO) test -count=1 -timeout 2m -run '^TestMigrate_DryRun$$' ./internal/migrate/...

.PHONY: migrate-rollback
migrate-rollback:
	@echo "Rolling back migrations in reverse order..."
	@if [ -z "$$CONTEXT_ENGINE_DATABASE_URL" ]; then \
		echo "CONTEXT_ENGINE_DATABASE_URL must be set"; exit 1; \
	fi
	@for f in $$(ls -r migrations/rollback/*.sql 2>/dev/null); do \
		echo "Applying $$f"; \
		PGPASSWORD="$${CONTEXT_ENGINE_DATABASE_PASSWORD:-hf}" \
			psql "$$CONTEXT_ENGINE_DATABASE_URL" -v ON_ERROR_STOP=1 -f $$f; \
	done

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

# Round-13 Task 16: developer prerequisite checker.
.PHONY: doctor
doctor:
	@echo "==> hunting-fishball doctor"
	@scripts/doctor.sh

# Round-13 Task 20: Postgres-backed migration dry-run.
.PHONY: migrate-dry-run-pg
migrate-dry-run-pg:
	@scripts/migrate-dry-run-pg.sh
