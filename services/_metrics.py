"""Phase 8 — shared Prometheus metrics scaffolding for the Python gRPC
sidecars (docling, embedding, memory).

Every sidecar exposes the same shape:

* `<name>_requests_total{status}` — counter, labelled by gRPC status
  ("ok" / "error").
* `<name>_duration_seconds` — histogram of per-request latency.
* `<name>_queue_depth` — gauge of in-flight requests, used by the
  HPA to scale before CPU saturates.

Each sidecar imports `make_metrics(prefix)` to mint a triple of
collectors and `start_metrics_server(port)` to start the
http listener that Prometheus scrapes (HPA + dashboards).
"""

from __future__ import annotations

import logging
import os
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Iterator

from prometheus_client import Counter, Gauge, Histogram, start_http_server

LOG = logging.getLogger("metrics")


@dataclass
class ServiceMetrics:
    """Triple of collectors per sidecar."""

    requests_total: Counter
    duration_seconds: Histogram
    queue_depth: Gauge

    @contextmanager
    def time(self) -> Iterator[None]:
        """Context manager: increments queue_depth, observes duration,
        and records the request as ok / error based on whether the
        wrapped block raised."""
        self.queue_depth.inc()
        status = "ok"
        try:
            with self.duration_seconds.time():
                yield
        except Exception:
            status = "error"
            raise
        finally:
            self.queue_depth.dec()
            self.requests_total.labels(status=status).inc()


def make_metrics(prefix: str) -> ServiceMetrics:
    """Create the three collectors for a sidecar named ``prefix``."""
    return ServiceMetrics(
        requests_total=Counter(
            f"{prefix}_requests_total",
            f"Total {prefix} requests served, labelled by gRPC status.",
            ["status"],
        ),
        duration_seconds=Histogram(
            f"{prefix}_duration_seconds",
            f"{prefix} request duration in seconds.",
            buckets=(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60),
        ),
        queue_depth=Gauge(
            f"{prefix}_queue_depth",
            f"Number of in-flight {prefix} requests.",
        ),
    )


def start_metrics_server(port: int | None = None) -> int:
    """Start the Prometheus HTTP server.

    The default port is 9090; callers can override via the
    ``METRICS_PORT`` env var (taking precedence over the argument).
    Returns the bound port for tests.
    """
    if port is None:
        port = int(os.environ.get("METRICS_PORT", "9090"))
    start_http_server(port)
    LOG.info("metrics: serving /metrics on :%d", port)
    return port
