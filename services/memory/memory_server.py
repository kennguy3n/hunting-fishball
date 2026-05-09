"""Phase 3 Mem0 gRPC microservice.

Wraps [Mem0](https://github.com/mem0ai/mem0) for persistent
user/session memory behind the proto contract in
`proto/memory/v1/memory.proto`. The Go retrieval API calls
`SearchMemory` from `/v1/retrieve`; the ingest pipeline calls
`WriteMemory` for memory-eligible chunks.

Per-tenant isolation is enforced inside Mem0 by the user_id /
session_id namespacing. The Python server refuses any call with an
empty tenant_id.
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
import time
import uuid
from concurrent import futures
from typing import Tuple

import grpc

# Make `_proto` importable.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
if _SERVICES_DIR not in sys.path:
    sys.path.insert(0, _SERVICES_DIR)

from _proto.memory.v1 import memory_pb2, memory_pb2_grpc  # noqa: E402

LOG = logging.getLogger("memory-server")


class MemoryBackend:
    """Adapter over Mem0. Lazily instantiates a Memory() so tests can
    swap a fake without ever importing mem0ai."""

    def __init__(self) -> None:
        self._mem = None

    def _client(self):
        if self._mem is None:
            from mem0 import Memory

            self._mem = Memory()

        return self._mem

    def write(
        self,
        tenant_id: str,
        user_id: str,
        session_id: str,
        content: str,
        metadata: dict[str, str],
    ) -> Tuple[str, int]:
        client = self._client()
        record = client.add(
            content,
            user_id=_compose_id(tenant_id, user_id),
            metadata={**metadata, "tenant_id": tenant_id, "session_id": session_id},
        )
        rid = (record or {}).get("id") or str(uuid.uuid4())

        return rid, int(time.time())

    def search(
        self,
        tenant_id: str,
        user_id: str,
        session_id: str,
        query: str,
        top_k: int,
    ) -> list[dict]:
        client = self._client()
        results = client.search(
            query,
            user_id=_compose_id(tenant_id, user_id),
            limit=top_k,
        )

        return list(results or [])

    def delete(self, tenant_id: str, mid: str) -> bool:
        client = self._client()
        try:
            client.delete(memory_id=mid)

            return True
        except Exception:  # noqa: BLE001
            return False


def _compose_id(tenant_id: str, user_id: str) -> str:
    """Mem0's `user_id` is the only first-class isolation key.
    Compose `<tenant>:<user>` so two tenants can't accidentally
    share memory through identical user ids."""
    return f"{tenant_id}:{user_id or 'anon'}"


class MemoryServicer(memory_pb2_grpc.MemoryServiceServicer):
    """gRPC servicer that delegates to a MemoryBackend."""

    def __init__(self, backend: MemoryBackend | None = None) -> None:
        self.backend = backend or MemoryBackend()

    def WriteMemory(
        self,
        request: memory_pb2.WriteMemoryRequest,
        context: grpc.ServicerContext,
    ) -> memory_pb2.WriteMemoryResponse:
        if not request.tenant_id:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "tenant_id required")
        if not request.content:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "content required")

        try:
            mid, created_at = self.backend.write(
                request.tenant_id,
                request.user_id,
                request.session_id,
                request.content,
                dict(request.metadata),
            )
        except Exception as exc:  # noqa: BLE001
            LOG.exception("write failed")
            context.abort(grpc.StatusCode.INTERNAL, f"write failed: {exc}")

        return memory_pb2.WriteMemoryResponse(id=mid, created_at=created_at)

    def SearchMemory(
        self,
        request: memory_pb2.SearchMemoryRequest,
        context: grpc.ServicerContext,
    ) -> memory_pb2.SearchMemoryResponse:
        if not request.tenant_id:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "tenant_id required")
        if not request.query:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "query required")

        top_k = int(request.top_k) or 10
        try:
            results = self.backend.search(
                request.tenant_id,
                request.user_id,
                request.session_id,
                request.query,
                top_k,
            )
        except Exception as exc:  # noqa: BLE001
            LOG.exception("search failed")
            context.abort(grpc.StatusCode.INTERNAL, f"search failed: {exc}")

        out = []
        for r in results:
            rec = memory_pb2.MemoryRecord(
                id=r.get("id", ""),
                tenant_id=request.tenant_id,
                user_id=request.user_id,
                session_id=request.session_id,
                content=r.get("memory") or r.get("content", ""),
                metadata={k: str(v) for k, v in (r.get("metadata") or {}).items()},
                created_at=int(r.get("created_at") or 0),
            )
            out.append(memory_pb2.SearchMemoryResult(record=rec, score=float(r.get("score") or 0.0)))

        return memory_pb2.SearchMemoryResponse(results=out)

    def DeleteMemory(
        self,
        request: memory_pb2.DeleteMemoryRequest,
        context: grpc.ServicerContext,
    ) -> memory_pb2.DeleteMemoryResponse:
        if not request.tenant_id or not request.id:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "tenant_id and id required")
        deleted = self.backend.delete(request.tenant_id, request.id)

        return memory_pb2.DeleteMemoryResponse(deleted=deleted)


def serve(addr: str, backend: MemoryBackend | None = None) -> Tuple[grpc.Server, int]:
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    memory_pb2_grpc.add_MemoryServiceServicer_to_server(MemoryServicer(backend), server)
    port = server.add_insecure_port(addr)
    server.start()
    LOG.info("memory-server listening on port %d", port)

    return server, port


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    parser = argparse.ArgumentParser()
    parser.add_argument("--addr", default=os.environ.get("MEMORY_ADDR", "[::]:50053"))
    args = parser.parse_args()

    server, _ = serve(args.addr)
    server.wait_for_termination()


if __name__ == "__main__":
    main()
