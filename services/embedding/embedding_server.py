"""Phase 3 embedding gRPC microservice.

Wraps a [sentence-transformers](https://www.sbert.net) model behind
the proto contract in `proto/embedding/v1/embedding.proto`. The Go
ingestion pipeline calls `ComputeEmbeddings(chunks)` and gets back
`[][]float32` plus the model id and dimension.

Stateless: tenant_id is forwarded for logging/quota only.
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
import threading
from concurrent import futures
from typing import List, Sequence, Tuple

import grpc

# Make `_proto` importable.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
if _SERVICES_DIR not in sys.path:
    sys.path.insert(0, _SERVICES_DIR)

from _proto.embedding.v1 import embedding_pb2, embedding_pb2_grpc  # noqa: E402
from _metrics import ServiceMetrics, make_metrics, start_metrics_server  # noqa: E402

LOG = logging.getLogger("embedding-server")

METRICS: ServiceMetrics = make_metrics("embedding")

_DEFAULT_MODEL = os.environ.get("EMBEDDING_MODEL", "sentence-transformers/all-MiniLM-L6-v2")
_BATCH_SIZE = int(os.environ.get("EMBEDDING_BATCH_SIZE", "32"))


class EmbeddingBackend:
    """Adapter over sentence-transformers. Lazily loads the model on
    first call so the gRPC server boots fast and tests can swap a
    fake before any heavy import."""

    def __init__(self, model_id: str = _DEFAULT_MODEL, batch_size: int = _BATCH_SIZE) -> None:
        self.model_id = model_id
        self.batch_size = batch_size
        self._model = None
        self._lock = threading.Lock()

    def _load(self):
        with self._lock:
            if self._model is None:
                from sentence_transformers import SentenceTransformer

                self._model = SentenceTransformer(self.model_id)

        return self._model

    def embed(self, chunks: Sequence[str]) -> Tuple[List[List[float]], int]:
        """Return embeddings as a Python list of lists plus the
        embedding dimension."""
        model = self._load()
        if not chunks:
            return [], 0
        vectors = model.encode(
            list(chunks),
            batch_size=self.batch_size,
            show_progress_bar=False,
            convert_to_numpy=True,
        )
        out = [v.tolist() for v in vectors]
        dim = len(out[0]) if out else 0

        return out, dim


class EmbeddingServicer(embedding_pb2_grpc.EmbeddingServiceServicer):
    """gRPC servicer that delegates to an EmbeddingBackend."""

    def __init__(self, backend: EmbeddingBackend | None = None) -> None:
        self.backend = backend or EmbeddingBackend()

    def ComputeEmbeddings(
        self,
        request: embedding_pb2.ComputeEmbeddingsRequest,
        context: grpc.ServicerContext,
    ) -> embedding_pb2.ComputeEmbeddingsResponse:
        if not request.tenant_id:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "tenant_id required")
        chunks = list(request.chunks)
        if not chunks:
            return embedding_pb2.ComputeEmbeddingsResponse(
                embeddings=[],
                model_id=self.backend.model_id,
                dimensions=0,
            )

        with METRICS.time():
            try:
                vectors, dim = self.backend.embed(chunks)
            except Exception as exc:  # noqa: BLE001
                LOG.exception("embedding failed")
                context.abort(grpc.StatusCode.INTERNAL, f"embed failed: {exc}")

        return embedding_pb2.ComputeEmbeddingsResponse(
            embeddings=[embedding_pb2.Embedding(values=v) for v in vectors],
            model_id=self.backend.model_id,
            dimensions=dim,
        )


def serve(addr: str, backend: EmbeddingBackend | None = None) -> Tuple[grpc.Server, int]:
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    embedding_pb2_grpc.add_EmbeddingServiceServicer_to_server(
        EmbeddingServicer(backend), server
    )
    port = server.add_insecure_port(addr)
    server.start()
    LOG.info("embedding-server listening on port %d", port)

    return server, port


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    parser = argparse.ArgumentParser()
    parser.add_argument("--addr", default=os.environ.get("EMBEDDING_ADDR", "[::]:50052"))
    parser.add_argument("--model", default=_DEFAULT_MODEL)
    parser.add_argument("--metrics-port", type=int, default=int(os.environ.get("METRICS_PORT", "9090")))
    args = parser.parse_args()

    start_metrics_server(args.metrics_port)
    server, _ = serve(args.addr, backend=EmbeddingBackend(model_id=args.model))
    server.wait_for_termination()


if __name__ == "__main__":
    main()
