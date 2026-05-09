"""Unit tests for the Docling gRPC server.

These tests use a fake backend so they don't load the heavy Docling
library at import time — meaning they pass without any ML deps
installed (which is exactly what `make test` and the unit-test job
in CI do).
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

from _proto.docling.v1 import docling_pb2, docling_pb2_grpc  # noqa: E402

import docling_server as srv  # noqa: E402


class _FakeBackend:
    def __init__(self, blocks=None, structure=None, exc=None):
        self.blocks = blocks or [
            docling_pb2.ParsedBlock(
                block_id="b1",
                text="hello world",
                type=docling_pb2.BLOCK_TYPE_PARAGRAPH,
                position=0,
            )
        ]
        self.structure = structure or docling_pb2.DocumentStructure(
            title="t", page_count=1
        )
        self.exc = exc
        self.last_content = None
        self.last_mime = None

    def parse(self, content, mime):
        self.last_content = content
        self.last_mime = mime
        if self.exc:
            raise self.exc

        return self.blocks, self.structure


@pytest.fixture
def channel():
    backend = _FakeBackend()
    server, port = srv.serve("[::]:0", backend=backend)
    chan = grpc.insecure_channel(f"localhost:{port}")
    yield chan, backend
    chan.close()
    server.stop(grace=0.1).wait()


def test_parse_document_happy_path(channel):
    chan, backend = channel
    stub = docling_pb2_grpc.DoclingServiceStub(chan)
    resp = stub.ParseDocument(
        docling_pb2.ParseDocumentRequest(
            tenant_id="t1",
            document_id="d1",
            content=b"%PDF-1.4...",
            content_type="application/pdf",
        )
    )
    assert len(resp.blocks) == 1
    assert resp.blocks[0].text == "hello world"
    assert resp.structure.title == "t"
    assert backend.last_mime == "application/pdf"


def test_parse_document_rejects_missing_tenant(channel):
    chan, _ = channel
    stub = docling_pb2_grpc.DoclingServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.ParseDocument(
            docling_pb2.ParseDocumentRequest(content=b"x", content_type="application/pdf")
        )
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_parse_document_rejects_empty_content(channel):
    chan, _ = channel
    stub = docling_pb2_grpc.DoclingServiceStub(chan)
    with pytest.raises(grpc.RpcError) as exc:
        stub.ParseDocument(
            docling_pb2.ParseDocumentRequest(tenant_id="t1", content=b"", content_type="application/pdf")
        )
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_parse_document_surfaces_internal_error_on_backend_exception():
    backend = _FakeBackend(exc=RuntimeError("docling crashed"))
    server, port = srv.serve("[::]:0", backend=backend)
    chan = grpc.insecure_channel(f"localhost:{port}")
    stub = docling_pb2_grpc.DoclingServiceStub(chan)
    try:
        with pytest.raises(grpc.RpcError) as exc:
            stub.ParseDocument(
                docling_pb2.ParseDocumentRequest(
                    tenant_id="t1", content=b"x", content_type="application/pdf"
                )
            )
        assert exc.value.code() == grpc.StatusCode.INTERNAL
    finally:
        chan.close()
        server.stop(grace=0.1).wait()
