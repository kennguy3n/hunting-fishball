# Python ML microservices

Three thin gRPC servers that wrap heavyweight Python ML libraries and
expose them to the Go context engine over HTTP/2:

| Service     | Path                  | Port  | Wraps                           |
| ----------- | --------------------- | ----- | ------------------------------- |
| `docling`   | `services/docling`    | 50051 | [Docling][docling]              |
| `embedding` | `services/embedding`  | 50052 | [sentence-transformers][st]     |
| `memory`    | `services/memory`     | 50053 | [Mem0][mem0]                    |

[docling]: https://github.com/DS4SD/docling
[st]: https://www.sbert.net
[mem0]: https://github.com/mem0ai/mem0

Each service implements one of the proto contracts under
`proto/<service>/v1/*.proto`. The contracts are stable and
versioned — the Go side only depends on the generated gRPC stubs,
never on the Python code.

## Local quick-start

```bash
docker compose up -d docling embedding memory
```

Each service exposes a `/healthz`-style `Check` via the standard gRPC
health-checking protocol on the same port.

## Running unit tests

Each service has unit tests that use the proto-generated stubs and a
fake (or in-process) backend. They do **not** require docker. From
the repo root:

```bash
cd services/docling   && python -m pytest -q
cd services/embedding && python -m pytest -q
cd services/memory    && python -m pytest -q
```

## Layout

Generated proto stubs live under `services/_proto/` and are vendored
on first run by `gen_protos.sh`. They are re-generated from the
`.proto` files under `proto/`.
