"""Phase 8 — verify the docling sidecar exposes Prometheus metrics."""

from __future__ import annotations

import socket
import time
import urllib.request

import grpc

import docling_server
from _proto.docling.v1 import docling_pb2, docling_pb2_grpc


class _StubBackend:
    """Returns one parsed block deterministically."""

    def parse(self, content: bytes, content_type: str):
        block = docling_pb2.ParsedBlock(
            block_id="b-0",
            text=content.decode("utf-8", errors="ignore"),
            type=docling_pb2.BLOCK_TYPE_PARAGRAPH,
            heading_level=0,
            page_number=1,
            position=0,
        )
        structure = docling_pb2.DocumentStructure(title="t", page_count=1, outline_md="")
        return [block], structure


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_metrics_endpoint_exposes_collectors_and_records_request():
    metrics_port = _free_port()
    docling_server.start_metrics_server(metrics_port)

    server, grpc_port = docling_server.serve("[::]:0", backend=_StubBackend())
    try:
        chan = grpc.insecure_channel(f"127.0.0.1:{grpc_port}")
        stub = docling_pb2_grpc.DoclingServiceStub(chan)
        resp = stub.ParseDocument(
            docling_pb2.ParseDocumentRequest(
                tenant_id="t1", content=b"hello", content_type="text/plain"
            )
        )
        assert len(resp.blocks) == 1

        # /metrics scrape — Prometheus client is sync, so the sample is
        # available immediately after the call returns.
        body = urllib.request.urlopen(
            f"http://127.0.0.1:{metrics_port}/metrics", timeout=5
        ).read().decode("utf-8")

        assert "docling_parse_requests_total" in body
        assert "docling_parse_duration_seconds" in body
        assert "docling_parse_queue_depth" in body
        # And we should see at least one ok request.
        assert 'docling_parse_requests_total{status="ok"} 1.0' in body
    finally:
        server.stop(0)
        # Give the metrics http server a moment to release the port.
        time.sleep(0.05)
