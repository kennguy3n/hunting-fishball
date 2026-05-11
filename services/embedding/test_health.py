"""Round-12 Task 5: gRPC health check tests for the embedding sidecar.

The health probe must succeed without instantiating the
sentence-transformers model — these tests assert exactly that by
asking only the Health servicer and never the EmbeddingService.
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

from grpc_health.v1 import health_pb2, health_pb2_grpc  # noqa: E402

import embedding_server as srv  # noqa: E402


@pytest.fixture
def health_channel():
    server, port = srv.serve("[::]:0", backend=None)
    chan = grpc.insecure_channel(f"localhost:{port}")
    yield chan
    chan.close()
    server.stop(grace=0.1).wait()


def test_health_check_reports_serving_for_overall(health_channel):
    stub = health_pb2_grpc.HealthStub(health_channel)
    resp = stub.Check(health_pb2.HealthCheckRequest(service=""))
    assert resp.status == health_pb2.HealthCheckResponse.SERVING


def test_health_check_reports_serving_for_named_service(health_channel):
    stub = health_pb2_grpc.HealthStub(health_channel)
    resp = stub.Check(
        health_pb2.HealthCheckRequest(service="embedding.v1.EmbeddingService")
    )
    assert resp.status == health_pb2.HealthCheckResponse.SERVING
