"""Round-12 Task 5: gRPC health check tests for the memory sidecar.

The probe succeeds without touching Mem0 — the Health servicer
returns SERVING immediately so liveness probes don't depend on the
backing memory store coming online.
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

import memory_server as srv  # noqa: E402


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
        health_pb2.HealthCheckRequest(service="memory.v1.MemoryService")
    )
    assert resp.status == health_pb2.HealthCheckResponse.SERVING
