"""Round-12 Task 5: gRPC health check tests for the Docling sidecar.

Asserts the standard `grpc.health.v1.Health` servicer is registered
on the gRPC server. We don't exercise the underlying Docling backend
here — the health check must succeed independently of the
domain RPC's readiness, which is the whole point of the protocol.
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

import docling_server as srv  # noqa: E402


@pytest.fixture
def health_channel():
    server, port = srv.serve("[::]:0")
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
        health_pb2.HealthCheckRequest(service="docling.v1.DoclingService")
    )
    assert resp.status == health_pb2.HealthCheckResponse.SERVING


def test_health_check_unknown_service_raises(health_channel):
    stub = health_pb2_grpc.HealthStub(health_channel)
    with pytest.raises(grpc.RpcError) as exc:
        stub.Check(
            health_pb2.HealthCheckRequest(service="never.registered.Service")
        )
    assert exc.value.code() == grpc.StatusCode.NOT_FOUND
