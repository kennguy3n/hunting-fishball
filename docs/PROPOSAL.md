# hunting-fishball — Product Proposal

> **Audience.** Engineering leads, design partners, and reviewers evaluating the
> shape of the platform before code is written.
>
> **Status.** Greenfield. This document is the source of truth for the product
> thesis; [`ARCHITECTURE.md`](ARCHITECTURE.md) details how it is realized;
> [`PHASES.md`](PHASES.md) sequences delivery; [`PROGRESS.md`](PROGRESS.md)
> tracks the actual state.

---

## 1. Vision

`hunting-fishball` is a **knowledge & context platform** that lets every
client surface — admin portal, mobile app, desktop client, AI agent,
on-device SLM — answer questions over the customer's stuff *without each one
rebuilding ingestion, retrieval, and governance from scratch*.

The thesis in one paragraph:

> Every modern productivity surface (chat, mail, documents, dashboards,
> assistants) eventually wants the same retrieval call:
> *"answer this query for this user, in this scope, under this privacy mode,
> and tell me where the answer came from."* `hunting-fishball` builds that
> call once, multi-tenant and multi-platform, and exposes it to clients,
> agents, and on-device runtimes through a stable contract. Skill packs
> (`chat-b2b-skills`, `chat-b2c-skills`), assistant runtimes
> (`ai-agent-desktop`), and rich-media SLM stacks (`slm-rich-media`) consume
> that contract — they do not reinvent it.

What "knowledge & context" means concretely:

- A **connector catalog** that ingests from 150+ SaaS systems, file shares,
  chat history, and uploaded files.
- A **context engine** that turns raw bytes into chunks, embeddings, graph
  nodes, and memory entries.
- A **retrieval API** that fans out across vector, lexical, graph, and memory
  stores in parallel and returns a single ranked, governed result.
- A **policy framework** that lets admins simulate and govern *what data
  flows where*, before they roll changes out.
- A **cross-platform** delivery model — admin web, iOS, Android, desktop —
  where each platform respects the device's actual capability tier rather
  than forcing a one-size-fits-all path.

---

## 2. Product surfaces

### 2.1 Admin portal

A React `fe-admin` SPA used by tenant administrators to:

- Connect sources (OAuth, service accounts, on-prem service principals).
- Map sources → workspaces → channels (the platform-backend hierarchy).
- Define and simulate retrieval / privacy policies.
- Watch sync health, quotas, billing, and per-source provenance.
- Audit retrieval calls (who asked what, where the answer came from, what
  privacy label fired).

The admin portal speaks only to the **Go platform backend**. It never talks to
the context engine directly; everything goes through the control-plane API
surface so the audit log and tenant guard rails are uniform.

### 2.2 B2C clients

End-user clients in the B2C stack (mobile-first, with a desktop companion):

- **iOS** — Swift / SwiftUI with UniFFI bindings into the Rust
  `kennguy3n/knowledge` core.
- **Android** — Kotlin / Compose with the same UniFFI core.
- **Desktop companion** — Electron + React + N-API (for the cases where the
  user wants the same surface on a laptop).

B2C clients lean heavily on the **on-device SLM** (Bonsai-1.7B via
`llama.cpp`) and the on-device knowledge core. They retrieve remotely only
when the device tier or the policy requires it — and when they do, the UI
surfaces a **privacy strip** explaining the data flow.

### 2.3 B2B clients

B2B clients are the chat / collaboration / document surfaces (KChat
B2B-style):

- **Desktop** — Electron + React, signed and sandboxed; the primary B2B
  surface.
- **Mobile** — iOS / Android using the same UniFFI knowledge core, scoped to
  the user's organization.
- **Web** — administrators and read-mostly users access through the admin
  portal and the read-only web embed.

B2B retrieval is governed by **channel policy** (no-AI, local-only,
local-API, hybrid, remote) and by tenant-wide privacy mode. Skills consume
the retrieval API through the **B2B skill SDK** (`chat-b2b-skills`).

### 2.4 Agents and skill runtimes

`hunting-fishball` is a *substrate*, not an agent runtime. Agent and skill
runtimes consume it:

- **Skill packs** — `chat-b2b-skills` and `chat-b2c-skills`. Each skill calls
  `POST /v1/retrieve` with the user's query plus a tenant / channel /
  privacy scope, gets a ranked context bundle, and feeds it to the
  appropriate model (local Bonsai, remote LLM, or hybrid).
- **Agent platform** — the agent runtime and the control-plane backend
  use the same retrieval contract from the agent worker side.
- **On-device runtimes** — `slm-rich-media` and `cv-guard` use the on-device
  knowledge core for media safety and SLM inference grounding.

---

## 3. Architecture & rationale

The detailed system design lives in [`ARCHITECTURE.md`](ARCHITECTURE.md).
This section captures the *rationale* that the architecture document then
makes concrete.

The control plane and context engine are **Go**, matching the existing
platform backend (`ai-agent-platform`). Python is retained only as
**thin ML microservices** — Docling parsing, embedding inference, GraphRAG
entity extraction, Mem0 memory — accessed via gRPC. The on-device knowledge
core is **Rust** (`kennguy3n/knowledge`), exposed to mobile through UniFFI
and to desktop through N-API.

### 3.1 Context Engine language choice

The context engine is the bottleneck in this kind of system: it consumes
Kafka events at organizational throughput, runs a 4-stage pipeline per
document, and must respond to retrieval calls with sub-second latency. We
make it **Go**.

**Why Go for the Context Engine:**

- **Concurrency.** Goroutines provide lightweight concurrency for Kafka
  consumption and parallel retrieval fan-out, avoiding Python's GIL
  limitations and the operational tax of `multiprocessing` worker pools.
  The retrieval fan-out (vector / lexical / graph / memory) maps naturally
  to a goroutine per backend with `errgroup` for structured concurrency.
- **Consistency.** The platform backend (`ai-agent-platform`) is already Go
  with Gin, GORM, PostgreSQL, Redis, and Kafka. A Go context engine shares
  the same toolchain, build pipeline, deployment model, observability stack
  (OpenTelemetry), and operational runbooks. There is no "second control
  plane" for SREs to learn.
- **Performance.** Single-binary deployment with a lower memory footprint
  per worker than Python multiprocessing. Better cache locality for
  high-throughput ingestion, and a more predictable garbage-collector
  profile under load.
- **Type safety.** Go's static typing catches integration errors at compile
  time across the Kafka message schemas, retrieval models, and API
  contracts. With protobuf-defined gRPC interfaces, the boundary between Go
  and Python is also statically checked.
- **Retrieval layer.** The four retrieval backends we depend on all have
  mature Go clients: Qdrant (`github.com/qdrant/go-client`), Tantivy
  (`tantivy-go` bindings), FalkorDB (`falkordb-go`), and Redis
  (`go-redis/redis`). The parallel fan-out pattern maps naturally to
  goroutines + channels with deadline-bound `errgroup`s.

**Why Python ML microservices are retained:**

- **Docling** (document parsing for PDF, DOCX, XLSX, PPTX, HTML) is
  Python-only and the most mature option for our document mix.
- **Mem0** (persistent user memory) ships a Python SDK; the Go side calls it
  through gRPC.
- **GraphRAG** entity extraction and knowledge-graph construction lean on
  Python ML libraries (LangChain, LangGraph, sentence-transformers).
- **Embedding inference** uses Python ML frameworks when models run
  locally; remote embedding APIs (OpenAI, Voyage, etc.) are called directly
  from Go.

These services are **stateless workers** behind gRPC interfaces. They do not
own state, they do not coordinate ingestion, and they can be replaced with
Go implementations as Go ML tooling matures — without re-architecting the
control plane.

**Architecture pattern:**

```
Go Context Engine (orchestrator)
├── Kafka Consumer (goroutines, sarama / confluent-kafka-go)
├── Pipeline Coordinator (goroutines + channels)
│   ├── Stage 1: Fetch     (Go — HTTP / S3 downloads, retry, dedupe)
│   ├── Stage 2: Parse     (gRPC call to Python Docling service)
│   ├── Stage 3: Embed     (gRPC call to Python embedding service,
│   │                       or direct API call to remote embedding endpoint)
│   └── Stage 4: Storage   (Go — Qdrant + FalkorDB + PostgreSQL writes)
├── Retrieval API (Gin HTTP server)
│   ├── Vector Search      (Qdrant Go client)
│   ├── BM25 Search        (tantivy-go)
│   ├── Graph Traversal    (FalkorDB Go client)
│   ├── Memory Search      (gRPC call to Python Mem0 service)
│   └── Result Merger + Reranker
└── Semantic Cache (Redis Go client)
```

Concretely, that means:

- Stage 1 (Fetch) and Stage 4 (Storage) are pure Go and never block on the
  Python tier.
- Stage 2 (Parse) is the only stage that *requires* Python today. It is
  isolated behind a gRPC service contract; replacing it later does not
  change the orchestrator.
- Stage 3 (Embed) can run in either mode — local Python embedding service,
  or direct remote API. The orchestrator picks per-tenant per-source.
- Retrieval fan-out is goroutine-per-backend with `errgroup`. A slow or
  failing backend is bounded by deadline and degrades the result, not the
  whole call.

### 3.2 On-device tier

The on-device tier is **Rust + UniFFI / N-API + llama.cpp**:

- `kennguy3n/knowledge` (Rust) — encrypted evidence store, decay state
  machine, on-device concept graph, on-device retrieval shards.
- `kennguy3n/llama.cpp` (C++) — Bonsai-1.7B inference on CPU, Metal, CUDA,
  Vulkan, or NPU as the device supports.
- `kennguy3n/cv-guard` (Rust + ONNX / CoreML / LiteRT) — on-device media
  safety classifier.
- `kennguy3n/slm-guardrail` (Python build, ONNX runtime ship) — on-device
  text safety classifier.

The on-device tier is the *first* place a query lands when the user is on a
device that can serve it locally. The retrieval API is contacted only when
the device cannot, or when policy requires the remote shards.

---

## 4. Connector catalog

The platform ships with a connector catalog covering the source systems
customers actually have. The catalog is pluggable: each connector implements
the same `SourceConnector` Go interface (defined in `ai-agent-platform`),
registers itself in a global registry, and exposes a fixed lifecycle
(`Validate`, `Connect`, `ListNamespaces`, `ListDocuments`, `FetchDocument`,
`Subscribe`, `Disconnect`).

Phase-1 catalog (≥ 12 connectors at GA):

| Group | Connectors |
|---|---|
| File storage | SharePoint Online, Google Drive, OneDrive, Dropbox, Box |
| Knowledge / wiki | Notion, Confluence Cloud |
| Issue / project | Jira Cloud, GitHub, GitLab |
| Chat | Slack, KChat (internal) |

Phase-2+ targets (additive, plug-in only):

| Group | Connectors |
|---|---|
| File storage | SharePoint on-prem, Google Shared Drives, S3 / Azure Blob / GCS, Egnyte |
| Knowledge / wiki | Confluence Server / Data Center, Coda, Bookstack |
| Issue / project | Linear, Asana, ClickUp, Monday |
| Chat | Microsoft Teams, Discord, Mattermost |
| CRM | Salesforce, HubSpot, Pipedrive |
| HR | Workday, BambooHR, Personio |
| Identity | Okta, Entra ID, Google Workspace |
| Email | Microsoft 365 mailboxes, Gmail (read-only knowledge ingestion) |
| Generic | RSS, sitemap crawl, S3-compatible buckets, signed-upload portal |

Every connector is the same shape:

1. **Authentication.** OAuth 2.0 with PKCE for delegated, service-account /
   credentials for app-installed, mTLS for on-prem bridges. Credentials are
   AES-GCM encrypted at the platform-backend level (envelope encryption).
2. **Source mapping.** A connector emits *source documents* into a tenant's
   logical namespace; the admin portal maps that namespace into one or more
   workspaces / channels.
3. **Sync model.** Connectors prefer **delta tokens** (Microsoft Graph
   delta, Drive `changes.list`, Slack `cursor`); fall back to **scheduled
   re-crawl** when the source does not support deltas. Webhook ingestion is
   used where available (Slack Events API, Notion webhooks).
4. **Egress.** Connectors emit `SourceDocument` events into Kafka. They do
   *not* embed, parse, or retrieve. That is the context engine's job.
5. **Quotas.** Per-tenant + per-source rate limits and concurrency budgets,
   enforced by the platform backend before the connector ever talks to the
   external API.

The connector contract is identical to the existing
`ai-agent-platform` connector contract, so connectors can be moved
between repositories without code changes.

---

## 5. Lifecycle management

Lifecycle = how a piece of knowledge enters the platform, how it stays in
sync with its source, and how it leaves.

1. **Onboard.** Admin connects a source. The platform validates the
   credential, lists namespaces (drives, repos, channels, projects), and
   stages a sync plan.
2. **Initial sync.** The connector emits all in-scope documents into Kafka
   over a paced backfill. The Go context engine processes them through the
   4-stage pipeline. Progress is reported per namespace.
3. **Steady-state sync.** Delta or webhook updates flow into the same Kafka
   topic. The pipeline coordinator deduplicates, debounces (rapid edits
   coalesce into a single ingestion task), and reprocesses only changed
   chunks where possible.
4. **Mutation.** Admins can pause, restart, scope-restrict, or relabel a
   connection at any time. Changes apply at the next steady-state poll;
   in-flight messages drain.
5. **Retention.** Documents inherit the source's retention rules by default;
   tenants can layer additional retention policy on top (e.g., "drop chat
   messages older than 90 days even if Slack keeps them").
6. **Forget.** When a connection is removed, all derived chunks /
   embeddings / graph nodes / memory entries are deleted asynchronously.
   The deletion job is fenced (lease-based locking) so a re-add does not
   race with the forget worker.

The lifecycle is observable. Every transition emits a structured event
(`source.connected`, `source.sync_started`, `chunk.indexed`,
`source.purged`) into the platform audit log, which is exposed in the admin
portal and accessible by API.

---

## 6. Policy simulation

Admins do not write rule DSLs. The policy framework gives them three
controls:

- **Privacy mode** (per tenant, per channel): `local-only`, `local-api`,
  `hybrid`, `remote`, `no-ai`. Every retrieval call respects the strictest
  scope in the chain.
- **Allow / deny lists** (per source, per namespace, per path glob): which
  documents may participate in retrieval and at which compute tier.
- **Recipient policy** (per channel, per skill): which downstream skills /
  agents are allowed to receive matches.

The **policy simulator** lets admins evaluate proposed changes *before* they
go live:

- **What-if retrieval.** *"Under this draft policy, what would happen if
  user X queried `quarterly revenue` in channel Y?"* The simulator runs
  retrieval against the proposed policy and shows the diff vs. live: which
  matches are added, which are dropped, which compute tier each match would
  use.
- **Data-flow diff.** For a proposed change, the simulator shows the change
  in *where data goes* — e.g., "this change would route 12 % more matches
  through the remote embedding API."
- **Conflict detection.** Surfaces overlapping or contradictory rules
  before promotion (e.g., a tenant-wide `local-only` mode in conflict with
  a per-channel `hybrid` override).
- **Promotion.** A draft is never live. Admins promote it explicitly; the
  promotion is an audit-logged event.

The simulator runs entirely in the Go context engine — it does not require
the Python ML tier. It uses cached embeddings and a copy-on-write policy
view to avoid touching live state.

---

## 7. Privacy

Privacy in `hunting-fishball` is not a wrapper on the retrieval call; it is
the retrieval call. Every result row carries a `privacy_label` describing
*where* the data was processed, *what* tier of model touched it, and *why*
the platform was allowed to surface it.

The privacy contract:

1. **Local-first when feasible.** If the device tier supports local
   retrieval and the channel allows it, the platform answers locally. The
   user sees a privacy strip indicating "answered on this device".
2. **Tier escalation is explicit.** Going from local → local-API → hybrid →
   remote is a visible, policy-bounded escalation. The UI shows the chain.
3. **Redaction at egress.** When data must leave the device or the tenant
   boundary, redaction (PII tokenization, attachment hashing, metadata
   stripping) happens *before* egress. Redaction is reversible only inside
   the tenant boundary.
4. **Provenance everywhere.** Every chunk that participates in a retrieval
   carries the source document URI, the connector that produced it, the
   tenant scope, and the encryption mode it was stored under. The audit log
   reconstructs the full chain on demand.
5. **Cryptographic forgetting.** Deletion is *cryptographic* where possible
   — keys are destroyed alongside the index entries, so subsequent recovery
   from snapshots is infeasible.
6. **No silent fallback.** A retrieval call never silently degrades from
   `hybrid` to `remote`. If the policy forbids remote and the local tier
   cannot answer, the call returns an empty result with a `policy_blocked`
   reason — never a remote answer.

Privacy is also enforced at the **storage plane**:

- **Per-tenant logical isolation** in PostgreSQL (Row-Level Security on
  tenant_id, enforced by GORM scopes).
- **Per-tenant Qdrant collections** for vector storage; cross-tenant query
  is structurally impossible at the API layer.
- **Per-tenant FalkorDB graphs** keyed on tenant id.
- **Per-tenant Tantivy indices**; index files are mounted only when the
  tenant scope is active.
- **Per-tenant Redis namespaces** for cache and rate limit; key prefix
  enforced by the cache wrapper.

---

## 8. Cross-platform strategy

The platform supports **admin web, B2B desktop, B2B mobile, B2C mobile,
B2C desktop**. Each platform has different constraints; the strategy is to
share *core* (Rust knowledge, Bonsai SLM, retrieval contract) and to adapt
the *shell*:

- **Admin Portal (Web).** React `fe-admin` SPA. Speaks REST to the Go
  platform backend. No on-device retrieval; admin work is always
  remote-mediated.
- **Desktop (B2B + B2C companion).** Electron + React + N-API into the
  Rust knowledge core and `llama.cpp`. The same renderer is reused
  per-segment with different configuration: B2B uses corporate workspaces
  and channel policy; B2C uses personal workspaces and on-device-first.
- **iOS (B2B + B2C).** Swift + SwiftUI. The Rust knowledge core ships as
  an XCFramework via UniFFI; `llama.cpp` ships as a Metal-accelerated
  static library.
- **Android (B2B + B2C).** Kotlin + Compose. The Rust core ships as an
  AAR via UniFFI; `llama.cpp` ships with a JNI bridge and per-ABI native
  libraries.

What is shared across all platforms:

- **Retrieval contract.** `POST /v1/retrieve` returns the same JSON shape
  with the same privacy / provenance fields on every platform.
- **Privacy strip.** Every client implements the same UI element listing
  *where the answer came from*.
- **Knowledge core.** All non-trivial on-device work goes through the Rust
  core; the platform-specific code is glue.
- **Skill SDK.** Both B2B and B2C clients consume the same retrieval API
  and the same skill manifest format.

What is *not* shared:

- **UI shell.** Each platform owns its native shell (admin portal SPA, B2B
  desktop window manager, mobile feature flags).
- **Background sync.** Each platform uses its native scheduler (BGAppRefresh
  on iOS, WorkManager on Android, system timers on desktop, server cron in
  the cloud).
- **Distribution.** Each platform uses its native distribution channel
  (App Store, Play Store, signed installers, direct web).

---

## 9. Device tiering

Devices are classified into tiers based on RAM, accelerator, and thermal
budget. The tier is computed at startup, re-evaluated under thermal /
battery pressure, and consumed by the on-device runtime to decide *which
models to run, when, and for how long*.

| Tier | RAM | Accelerator | Behavior |
|---|---|---|---|
| **Low** | < 4 GB | CPU only | No on-device SLM. Retrieval calls are remote. UI surfaces an explicit "remote retrieval" privacy strip. |
| **Mid** | 4–8 GB | NPU / GPU optional | Bonsai-1.7B int4 SLM. Local retrieval on small shards; remote retrieval for everything else. |
| **High** | ≥ 8 GB | NPU / GPU available | Bonsai-1.7B fp16 / int8 SLM. Local retrieval on most shards. Diffusion / VLM gated behind explicit user opt-in. |

The tiering policy is enforced by the Rust knowledge core, not by the UI:

- **Eligibility.** A model is *eligible* on a tier only if the tier passes
  its RAM / accelerator floor and the OS reports a healthy thermal state.
- **Effective tier.** Under thermal or battery pressure, the runtime drops
  to the next tier down, evicts the heaviest models, and re-runs only the
  workload the user is actively waiting for.
- **Co-residency.** Heavy models (Bonsai fp16 + diffusion, for example)
  cannot be co-resident on Mid tier; the eviction ladder is deterministic
  and observable.

Tiering rules are read from a **signed model catalog** (the same catalog
shape used by `slm-rich-media`), so platform updates can ship new tier
floors without a client release.

---

## 10. Roadmap reference

The phased delivery plan lives in [`PHASES.md`](PHASES.md). The current
shape is:

| Phase | Theme |
|---|---|
| Phase 0 | Connector contract, registry, audit-log primitives |
| Phase 1 | Single-source MVP (Google Drive + Slack) end-to-end |
| Phase 2 | B2B Admin Source Management — wire the org-wide sync pipeline through the **Go context engine** (Kafka) |
| Phase 3 | Retrieval fan-out (Qdrant + Tantivy + FalkorDB + Mem0) |
| Phase 4 | Policy framework + simulator + privacy strip |
| Phase 5 | On-device knowledge core integration (mobile + desktop) |
| Phase 6 | B2C client surfaces (mobile + desktop companion) |
| Phase 7 | Catalog expansion (additional connectors per the connector catalog) |
| Phase 8 | Cross-platform optimization — Go context engine performance tuning, Python ML microservice scaling |

The Context Engine migration tasks (proto definitions, Go workers, Python
gRPC microservice wrappers, integration tests, throughput benchmarks) are
tracked in [`PROGRESS.md`](PROGRESS.md).
