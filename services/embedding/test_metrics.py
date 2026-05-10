"""Phase 8 — verify the embedding sidecar exposes Prometheus metrics."""

from __future__ import annotations

import socket
import time
import urllib.request

import grpc

import embedding_server
from _proto.embedding.v1 import embedding_pb2, embedding_pb2_grpc


class _StubBackend:
    """Returns deterministic 4-d vectors regardless of input text."""

    model_id = "test-stub"

    def embed(self, chunks):
        vectors = [[0.1, 0.2, 0.3, 0.4] for _ in chunks]
        return vectors, 4


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_metrics_endpoint_exposes_collectors_and_records_request():
    metrics_port = _free_port()
    embedding_server.start_metrics_server(metrics_port)

    server, grpc_port = embedding_server.serve("[::]:0", backend=_StubBackend())
    try:
        chan = grpc.insecure_channel(f"127.0.0.1:{grpc_port}")
        stub = embedding_pb2_grpc.EmbeddingServiceStub(chan)
        resp = stub.ComputeEmbeddings(
            embedding_pb2.ComputeEmbeddingsRequest(
                tenant_id="t1", chunks=["hello", "world"]
            )
        )
        assert len(resp.embeddings) == 2
        assert resp.dimensions == 4

        body = urllib.request.urlopen(
            f"http://127.0.0.1:{metrics_port}/metrics", timeout=5
        ).read().decode("utf-8")

        assert "embedding_requests_total" in body
        assert "embedding_duration_seconds" in body
        assert "embedding_queue_depth" in body
        assert 'embedding_requests_total{status="ok"} 1.0' in body
    finally:
        server.stop(0)
        time.sleep(0.05)
