"""Round-18 Task 9 — cross-encoder reranker gRPC sidecar.

Wraps a cross-encoder model (e.g. `cross-encoder/ms-marco-MiniLM-L-6-v2`)
behind the proto contract in `proto/reranker/v1/reranker.proto`.
The Go retrieval handler calls `Rerank(query, candidates)` and gets
back a list of `(chunk_id, score)` pairs sorted descending.

Stateless beyond model state; tenant_id is forwarded for logging
and quota accounting only.

The default `CrossEncoderBackend` lazily imports `sentence-transformers`
on first use, so the gRPC server boots fast and unit tests can swap
in a fake before any heavy imports happen.
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
from grpc_health.v1 import health, health_pb2, health_pb2_grpc

# Make `_proto` importable.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
if _SERVICES_DIR not in sys.path:
    sys.path.insert(0, _SERVICES_DIR)

from _proto.reranker.v1 import reranker_pb2, reranker_pb2_grpc  # noqa: E402
from _metrics import ServiceMetrics, make_metrics, start_metrics_server  # noqa: E402

LOG = logging.getLogger("reranker-server")

METRICS: ServiceMetrics = make_metrics("reranker")

_DEFAULT_MODEL = os.environ.get(
    "RERANKER_MODEL", "cross-encoder/ms-marco-MiniLM-L-6-v2"
)
_BATCH_SIZE = int(os.environ.get("RERANKER_BATCH_SIZE", "32"))


class CrossEncoderBackend:
    """Adapter over sentence-transformers' CrossEncoder. Lazily
    loads the model on first call so the gRPC server boots fast and
    tests can swap in a fake before any heavy import."""

    def __init__(self, model_id: str = _DEFAULT_MODEL, batch_size: int = _BATCH_SIZE) -> None:
        self.model_id = model_id
        self.batch_size = batch_size
        self._model = None
        self._lock = threading.Lock()

    def _load(self):
        with self._lock:
            if self._model is None:
                from sentence_transformers import CrossEncoder

                self._model = CrossEncoder(self.model_id)

        return self._model

    def score(self, query: str, texts: Sequence[str]) -> List[float]:
        """Return a list of relevance scores aligned to `texts`."""
        model = self._load()
        if not texts:
            return []
        pairs = [(query, t) for t in texts]
        scores = model.predict(pairs, batch_size=self.batch_size, show_progress_bar=False)

        return [float(s) for s in scores]


class RerankerServicer(reranker_pb2_grpc.RerankerServiceServicer):
    """gRPC servicer that delegates to a CrossEncoderBackend."""

    def __init__(self, backend: CrossEncoderBackend | None = None) -> None:
        self.backend = backend or CrossEncoderBackend()

    def Rerank(
        self,
        request: reranker_pb2.RerankRequest,
        context: grpc.ServicerContext,
    ) -> reranker_pb2.RerankResponse:
        if not request.tenant_id:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "tenant_id required")
        if not request.query:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "query required")
        candidates = list(request.candidates)
        if not candidates:
            return reranker_pb2.RerankResponse(
                scored=[], model_id=self.backend.model_id
            )

        texts = [c.text for c in candidates]
        with METRICS.time():
            try:
                scores = self.backend.score(request.query, texts)
            except Exception as exc:  # noqa: BLE001
                LOG.exception("rerank failed")
                context.abort(grpc.StatusCode.INTERNAL, f"rerank failed: {exc}")

        scored = [
            reranker_pb2.ScoredCandidate(chunk_id=c.chunk_id, score=float(s))
            for c, s in zip(candidates, scores)
        ]
        scored.sort(key=lambda sc: sc.score, reverse=True)
        if request.top_k > 0 and request.top_k < len(scored):
            scored = scored[: request.top_k]

        return reranker_pb2.RerankResponse(scored=scored, model_id=self.backend.model_id)


def serve(addr: str, backend: CrossEncoderBackend | None = None) -> Tuple[grpc.Server, int]:
    """Build a gRPC server and start it. Registers the standard
    grpc.health.v1.Health servicer alongside the domain RPC so
    liveness probes and the Go-side circuit breaker can check
    SERVING state cheaply."""
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    reranker_pb2_grpc.add_RerankerServiceServicer_to_server(
        RerankerServicer(backend), server
    )

    health_servicer = health.HealthServicer()
    health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)
    health_servicer.set(
        "reranker.v1.RerankerService",
        health_pb2.HealthCheckResponse.SERVING,
    )
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)

    port = server.add_insecure_port(addr)
    server.start()
    LOG.info("reranker-server listening on port %d", port)

    return server, port


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--addr", default=os.environ.get("RERANKER_ADDR", "[::]:50056")
    )
    parser.add_argument("--model", default=_DEFAULT_MODEL)
    parser.add_argument(
        "--metrics-port", type=int, default=int(os.environ.get("METRICS_PORT", "9090"))
    )
    args = parser.parse_args()

    start_metrics_server(args.metrics_port)
    server, _ = serve(args.addr, backend=CrossEncoderBackend(model_id=args.model))
    server.wait_for_termination()


if __name__ == "__main__":
    main()
