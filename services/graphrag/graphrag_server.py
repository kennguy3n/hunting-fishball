"""Phase 3 GraphRAG entity-extraction gRPC microservice.

Wraps a lightweight entity/relation extractor behind the proto
contract in ``proto/graphrag/v1/graphrag.proto``. The Go ingestion
pipeline calls ``ExtractEntities(chunks)`` from Stage 3b (after
Embed, before Store) and gets back a list of nodes + edges that
the coordinator writes into the per-tenant FalkorDB graph for
traversal-aided retrieval.

The default backend is a deterministic regex extractor — it finds
capitalised n-grams as Person/Org candidates and emits MENTIONED_IN
edges between every entity pair that co-occur in the same chunk.
This is *not* a state-of-the-art extractor but it gives the
pipeline a stable, dependency-free baseline that satisfies the
proto contract and runs in the same Docker stack as the other
ML microservices. Operators can swap a heavier backend (spaCy,
GLiNER, OpenAI structured output) by replacing the Backend object
passed to ``serve``.

Stateless: tenant_id is forwarded for logging/quota only.
"""

from __future__ import annotations

import argparse
import logging
import os
import re
import sys
from concurrent import futures
from dataclasses import dataclass, field
from typing import Iterable, List, Sequence, Tuple

import grpc

# Make `_proto` importable.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
if _SERVICES_DIR not in sys.path:
    sys.path.insert(0, _SERVICES_DIR)

from _proto.graphrag.v1 import graphrag_pb2, graphrag_pb2_grpc  # noqa: E402

LOG = logging.getLogger("graphrag-server")

_DEFAULT_MODEL = os.environ.get("GRAPHRAG_MODEL", "regex-baseline-v1")

# Capitalised single-or-multi-word phrases. Conservative: requires
# at least one capital letter at the start, allows hyphens/dots, and
# bounds the run-length so we don't latch onto sentence fragments.
_ENTITY_RE = re.compile(r"\b([A-Z][A-Za-z0-9.\-']*(?:\s+[A-Z][A-Za-z0-9.\-']*){0,3})\b")

# Words that match the regex but are not real entities. Conservative
# stop-list — we'd rather drop a real entity than overload the graph
# with sentence-start noise.
_STOPWORDS = {
    "The",
    "A",
    "An",
    "I",
    "We",
    "They",
    "You",
    "He",
    "She",
    "It",
    "This",
    "That",
    "These",
    "Those",
    "There",
    "Here",
}


@dataclass
class _ExtractedNode:
    id: str
    label: str
    name: str
    mentions: List[str] = field(default_factory=list)


@dataclass
class _ExtractedEdge:
    source_id: str
    destination_id: str
    relation: str
    confidence: float = 0.0


class RegexExtractor:
    """Deterministic baseline entity extractor.

    Finds capitalised n-grams as Person/Org candidates and emits
    MENTIONED_WITH edges between every co-occurring pair. Idempotent
    for a given (document_id, chunks) input — the coordinator can
    re-extract on any chunk update without producing dup nodes.
    """

    def __init__(self, model_id: str = _DEFAULT_MODEL) -> None:
        self.model_id = model_id

    def extract(
        self, document_id: str, chunks: Sequence[graphrag_pb2.Chunk]
    ) -> Tuple[List[_ExtractedNode], List[_ExtractedEdge]]:
        nodes_by_id: dict[str, _ExtractedNode] = {}
        chunk_to_nodes: dict[str, List[str]] = {}

        for ch in chunks:
            chunk_id = ch.chunk_id or ""
            seen: List[str] = []
            for m in _ENTITY_RE.finditer(ch.text or ""):
                surface = m.group(1).strip()
                # Drop leading stopwords ("The Acme" -> "Acme") so we
                # don't lose real entities to a sentence-start article.
                while surface:
                    head, _, rest = surface.partition(" ")
                    if head in _STOPWORDS and rest:
                        surface = rest
                        continue
                    break
                if not surface or surface.split(" ", 1)[0] in _STOPWORDS:
                    continue
                node_id = self._node_id(document_id, surface)
                node = nodes_by_id.get(node_id)
                if node is None:
                    node = _ExtractedNode(
                        id=node_id,
                        label=self._label_for(surface),
                        name=surface,
                    )
                    nodes_by_id[node_id] = node
                if chunk_id and chunk_id not in node.mentions:
                    node.mentions.append(chunk_id)
                if node_id not in seen:
                    seen.append(node_id)
            if chunk_id:
                chunk_to_nodes[chunk_id] = seen

        edges: List[_ExtractedEdge] = []
        seen_pairs: set[Tuple[str, str]] = set()
        for chunk_id, ids in chunk_to_nodes.items():
            for i, a in enumerate(ids):
                for b in ids[i + 1 :]:
                    pair = (a, b) if a < b else (b, a)
                    if pair in seen_pairs:
                        continue
                    seen_pairs.add(pair)
                    edges.append(
                        _ExtractedEdge(
                            source_id=pair[0],
                            destination_id=pair[1],
                            relation="MENTIONED_WITH",
                            confidence=0.5,
                        )
                    )

        return list(nodes_by_id.values()), edges

    @staticmethod
    def _node_id(document_id: str, surface: str) -> str:
        # Stable, content-addressed: same (document, surface form)
        # always maps to the same id, so re-extraction MERGEs in
        # the graph rather than duplicating.
        slug = re.sub(r"[^A-Za-z0-9]+", "-", surface.lower()).strip("-")
        if document_id:
            return f"{document_id}:{slug}"
        return slug

    @staticmethod
    def _label_for(surface: str) -> str:
        # Heuristic: 1 word & all-caps -> "Acronym"; > 1 word -> "Entity".
        if " " not in surface and surface.isupper():
            return "Acronym"
        return "Entity"


class GraphRAGServicer(graphrag_pb2_grpc.GraphRAGServiceServicer):
    """gRPC servicer that delegates to a backend extractor."""

    def __init__(self, backend: RegexExtractor | None = None) -> None:
        self.backend = backend or RegexExtractor()

    def ExtractEntities(
        self,
        request: graphrag_pb2.ExtractRequest,
        context: grpc.ServicerContext,
    ) -> graphrag_pb2.ExtractResponse:
        if not request.tenant_id:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "tenant_id required")
        try:
            nodes, edges = self.backend.extract(request.document_id, list(request.chunks))
        except Exception as exc:  # noqa: BLE001
            LOG.exception("graphrag extraction failed")
            context.abort(grpc.StatusCode.INTERNAL, f"extract failed: {exc}")

        return graphrag_pb2.ExtractResponse(
            nodes=[
                graphrag_pb2.Node(
                    id=n.id,
                    label=n.label,
                    name=n.name,
                    mentions=list(n.mentions),
                )
                for n in nodes
            ],
            edges=[
                graphrag_pb2.Edge(
                    source_id=e.source_id,
                    destination_id=e.destination_id,
                    relation=e.relation,
                    confidence=e.confidence,
                )
                for e in edges
            ],
            model_id=self.backend.model_id,
        )


def serve(addr: str, backend: RegexExtractor | None = None) -> Tuple[grpc.Server, int]:
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    graphrag_pb2_grpc.add_GraphRAGServiceServicer_to_server(
        GraphRAGServicer(backend), server
    )
    port = server.add_insecure_port(addr)
    server.start()
    LOG.info("graphrag-server listening on port %d", port)

    return server, port


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--addr", default=os.environ.get("GRAPHRAG_ADDR", "[::]:50054")
    )
    args = parser.parse_args()
    server, _ = serve(args.addr)
    server.wait_for_termination()


# `_unused_iterable` keeps the typing.Iterable import meaningful for
# downstream subclassers that want to type-annotate richer extractors.
_unused_iterable: Iterable[str] = []


if __name__ == "__main__":
    main()
