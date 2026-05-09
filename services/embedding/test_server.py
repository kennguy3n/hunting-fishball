"""Unit tests for the embedding gRPC server.

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

from _proto.embedding.v1 import embedding_pb2, embedding_pb2_grpc  # noqa: E402

import embedding_server as srv  # noqa: E402


class _FakeBackend:
    def __init__(self, model_id="fake-model", dim=4, exc=None):
        self.model_id = model_id
        self.dim = dim
        self.exc = exc
        self.last = None

    def embed(self, chunks):
        self.last = list(chunks)
        if self.exc:
            raise self.exc

        return [[float(i) + j for j in range(self.dim)] for i in range(len(chunks))], self.dim


@pytest.fixture
def channel():
    backend = _FakeBackend()
    server, port = srv.serve("[::]:0", backend=backend)
    chan = grpc.insecure_channel(f"localhost:{port}")
    yield chan, backend
    chan.close()
    server.stop(grace=0.1).wait()


def test_embed_happy_path(channel):
    chan, backend = channel
    stub = embedding_pb2_grpc.EmbeddingServiceStub(chan)
    resp = stub.ComputeEmbeddings(
        embedding_pb2.ComputeEmbeddingsRequest(tenant_id="t1", chunks=["hello", "world"])
    )
    assert len(resp.embeddings) == 2
    assert resp.dimensions == 4
    assert resp.model_id == "fake-model"
    assert backend.last == ["hello", "world"]
    assert list(resp.embeddings[0].values) == [0.0, 1.0, 2.0, 3.0]


def test_embed_empty_returns_zero_dim(channel):
    chan, _ = channel
    stub = embedding_pb2_grpc.EmbeddingServiceStub(chan)
    resp = stub.ComputeEmbeddings(
        embedding_pb2.ComputeEmbeddingsRequest(tenant_id="t1", chunks=[])
    )
    assert len(resp.embeddings) == 0
    assert resp.dimensions == 0


def test_embed_rejects_missing_tenant(channel):
    chan, _ = channel
    stub = embedding_pb2_grpc.EmbeddingServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.ComputeEmbeddings(embedding_pb2.ComputeEmbeddingsRequest(chunks=["x"]))
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_embed_surfaces_internal_error_on_backend_exception():
    backend = _FakeBackend(exc=RuntimeError("model OOM"))
    server, port = srv.serve("[::]:0", backend=backend)
    chan = grpc.insecure_channel(f"localhost:{port}")
    stub = embedding_pb2_grpc.EmbeddingServiceStub(chan)
    try:
        with pytest.raises(grpc.RpcError) as exc:
            stub.ComputeEmbeddings(
                embedding_pb2.ComputeEmbeddingsRequest(tenant_id="t1", chunks=["x"])
            )
        assert exc.value.code() == grpc.StatusCode.INTERNAL
    finally:
        chan.close()
        server.stop(grace=0.1).wait()


def test_embed_batches_consistently(channel):
    chan, _ = channel
    stub = embedding_pb2_grpc.EmbeddingServiceStub(chan)
    resp = stub.ComputeEmbeddings(
        embedding_pb2.ComputeEmbeddingsRequest(
            tenant_id="t1", chunks=[f"chunk-{i}" for i in range(8)]
        )
    )
    assert len(resp.embeddings) == 8
    for emb in resp.embeddings:
        assert len(emb.values) == resp.dimensions
