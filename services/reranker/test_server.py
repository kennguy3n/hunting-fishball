"""Unit tests for the cross-encoder reranker gRPC server.

Uses a fake backend so the heavy sentence-transformers / torch deps
don't have to be installed for `make test` / unit-test CI.
"""

from __future__ import annotations

import os
import sys

import grpc
import pytest

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
sys.path.insert(0, _SERVICES_DIR)
sys.path.insert(0, _THIS_DIR)

from _proto.reranker.v1 import reranker_pb2, reranker_pb2_grpc  # noqa: E402

import reranker_server as srv  # noqa: E402


class _FakeBackend:
    def __init__(self, model_id="fake-model", scores=None, exc=None):
        self.model_id = model_id
        self.scores = scores
        self.exc = exc
        self.last_query = None
        self.last_texts = None

    def score(self, query, texts):
        self.last_query = query
        self.last_texts = list(texts)
        if self.exc:
            raise self.exc
        if self.scores is not None:
            return list(self.scores)
        # Default: return scores proportional to text length (a
        # deterministic stand-in for cross-encoder relevance).

        return [float(len(t)) for t in texts]


@pytest.fixture
def channel():
    backend = _FakeBackend()
    server, port = srv.serve("[::]:0", backend=backend)
    chan = grpc.insecure_channel(f"localhost:{port}")
    yield chan, backend
    chan.close()
    server.stop(grace=0.1).wait()


def _candidates(*ids_and_text):
    return [
        reranker_pb2.Candidate(chunk_id=i, text=t, score=0.0)
        for i, t in ids_and_text
    ]


def test_rerank_happy_path(channel):
    chan, backend = channel
    stub = reranker_pb2_grpc.RerankerServiceStub(chan)
    req = reranker_pb2.RerankRequest(
        tenant_id="t1",
        query="rotate credentials",
        candidates=_candidates(("c1", "short"), ("c2", "longer text body")),
    )
    resp = stub.Rerank(req)
    assert resp.model_id == "fake-model"
    assert [s.chunk_id for s in resp.scored] == ["c2", "c1"]
    assert backend.last_query == "rotate credentials"


def test_rerank_empty_candidates(channel):
    chan, _ = channel
    stub = reranker_pb2_grpc.RerankerServiceStub(chan)
    req = reranker_pb2.RerankRequest(
        tenant_id="t1", query="anything", candidates=[]
    )
    resp = stub.Rerank(req)
    assert list(resp.scored) == []
    assert resp.model_id == "fake-model"


def test_rerank_top_k_truncates(channel):
    chan, _ = channel
    stub = reranker_pb2_grpc.RerankerServiceStub(chan)
    req = reranker_pb2.RerankRequest(
        tenant_id="t1",
        query="q",
        candidates=_candidates(("a", "x"), ("b", "xx"), ("c", "xxx")),
        top_k=2,
    )
    resp = stub.Rerank(req)
    assert [s.chunk_id for s in resp.scored] == ["c", "b"]


def test_rerank_rejects_missing_tenant(channel):
    chan, _ = channel
    stub = reranker_pb2_grpc.RerankerServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.Rerank(
            reranker_pb2.RerankRequest(query="q", candidates=_candidates(("a", "x")))
        )
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_rerank_rejects_missing_query(channel):
    chan, _ = channel
    stub = reranker_pb2_grpc.RerankerServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.Rerank(
            reranker_pb2.RerankRequest(
                tenant_id="t1", candidates=_candidates(("a", "x"))
            )
        )
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_rerank_surfaces_internal_error_on_backend_exception():
    backend = _FakeBackend(exc=RuntimeError("cross-encoder OOM"))
    server, port = srv.serve("[::]:0", backend=backend)
    chan = grpc.insecure_channel(f"localhost:{port}")
    stub = reranker_pb2_grpc.RerankerServiceStub(chan)
    try:
        with pytest.raises(grpc.RpcError) as exc:
            stub.Rerank(
                reranker_pb2.RerankRequest(
                    tenant_id="t1",
                    query="q",
                    candidates=_candidates(("a", "x")),
                )
            )
        assert exc.value.code() == grpc.StatusCode.INTERNAL
    finally:
        chan.close()
        server.stop(grace=0.1).wait()
