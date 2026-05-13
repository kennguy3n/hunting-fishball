# hunting-fishball

> Privacy-preserving knowledge and context platform: a single Go
> backend that ingests, indexes, retrieves, and governs the
> documents that power B2B copilots, B2C assistants, and
> on-device skill packs.

## What this is

`hunting-fishball` builds the five things every "answer over the
customer's stuff" surface ends up rebuilding — **connector ingest,
a Fetch → Parse → Embed → Store pipeline, parallel retrieval fan-out
(vector + BM25 + graph + memory), a privacy-policy framework, and
an evaluation harness** — once, multi-tenant, multi-platform, and
exposes them as connector-agnostic, client-agnostic APIs.

Skill packs, chat assistants, and agent runtimes (`chat-b2b-skills`,
`chat-b2c-skills`, `slm-rich-media`, `cv-guard`, …) live *on top of
it*. They do not re-invent ingestion, retrieval, or governance.

The platform reuses the existing Go-on-Gin / GORM / PostgreSQL /
Redis / Kafka stack from `ai-agent-platform`. Python lives only
where the ML ecosystem libraries require it, behind gRPC, so each
Python service can be replaced with Go without re-architecting the
control plane.

## Architecture overview

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Clients                                                                │
│    Admin Portal (React)                                                 │
│    Mobile (iOS Swift/SwiftUI + UniFFI / Android Kotlin/Compose + UniFFI)│
│    Desktop (Electron + React + N-API)                                   │
└──────────────────────────┬──────────────────────────────────────────────┘
                           │  REST / WebSocket / SSE
                           ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Platform Backend (Go — Gin, GORM, PostgreSQL)                          │
│    Tenancy, IAM, billing, connector registry, source management,        │
│    knowledge nodes, audit log, control-plane APIs                       │
└──────────────────────┬──────────────────────────┬───────────────────────┘
                       │  Kafka                   │  HTTP (Gin)
                       ▼                          ▼
┌──────────────────────────────────┐   ┌──────────────────────────────────┐
│  Context Engine (Go)             │   │  Retrieval API (Go — Gin)        │
│    Kafka consumer                │   │    Vector  (Qdrant)              │
│    Pipeline coordinator          │   │    BM25    (tantivy-go)          │
│    Stage 1 Fetch                 │   │    Graph   (FalkorDB)            │
│    Stage 4 Storage               │◀──│    Memory  (Mem0 over gRPC)      │
│                                  │   │    Result merger + reranker      │
│                                  │   │    Semantic cache (Redis)        │
└──────────────────────────────────┘   └──────────────────────────────────┘
                       │  gRPC
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Python ML microservices (stateless, gRPC)                              │
│    Docling parsing  •  Embedding service  •  GraphRAG  •  Mem0          │
└─────────────────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Storage plane                                                          │
│    PostgreSQL · Qdrant · FalkorDB · Tantivy · Redis · Object storage    │
└─────────────────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  On-device                                                              │
│    Knowledge Core (Rust, UniFFI / N-API)                                │
│    Bonsai-1.7B SLM via llama.cpp (CPU / Metal / CUDA / Vulkan / NPU)    │
└─────────────────────────────────────────────────────────────────────────┘
```

## Tech stack

| Layer            | Technology |
|------------------|------------|
| Admin Portal     | React |
| Mobile           | iOS (Swift / SwiftUI + UniFFI), Android (Kotlin / Compose + UniFFI) |
| Desktop          | Electron + React + N-API bindings |
| Platform Backend | Go (Gin, GORM, PostgreSQL) |
| Context Engine   | Go (Kafka, retrieval API, caching, pipeline orchestration) |
| ML Microservices | Python (Docling parsing, embedding, GraphRAG) |
| Knowledge Core   | Rust (from `kennguy3n/knowledge`) |
| On-device SLM    | Bonsai-1.7B via `llama.cpp` |

## Quick start

```bash
# 1. Clone
git clone https://github.com/kennguy3n/hunting-fishball.git
cd hunting-fishball

# 2. Install dependencies (Go 1.25+)
go mod download

# 3. Bring up the storage plane (Postgres / Redis / Kafka / Qdrant)
docker compose up -d

# 4. Run the unit suite
make test

# 5. Run the e2e smoke against docker-compose
make test-e2e

# 6. Run the Go ↔ Python integration tests
make test-integration

# 7. Run the per-connector e2e smoke suite
make test-connector-smoke

# 8. Enforce the P95 retrieval-latency budget
make bench-e2e

# 9. Build the binaries
make build   # produces ./bin/context-engine-ingest and ./bin/context-engine-api
```

The API and ingest binaries each expose three operational endpoints:

- `GET /healthz` — liveness probe.
- `GET /readyz`  — readiness (Postgres / Redis / Qdrant for the API;
  Postgres / Redis / Kafka for ingest). Returns 503 if any
  required dependency is down.
- `GET /metrics` — Prometheus scrape endpoint.

Python ML sidecars (docling, embedding) expose `/metrics` on a
separate sidecar HTTP listener (default port 9090, override with
`METRICS_PORT`).

## Make targets

| Target                  | What it does |
|-------------------------|--------------|
| `make test`             | `go test -race -cover ./...` |
| `make test-e2e`         | Bring up docker-compose, run the e2e smoke |
| `make test-integration` | Run the Phase-3 ML-service integration tests |
| `make services-test`    | Python unit tests for `services/{docling,embedding,memory}` |
| `make services-protos`  | Regenerate Python gRPC stubs into `services/_proto/` |
| `make bench`            | Pipeline + retrieval benchmarks in `tests/benchmark/` |
| `make capacity-test`    | Phase-8 capacity harness (tunable via `CAPACITY_DOCS_PER_MIN` / `CAPACITY_DURATION`) |
| `make test-connector-smoke` | Per-connector e2e suite (build tag `e2e`) |
| `make bench-e2e`        | Enforce the retrieval P95 / round-trip P95 budget (build tag `e2e`) |
| `make build`            | Build both binaries into `./bin/` |
| `make vet`              | `go vet ./...` |
| `make fmt`              | `gofmt -s -w` over hand-written sources |
| `make lint`             | `golangci-lint run` |
| `make proto-gen`        | Regenerate `*.pb.go` from `proto/**/*.proto` |
| `make proto-check`      | Verify generated proto files are up to date |
| `make alerts-check`     | Validate `deploy/alerts.yaml` Prometheus rule manifest |
| `make fuzz`             | Run Go fuzz targets across `internal/...` (30s per target) |
| `make migrate-dry-run`  | Run the migration runner with `DryRun=true` against SQLite |
| `make migrate-dry-run-pg` | Replay every up + rollback migration against Postgres 16 |
| `make migrate-rollback` | Apply `migrations/rollback/*.down.sql` (gated on `CONTEXT_ENGINE_DATABASE_URL`) |
| `make doctor`           | Check developer prerequisites |
| `make test-isolation`   | Tenant-isolation e2e smoke (build tag `e2e`) |
| `make eval`             | Golden-corpus eval (fails if Precision@5 < 0.6) |
| `make clean`            | Remove `./bin/` and cached test outputs |

## Project structure

```
hunting-fishball/
├── cmd/
│   ├── api/                   # context-engine-api binary entry point
│   └── ingest/                # context-engine-ingest binary entry point
├── internal/
│   ├── connector/             # SourceConnector interface, optional
│   │                          # interfaces (DeltaSyncer, WebhookReceiver,
│   │                          # Provisioner), process-global registry.
│   │                          # Each subdirectory is one connector
│   │                          # (Google Drive, Slack, SharePoint, S3,
│   │                          # Linear, Asana, Salesforce, Zendesk,
│   │                          # ServiceNow, Quip, Freshservice,
│   │                          # PagerDuty, Zoho Desk, … see
│   │                          # docs/PROGRESS.md §"Connector capability
│   │                          # matrix" for the live list).
│   ├── credential/            # AES-256-GCM envelope encryption
│   ├── audit/                 # audit_logs model + repository + Kafka
│   │                          # outbox + Gin handler
│   ├── admin/                 # Source-management API, per-source
│   │                          # rate limiter, source-health tracker,
│   │                          # forget worker, DLQ analytics, policy
│   │                          # simulator, bulk-source actions,
│   │                          # auto-pause, billing webhook
│   ├── policy/                # Privacy modes, ACLs, recipient policy,
│   │                          # snapshot diff, draft store, promotion
│   │                          # FSM with transactional audit
│   ├── pipeline/              # Fetch → Parse → Embed → Store pipeline,
│   │                          # backfill, partition-key routing
│   ├── retrieval/             # /v1/retrieve, parallel fan-out merger,
│   │                          # reranker, policy filter, privacy strip
│   ├── storage/               # Qdrant + Postgres + Bleve BM25 +
│   │                          # FalkorDB + Redis semantic cache
│   ├── shard/                 # Manifest API, generation worker, delta
│   │                          # sync, cryptographic-forgetting
│   ├── models/                # Model catalog + /v1/models/catalog
│   ├── b2c/                   # B2C client SDK bootstrap
│   ├── observability/         # OpenTelemetry tracing + Prometheus
│   ├── grpcpool/              # gRPC connection pool with circuit breaker
│   └── migrate/               # SQL migration runner (up + rollback)
├── services/                  # Python ML microservices (gRPC)
│   ├── docling/               # PDF / DOCX / XLSX / PPTX / HTML parsing
│   ├── embedding/             # Local model inference + batching
│   └── memory/                # Mem0 persistent user memory
├── proto/                     # gRPC contract (.proto files)
├── migrations/                # SQL migrations + rollbacks
├── tests/                     # e2e, integration, regression, benchmark
└── docs/                      # PROPOSAL / ARCHITECTURE / PHASES /
                               # PROGRESS / CUTOVER / openapi.yaml /
                               # runbooks/
```

## API highlights

The full HTTP contract is in [`docs/openapi.yaml`](docs/openapi.yaml).
Headlines:

- **Retrieval** — `POST /v1/retrieve`, `/retrieve/batch`,
  `/retrieve/explain`, `/retrieve/stream`, `/retrieve/feedback`.
- **Source management** — `/v1/admin/sources` lifecycle (connect /
  pause / resume / re-scope / disconnect), credential rotation,
  bulk actions (`/v1/admin/sources/bulk`,
  `/v1/admin/sources/bulk-reindex`,
  `/v1/admin/sources/bulk-rotate`).
- **Health & analytics** — `/v1/admin/connectors/health`,
  `/v1/admin/analytics/connector-health`,
  `/v1/admin/dlq/analytics`, `/v1/admin/health/summary`.
- **Policy & audit** — `/v1/admin/policy/*` (snapshot, simulate,
  diff, draft, promote), `/v1/admin/audit`, `/v1/admin/audit/integrity`.
- **Operations** — `/healthz`, `/readyz`, `/metrics`.

## Documents in this repo

- [`docs/PROPOSAL.md`](docs/PROPOSAL.md) — product vision, capabilities,
  connector catalog, B2C / B2B / lifecycle / policy / privacy /
  cross-platform / device tiering, language-choice rationale.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system overview,
  sync architecture, retrieval architecture, storage plane,
  multi-tenancy, observability.
- [`docs/PHASES.md`](docs/PHASES.md) — phased delivery plan with exit
  criteria.
- [`docs/PROGRESS.md`](docs/PROGRESS.md) — live checklist + changelog +
  connector capability matrix.
- [`docs/CUTOVER.md`](docs/CUTOVER.md) — Python → Go cutover plan and
  rollback procedure.
- [`docs/openapi.yaml`](docs/openapi.yaml) — OpenAPI 3.0 spec for the
  full public + admin HTTP surface.
- [`docs/runbooks/`](docs/runbooks/) — per-connector and operational
  runbooks (credential rotation, quota incidents, outage detection,
  error codes).

## Repository conventions

- **Branches.** `devin/<timestamp>-<short-name>` for AI-generated work,
  `feat/<short-name>` for human-authored work, `fix/<issue-number>`
  for bug fixes. Direct pushes to `main` are not permitted.
- **PR format.** Summary + Plan / Phase reference + Verification.
- **Documentation-as-code.** Architectural changes land here *before*
  the code change in the relevant service repo.
- **CI lanes.** `.github/workflows/ci.yml` defines three lanes:
  - **Fast lane** — runs on every push, required for merge. Branch
    protection requires the single aggregator `Required CI (fast lane)`
    (`fast-required`). Includes `fast-check`, `fast-test`, `fast-build`,
    `fast-lint`, `fast-eval`, `fast-alerts`, `fast-rollback-parity`,
    `fast-migrate-dry-run`, `fast-proto-check`, `fast-python`. A
    soft-fail `fast-govulncheck` job runs on every PR but does not
    block merge.
  - **Full lane** — runs on push to `main`, PRs labelled `full-ci`,
    the nightly cron, and manual dispatch. Covers Postgres migration
    replay, end-to-end suites, capacity, connector smoke, and
    bench-e2e.
  - **Nightly lane** — `nightly-fuzz` runs `make fuzz` on cron so
    panics surface within 24 hours without blocking PR turnaround.

## Related repositories

| Repo | Role |
|------|------|
| `ai-agent-platform` | Go control-plane backend (Gin, GORM, PostgreSQL) — workspace, channels, knowledge nodes |
| `ai-agent-context-engine` | Source of the existing 4-stage pipeline; Go rewrite lives alongside the Python ML services |
| `kennguy3n/knowledge` | Rust knowledge core (UniFFI / N-API) — on-device evidence store, decay, graph |
| `kennguy3n/llama.cpp` | On-device SLM runtime (Bonsai-1.7B GGUF) |
| `kennguy3n/slm-rich-media` | Cross-platform SLM / diffusion / VLM runtime |
| `kennguy3n/chat-b2b-skills` | B2B skill packs that consume the retrieval API |
| `kennguy3n/chat-b2c-skills` | B2C skill packs that consume the retrieval API |
| `kennguy3n/cv-guard` | On-device media safety classifier used in privacy gating |
| `kennguy3n/slm-guardrail` | On-device text safety classifier used in privacy gating |
