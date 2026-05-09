"""Unit tests for the Mem0 gRPC server.

Uses a fake backend that mimics Mem0's surface so the tests don't
need mem0ai or a vector DB installed.
"""

from __future__ import annotations

import os
import sys
import time

import grpc
import pytest

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
sys.path.insert(0, _SERVICES_DIR)
sys.path.insert(0, _THIS_DIR)

from _proto.memory.v1 import memory_pb2, memory_pb2_grpc  # noqa: E402

import memory_server as srv  # noqa: E402


class _FakeBackend:
    def __init__(self):
        # tenant -> id -> record
        self.store: dict[str, dict[str, dict]] = {}
        self.last_search = None

    def write(self, tenant_id, user_id, session_id, content, metadata):
        rid = f"mem-{len(self.store.get(tenant_id, {}))}"
        self.store.setdefault(tenant_id, {})[rid] = {
            "id": rid,
            "memory": content,
            "metadata": {**metadata, "user_id": user_id, "session_id": session_id},
            "created_at": int(time.time()),
            "score": 1.0,
        }
        return rid, int(time.time())

    def search(self, tenant_id, user_id, session_id, query, top_k):
        self.last_search = (tenant_id, user_id, session_id, query, top_k)
        bucket = self.store.get(tenant_id, {})
        # Naive substring match.
        results = [v for v in bucket.values() if query.lower() in v["memory"].lower()]

        return results[:top_k]

    def delete(self, tenant_id, mid):
        bucket = self.store.get(tenant_id, {})
        if mid in bucket:
            del bucket[mid]
            return True
        return False


@pytest.fixture
def channel():
    backend = _FakeBackend()
    server, port = srv.serve("[::]:0", backend=backend)
    chan = grpc.insecure_channel(f"localhost:{port}")
    yield chan, backend
    chan.close()
    server.stop(grace=0.1).wait()


def test_write_then_search(channel):
    chan, backend = channel
    stub = memory_pb2_grpc.MemoryServiceStub(chan)
    write_resp = stub.WriteMemory(
        memory_pb2.WriteMemoryRequest(
            tenant_id="t1",
            user_id="u1",
            session_id="s1",
            content="user prefers dark mode",
        )
    )
    assert write_resp.id
    assert write_resp.created_at > 0

    search_resp = stub.SearchMemory(
        memory_pb2.SearchMemoryRequest(
            tenant_id="t1", user_id="u1", session_id="s1", query="dark", top_k=5
        )
    )
    assert len(search_resp.results) == 1
    rec = search_resp.results[0].record
    assert rec.content == "user prefers dark mode"
    assert rec.tenant_id == "t1"
    assert search_resp.results[0].score > 0


def test_search_isolates_tenants(channel):
    chan, _ = channel
    stub = memory_pb2_grpc.MemoryServiceStub(chan)
    stub.WriteMemory(
        memory_pb2.WriteMemoryRequest(
            tenant_id="t1", user_id="u1", session_id="s1", content="tenant-1 secret"
        )
    )
    resp = stub.SearchMemory(
        memory_pb2.SearchMemoryRequest(
            tenant_id="t2", user_id="u1", session_id="s1", query="secret", top_k=5
        )
    )
    assert len(resp.results) == 0


def test_delete_memory(channel):
    chan, _ = channel
    stub = memory_pb2_grpc.MemoryServiceStub(chan)
    write_resp = stub.WriteMemory(
        memory_pb2.WriteMemoryRequest(
            tenant_id="t1", user_id="u1", session_id="s1", content="forget this"
        )
    )
    delete_resp = stub.DeleteMemory(
        memory_pb2.DeleteMemoryRequest(tenant_id="t1", id=write_resp.id)
    )
    assert delete_resp.deleted is True


def test_write_rejects_missing_tenant(channel):
    chan, _ = channel
    stub = memory_pb2_grpc.MemoryServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.WriteMemory(memory_pb2.WriteMemoryRequest(content="x"))
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_search_rejects_missing_tenant(channel):
    chan, _ = channel
    stub = memory_pb2_grpc.MemoryServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.SearchMemory(memory_pb2.SearchMemoryRequest(query="x"))
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_search_rejects_missing_query(channel):
    chan, _ = channel
    stub = memory_pb2_grpc.MemoryServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.SearchMemory(memory_pb2.SearchMemoryRequest(tenant_id="t1"))
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT
