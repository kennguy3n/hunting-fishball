"""Unit tests for the deterministic GraphRAG entity extractor —
Round-9 Task 11.

``test_server.py`` already spins up the full gRPC servicer in-process
and exercises the wire contract. Those tests are integration-style:
they require a working gRPC stack and start a server. The Round-9
spec calls for **unit** tests targeting the extractor itself so the
deterministic logic (entity normalisation, edge generation, empty
input, error handling) can be regression-tested cheaply.

This file therefore drills directly into ``RegexExtractor`` and the
servicer's input validation without booting a gRPC server.
"""

from __future__ import annotations

import os
import sys

import pytest

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
if _SERVICES_DIR not in sys.path:
    sys.path.insert(0, _SERVICES_DIR)

from _proto.graphrag.v1 import graphrag_pb2  # noqa: E402
from services.graphrag.graphrag_server import (  # noqa: E402
    GraphRAGServicer,
    RegexExtractor,
)


def _chunk(chunk_id: str, text: str) -> graphrag_pb2.Chunk:
    return graphrag_pb2.Chunk(chunk_id=chunk_id, text=text)


class TestRegexExtractor:
    """Unit tests for the deterministic entity extractor."""

    def test_extracts_capitalised_entities_from_sample_text(self) -> None:
        ex = RegexExtractor()
        nodes, _ = ex.extract(
            "doc-1",
            [_chunk("c-1", "Alice Johnson met Bob Smith at Acme Corp.")],
        )
        names = {n.name for n in nodes}
        assert "Alice Johnson" in names
        assert "Bob Smith" in names
        assert "Acme Corp" in names

    def test_emits_mentioned_with_edges_for_cooccurring_entities(self) -> None:
        ex = RegexExtractor()
        nodes, edges = ex.extract(
            "doc-1",
            [_chunk("c-1", "Alice met Bob.")],
        )
        # 2 entities → exactly 1 MENTIONED_WITH edge
        assert len(nodes) == 2
        assert len(edges) == 1
        assert edges[0].relation == "MENTIONED_WITH"
        # Edge endpoints reference the node ids (sorted-pair).
        ids = {n.id for n in nodes}
        assert edges[0].source_id in ids
        assert edges[0].destination_id in ids
        assert edges[0].source_id != edges[0].destination_id

    def test_strips_leading_stopword_articles(self) -> None:
        ex = RegexExtractor()
        nodes, _ = ex.extract("doc-1", [_chunk("c-1", "The Acme is here.")])
        names = {n.name for n in nodes}
        # "The" must be stripped, leaving "Acme".
        assert "Acme" in names
        assert "The Acme" not in names

    def test_empty_input_returns_empty(self) -> None:
        ex = RegexExtractor()
        nodes, edges = ex.extract("doc-1", [])
        assert nodes == []
        assert edges == []

    def test_empty_text_chunk_yields_no_entities(self) -> None:
        ex = RegexExtractor()
        nodes, edges = ex.extract("doc-1", [_chunk("c-1", "")])
        assert nodes == []
        assert edges == []

    def test_idempotent_re_extraction_same_node_ids(self) -> None:
        ex = RegexExtractor()
        chunks = [_chunk("c-1", "Alice met Bob at Acme.")]
        a_nodes, _ = ex.extract("doc-1", chunks)
        b_nodes, _ = ex.extract("doc-1", chunks)
        assert {n.id for n in a_nodes} == {n.id for n in b_nodes}

    def test_acronym_label_for_uppercase_single_word(self) -> None:
        ex = RegexExtractor()
        nodes, _ = ex.extract("doc-1", [_chunk("c-1", "We use NATO standards.")])
        labels = {n.name: n.label for n in nodes}
        assert labels.get("NATO") == "Acronym"

    def test_multi_word_entity_labelled_as_entity(self) -> None:
        ex = RegexExtractor()
        nodes, _ = ex.extract("doc-1", [_chunk("c-1", "Acme Corp ships fast.")])
        labels = {n.name: n.label for n in nodes}
        assert labels.get("Acme Corp") == "Entity"

    def test_pure_stopword_input_yields_no_entities(self) -> None:
        ex = RegexExtractor()
        nodes, _ = ex.extract("doc-1", [_chunk("c-1", "The There Here This.")])
        # Every match is a single stopword → all dropped.
        assert nodes == []

    def test_co_occurrence_edges_unique_per_pair(self) -> None:
        ex = RegexExtractor()
        # Two chunks sharing the same pair: edge must still be unique.
        nodes, edges = ex.extract(
            "doc-1",
            [
                _chunk("c-1", "Alice met Bob."),
                _chunk("c-2", "Bob saw Alice."),
            ],
        )
        relations = [(e.source_id, e.destination_id) for e in edges]
        assert len(relations) == len(set(relations)), "expected unique edge pairs"

    def test_node_mentions_track_originating_chunks(self) -> None:
        ex = RegexExtractor()
        nodes, _ = ex.extract(
            "doc-1",
            [
                _chunk("c-1", "Alice."),
                _chunk("c-2", "Alice again."),
            ],
        )
        alice = next(n for n in nodes if n.name == "Alice")
        assert sorted(alice.mentions) == ["c-1", "c-2"]


class TestServicerInputValidation:
    """The servicer enforces a few invariants before delegating to
    the extractor; cover them here without booting gRPC."""

    def test_servicer_with_default_extractor_uses_regex_baseline(self) -> None:
        s = GraphRAGServicer()
        assert isinstance(s.backend, RegexExtractor)
        assert s.backend.model_id == os.environ.get(
            "GRAPHRAG_MODEL", "regex-baseline-v1"
        )

    def test_servicer_accepts_injected_backend(self) -> None:
        class _Stub:
            model_id = "stub-v0"

            def extract(self, _doc, _chunks):
                return [], []

        s = GraphRAGServicer(backend=_Stub())
        assert s.backend.model_id == "stub-v0"


if __name__ == "__main__":
    sys.exit(pytest.main([__file__]))
