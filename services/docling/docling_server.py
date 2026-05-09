"""Phase 3 Docling gRPC microservice.

Wraps the [Docling](https://github.com/DS4SD/docling) document parser
behind the proto contract in `proto/docling/v1/docling.proto`. The Go
ingestion pipeline calls `ParseDocument(content, content_type)` and
gets back `[]ParsedBlock` plus a `DocumentStructure`.

Stateless: no per-tenant state lives here. Tenancy is enforced at the
caller (the Go pipeline tags each request with `tenant_id`).
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
from concurrent import futures
from typing import Iterable, Tuple

import grpc

# Make `_proto` importable. Servers run from anywhere, but the proto
# stubs live under `services/_proto`.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
if _SERVICES_DIR not in sys.path:
    sys.path.insert(0, _SERVICES_DIR)

from _proto.docling.v1 import docling_pb2, docling_pb2_grpc  # noqa: E402

LOG = logging.getLogger("docling-server")


class DoclingBackend:
    """Adapter over the real Docling library.

    The class is intentionally narrow so unit tests can swap it out
    for a fake without touching the gRPC layer.
    """

    def parse(
        self, content: bytes, content_type: str
    ) -> Tuple[Iterable[docling_pb2.ParsedBlock], docling_pb2.DocumentStructure]:
        # Lazy-import to keep cold-start cheap when running tests with
        # the FakeDoclingBackend.
        from docling.datamodel.base_models import InputFormat
        from docling.document_converter import DocumentConverter

        fmt = _format_for(content_type)
        converter = DocumentConverter()
        result = converter.convert_string(content, format=fmt)  # type: ignore[arg-type]
        doc = result.document

        blocks = list(_blocks_from_docling(doc))
        structure = docling_pb2.DocumentStructure(
            title=getattr(doc, "name", "") or "",
            page_count=len(getattr(doc, "pages", []) or []),
            outline_md=_outline_md(doc),
        )

        return blocks, structure


def _format_for(content_type: str):
    """Map MIME to Docling input format. Defaults to PDF."""
    from docling.datamodel.base_models import InputFormat

    return {
        "application/pdf": InputFormat.PDF,
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document": InputFormat.DOCX,
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": InputFormat.XLSX,
        "application/vnd.openxmlformats-officedocument.presentationml.presentation": InputFormat.PPTX,
        "text/html": InputFormat.HTML,
        "application/epub+zip": InputFormat.EPUB,
        "text/markdown": InputFormat.MD,
        "text/plain": InputFormat.TEXT,
    }.get(content_type, InputFormat.PDF)


def _blocks_from_docling(doc) -> Iterable["docling_pb2.ParsedBlock"]:
    """Project Docling's document structure into ParsedBlock messages."""
    pos = 0
    for item in getattr(doc, "iterate_items", lambda: [])():
        kind = type(item).__name__.lower()
        block_type = docling_pb2.BLOCK_TYPE_PARAGRAPH
        heading_level = 0
        if "heading" in kind:
            block_type = docling_pb2.BLOCK_TYPE_HEADING
            heading_level = int(getattr(item, "level", 1) or 1)
        elif "table" in kind:
            block_type = docling_pb2.BLOCK_TYPE_TABLE
        elif "figure" in kind or "picture" in kind:
            block_type = docling_pb2.BLOCK_TYPE_FIGURE
        elif "code" in kind:
            block_type = docling_pb2.BLOCK_TYPE_CODE
        elif "list" in kind:
            block_type = docling_pb2.BLOCK_TYPE_LIST_ITEM

        text = getattr(item, "text", "") or ""
        yield docling_pb2.ParsedBlock(
            block_id=f"block-{pos}",
            text=text,
            type=block_type,
            heading_level=heading_level,
            page_number=int(getattr(item, "page_no", 0) or 0),
            position=pos,
        )
        pos += 1


def _outline_md(doc) -> str:
    """Render the document outline as a markdown string. Optional —
    if Docling doesn't surface one, return empty."""
    fn = getattr(doc, "export_to_markdown", None)

    return fn() if callable(fn) else ""


class DoclingServicer(docling_pb2_grpc.DoclingServiceServicer):
    """gRPC servicer that delegates to a DoclingBackend."""

    def __init__(self, backend: DoclingBackend | None = None) -> None:
        self.backend = backend or DoclingBackend()

    def ParseDocument(
        self,
        request: docling_pb2.ParseDocumentRequest,
        context: grpc.ServicerContext,
    ) -> docling_pb2.ParseDocumentResponse:
        if not request.tenant_id:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "tenant_id required")
        if not request.content:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "content required")

        try:
            blocks, structure = self.backend.parse(request.content, request.content_type)
        except Exception as exc:  # noqa: BLE001
            LOG.exception("docling parse failed")
            context.abort(grpc.StatusCode.INTERNAL, f"parse failed: {exc}")

        return docling_pb2.ParseDocumentResponse(blocks=list(blocks), structure=structure)


def serve(addr: str, backend: DoclingBackend | None = None) -> Tuple[grpc.Server, int]:
    """Build a gRPC server and start it.

    Returns the server plus the bound port (resolves "[::]:0" so unit
    tests can pick a port without contention).
    """
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    docling_pb2_grpc.add_DoclingServiceServicer_to_server(DoclingServicer(backend), server)
    port = server.add_insecure_port(addr)
    server.start()
    LOG.info("docling-server listening on port %d", port)

    return server, port


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    parser = argparse.ArgumentParser()
    parser.add_argument("--addr", default=os.environ.get("DOCLING_ADDR", "[::]:50051"))
    args = parser.parse_args()

    server, _ = serve(args.addr)
    server.wait_for_termination()


if __name__ == "__main__":
    main()
