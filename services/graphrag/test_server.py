"""Unit tests for the GraphRAG entity-extraction microservice.

Smoke-tests the deterministic regex extractor + the gRPC servicer
end-to-end (in-process server + insecure channel, no docker stack).
"""

from __future__ import annotations

import os
import sys
from concurrent import futures

import grpc
import pytest

# Make the repo root importable so `_proto` resolves.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
if _SERVICES_DIR not in sys.path:
    sys.path.insert(0, _SERVICES_DIR)

from _proto.graphrag.v1 import graphrag_pb2, graphrag_pb2_grpc  # noqa: E402
from services.graphrag.graphrag_server import (  # noqa: E402
    GraphRAGServicer,
    RegexExtractor,
)


def _make_chunks(*pairs: tuple[str, str]) -> list[graphrag_pb2.Chunk]:
    return [graphrag_pb2.Chunk(chunk_id=cid, text=text) for cid, text in pairs]


def test_regex_extractor_extracts_capitalised_entities():
    backend = RegexExtractor()
    nodes, edges = backend.extract(
        "doc-1",
        _make_chunks(
            ("c1", "Alice Smith works at Acme Corp."),
            ("c2", "Bob Jones met Alice Smith at the Acme Corp lobby."),
        ),
    )
    names = sorted(n.name for n in nodes)
    assert "Alice Smith" in names
    assert "Acme Corp" in names
    assert "Bob Jones" in names

    by_id = {n.id: n for n in nodes}
    alice = next(n for n in nodes if n.name == "Alice Smith")
    # Cross-chunk mentions must dedupe.
    assert sorted(alice.mentions) == ["c1", "c2"]

    pairs = {tuple(sorted((e.source_id, e.destination_id))) for e in edges}
    alice_id = next(n.id for n in by_id.values() if n.name == "Alice Smith")
    acme_id = next(n.id for n in by_id.values() if n.name == "Acme Corp")
    assert tuple(sorted((alice_id, acme_id))) in pairs


def test_regex_extractor_filters_stopwords():
    backend = RegexExtractor()
    nodes, _ = backend.extract(
        "doc-1",
        _make_chunks(("c1", "The Acme team shipped the launch.")),
    )
    names = {n.name for n in nodes}
    assert "The" not in names
    assert "Acme" in names


def test_regex_extractor_idempotent():
    backend = RegexExtractor()
    chunks = _make_chunks(("c1", "Alice Smith works at Acme Corp."))
    n1, _ = backend.extract("doc-1", chunks)
    n2, _ = backend.extract("doc-1", chunks)
    assert sorted(n.id for n in n1) == sorted(n.id for n in n2)


@pytest.fixture()
def grpc_channel():
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=2))
    graphrag_pb2_grpc.add_GraphRAGServiceServicer_to_server(
        GraphRAGServicer(), server
    )
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    try:
        yield grpc.insecure_channel(f"127.0.0.1:{port}")
    finally:
        server.stop(0).wait()


def test_servicer_returns_response(grpc_channel):
    stub = graphrag_pb2_grpc.GraphRAGServiceStub(grpc_channel)
    resp = stub.ExtractEntities(
        graphrag_pb2.ExtractRequest(
            tenant_id="tenant-a",
            document_id="doc-1",
            chunks=_make_chunks(("c1", "Alice Smith met Bob Jones.")),
        )
    )
    assert resp.model_id
    assert len(resp.nodes) >= 2
    # Response is well-formed: every edge endpoint is one of the nodes.
    node_ids = {n.id for n in resp.nodes}
    for e in resp.edges:
        assert e.source_id in node_ids
        assert e.destination_id in node_ids


def test_servicer_rejects_missing_tenant(grpc_channel):
    stub = graphrag_pb2_grpc.GraphRAGServiceStub(grpc_channel)
    with pytest.raises(grpc.RpcError) as exc:
        stub.ExtractEntities(
            graphrag_pb2.ExtractRequest(
                tenant_id="",
                document_id="doc-1",
                chunks=_make_chunks(("c1", "anything")),
            )
        )
    assert exc.value.code() == grpc.StatusCode.INVALID_ARGUMENT
