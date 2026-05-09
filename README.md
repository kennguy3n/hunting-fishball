# hunting-fishball

> **Status.** Phase 0 scaffolding is in `main` (🟡 partial — see
> [`docs/PROGRESS.md`](docs/PROGRESS.md) for the live checklist). Phase 1
> connectors and the end-to-end pipeline are still planned. The product
> thesis lives in [`docs/PROPOSAL.md`](docs/PROPOSAL.md) and the target
> system design in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

`hunting-fishball` is a privacy-preserving **knowledge & context platform** that
unifies an organization's documents, chat history, files, and SaaS records into
a single, governed knowledge plane — and exposes that plane to AI agents,
on-device SLMs, and end-user clients across **mobile, desktop, and web**.

The platform is a multi-tenant control plane sitting between:

- **Source systems** — SharePoint, Google Drive, OneDrive, Dropbox, Box, Notion,
  Confluence, Jira, GitHub, Slack, chat history, uploaded files, and 150+ SaaS
  connectors over time.
- **Knowledge consumers** — admin portal, mobile apps, desktop clients, AI
  agents, on-device Small Language Models (SLMs), and third-party integrations.

It is opinionated about three things:

1. **Local-first where possible.** Sensitive content is summarized, indexed,
   and retrieved on-device whenever the device tier supports it. The platform
   never forces remote processing for content the device can handle locally.
2. **Privacy-first by default.** Every retrieval call carries a privacy label
   that surfaces *what data was processed where* in the UI, and the policy
   simulator lets admins evaluate the data-flow impact of any rule change
   before it ships.
3. **Polyglot by necessity, not by accident.** The control plane, context
   engine, and retrieval API are **Go**, matching the existing platform
   backend. Python is retained only as **thin ML microservices** (document
   parsing, embedding computation, GraphRAG entity extraction, persistent
   memory) accessed via gRPC.

---

## Why this exists

Every product surface that wants to "answer over the customer's stuff" — an
inbox digest, a chat copilot, a desktop search bar, a mobile assistant — ends
up rebuilding the same five things:

1. Connectors that ingest from SaaS sources and chat history.
2. A pipeline that fetches, parses, embeds, and stores.
3. A retrieval layer that fans out across vector / lexical / graph / memory.
4. An evaluation harness that knows whether retrieval is actually any good.
5. A privacy + policy framework that tells admins what data is going where.

`hunting-fishball` builds those once, multi-tenant, multi-platform, and exposes
them as **connector-agnostic, client-agnostic** APIs. Skill packs, chat
assistants, and agent runtimes live *on top of it* (see `chat-b2b-skills`,
`chat-b2c-skills`, `slm-rich-media`, `cv-guard`). They do not reinvent
ingestion, retrieval, or governance.

---

## Tech Stack

| Layer | Technology |
|---|---|
| Admin Portal | React (fe-admin) |
| Mobile | iOS (Swift / SwiftUI + UniFFI), Android (Kotlin / Compose + UniFFI) |
| Desktop | Electron + React + N-API bindings |
| Platform Backend | Go (Gin, GORM, PostgreSQL) |
| Context Engine | Go (Kafka consumer, retrieval API, caching, pipeline orchestration) |
| ML Microservices | Python (Docling document parsing, embedding computation, GraphRAG construction) |
| Knowledge Core | Rust (from `kennguy3n/knowledge`) |
| On-device SLM | Bonsai-1.7B via `llama.cpp` |

The platform backend (`ai-agent-platform`) is already Go on Gin, GORM,
PostgreSQL, Redis, and Kafka. The Go context engine reuses that toolchain
end-to-end (deployment model, auth/tenancy patterns, observability stack,
operational runbooks). Python lives only where the ML ecosystem libraries
require it, behind gRPC interfaces — so Python services can be replaced with
Go implementations as Go ML tooling matures, without re-architecting the
control plane.

---

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
│    Kafka consumer (sarama /      │   │    Vector  (Qdrant Go client)    │
│      confluent-kafka-go)         │   │    BM25    (tantivy-go)          │
│    Pipeline coordinator          │   │    Graph   (FalkorDB Go client)  │
│      (goroutines + errgroup)     │   │    Memory  (gRPC → Mem0 svc)     │
│    Stage 1 Fetch (HTTP / S3)     │◀──│    Result merger + reranker      │
│    Stage 4 Storage (Qdrant /     │   │    Semantic cache (Redis)        │
│      FalkorDB / PostgreSQL)      │   └──────────────────────────────────┘
└──────────────────────────────────┘
                       │  gRPC
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Python ML Microservices (stateless workers, gRPC)                      │
│    Docling parsing service     — PDF / DOCX / XLSX / PPTX / HTML        │
│    Embedding service           — local model inference, batching        │
│    GraphRAG entity extractor   — entity / relation extraction           │
│    Mem0 memory service         — persistent user memory                 │
└─────────────────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Storage plane                                                          │
│    PostgreSQL (metadata)  Qdrant (vectors)  FalkorDB (graph)            │
│    Tantivy (BM25)         Redis (cache)     Object storage (artifacts)  │
└─────────────────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  On-device                                                              │
│    Knowledge Core (Rust, UniFFI / N-API)                                │
│    Bonsai-1.7B SLM via llama.cpp (CPU / Metal / CUDA / Vulkan / NPU)    │
│    Local caches, redaction, on-device retrieval shards                  │
└─────────────────────────────────────────────────────────────────────────┘
```

### Sync architecture (high level)

Source systems push change events into Kafka. The Go context engine consumes
those events with a goroutine-per-partition pool, runs a 4-stage pipeline, and
writes the resulting chunks / embeddings / graph nodes into the storage plane.
Stages 1 and 4 are pure Go; stages 2 (parse) and 3 (embed) call the Python ML
microservices via gRPC, or — for embedding — call a remote embedding API
directly when the tenant has opted into a hosted model.

Goroutines and channels replace the previous Python `multiprocessing`
`ProcessCoordinator`, giving the engine lightweight per-document concurrency
without the GIL. `errgroup` is used for structured concurrency inside the
retrieval API's parallel fan-out (vector / lexical / graph / memory).

### Retrieval architecture (high level)

`POST /v1/retrieve` accepts a query plus a tenant / user / channel scope and
fans out **in parallel**:

- **Qdrant** vector search (Go client) — semantic recall.
- **Tantivy** BM25 / hybrid search (`tantivy-go` bindings) — exact / lexical
  recall.
- **FalkorDB** graph traversal (Go client) — entity-anchored multi-hop.
- **Mem0** memory search (gRPC → Python) — user / session memory.

A Go merger + reranker combines the four streams, applies tenant-level policy
(privacy mode, allow / deny lists, per-channel ACLs), and returns a single
ranked list with provenance. Hot queries are cached in **Redis** via a
semantic-cache key (query embedding + scope hash).

For the long-form rationale on language choice and retrieval-layer mapping,
see [`docs/PROPOSAL.md` §3.1](docs/PROPOSAL.md#31-context-engine-language-choice).

---

## Quick start

```bash
# 1. Clone
git clone https://github.com/kennguy3n/hunting-fishball.git
cd hunting-fishball

# 2. Install Go dependencies (Go 1.25+ required)
go mod download

# 3. Bring up the local storage plane (Postgres / Redis / Kafka / Qdrant)
docker compose up -d

# 4. Run the test suite
make test           # = go test -race -cover ./...

# 5. Generate proto stubs (only needed when proto files change)
make proto-gen

# 6. Build the binaries
make build          # produces ./bin/context-engine-ingest and ./bin/context-engine-api
```

Other targets:

| Target          | What it does                                  |
|-----------------|-----------------------------------------------|
| `make build`    | Build both binaries into `./bin/`             |
| `make test`     | `go test -race -cover ./...`                  |
| `make vet`      | `go vet ./...`                                |
| `make fmt`      | `gofmt -s -w` over hand-written sources       |
| `make lint`     | `golangci-lint run` (skipped if not installed)|
| `make proto-gen`| Regenerate `*.pb.go` from `proto/**/*.proto`  |
| `make proto-check` | Verify generated proto files are up to date|
| `make clean`    | Remove `./bin/` and coverage artefacts        |

## Project structure

```
hunting-fishball/
├── cmd/
│   ├── ingest/                # context-engine-ingest binary entry point
│   └── api/                   # context-engine-api    binary entry point
├── internal/
│   ├── connector/             # SourceConnector interface, optional
│   │                          # interfaces, process-global registry
│   ├── credential/            # AES-256-GCM envelope encryption
│   └── audit/                 # audit_logs model + repository + Kafka
│                              # outbox + Gin handler
├── proto/
│   ├── docling/v1/            # Python Docling parsing service
│   ├── embedding/v1/          # Python embedding service
│   └── memory/v1/             # Mem0 persistent memory service
├── migrations/                # SQL migrations (audit_logs, ...)
├── docs/                      # PROPOSAL / ARCHITECTURE / PHASES / PROGRESS
├── docker-compose.yml         # local Postgres / Redis / Kafka / Qdrant
├── Makefile                   # build / test / lint / proto-gen
└── .github/workflows/ci.yml   # CI: vet / test / lint / proto-check
```

The full target architecture (including phases that have not yet
landed) is documented in
[`docs/ARCHITECTURE.md` §9](docs/ARCHITECTURE.md#9-directory-structure).

---

## Documents in this repo

- [`docs/PROPOSAL.md`](docs/PROPOSAL.md) — product vision, capabilities,
  connector catalog, B2C, B2B, lifecycle, policy simulation, privacy,
  cross-platform strategy, device tiering, and the language-choice rationale
  for the Go context engine.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system overview, sync
  architecture, retrieval architecture, storage plane, multi-tenancy, and
  observability.
- [`docs/PHASES.md`](docs/PHASES.md) — phased delivery plan (Phase 0 through
  Phase 8) with exit criteria.
- [`docs/PROGRESS.md`](docs/PROGRESS.md) — checklist of what is built, what is
  in progress, and what is planned, including the Context Engine migration
  tasks.

---

## Repository conventions

- **Branches.** Use `devin/<timestamp>-<short-name>` for AI-generated work,
  `feat/<short-name>` for human-authored work, and `fix/<issue-number>` for
  bug fixes. Direct pushes to `main` are not permitted.
- **PR format.** PRs include a Summary, Plan / Phase reference, and a
  Verification section. Documentation-only PRs may skip Verification.
- **Documentation-as-code.** This repository is documentation-first.
  Architectural changes land here *before* the code change in the relevant
  service repository (`ai-agent-platform`, `ai-agent-context-engine`,
  `ai-agent-desktop`, `knowledge`, etc.).
---

## Related repositories

| Repo | Role |
|---|---|
| `ai-agent-platform` | Go control-plane backend (Gin, GORM, PostgreSQL) — workspace, channels, knowledge nodes |
| `ai-agent-context-engine` | Source of the existing 4-stage pipeline; the Go rewrite lives alongside the Python ML microservices |
| `kennguy3n/knowledge` | Rust knowledge core (UniFFI / N-API) — on-device evidence store, decay, graph |
| `kennguy3n/llama.cpp` | On-device SLM runtime (Bonsai-1.7B GGUF) |
| `kennguy3n/slm-rich-media` | Cross-platform SLM / diffusion / VLM runtime |
| `kennguy3n/chat-b2b-skills` | B2B skill packs that consume the retrieval API |
| `kennguy3n/chat-b2c-skills` | B2C skill packs that consume the retrieval API |
| `kennguy3n/cv-guard` | On-device media safety classifier used in privacy gating |
| `kennguy3n/slm-guardrail` | On-device text safety classifier used in privacy gating |
