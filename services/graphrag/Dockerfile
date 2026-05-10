# Phase 3 GraphRAG entity-extraction gRPC microservice container.
FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /srv

COPY services/graphrag/requirements.txt /srv/requirements.txt
RUN pip install -r /srv/requirements.txt

# Generate proto stubs into /srv/services/_proto.
COPY proto /srv/proto
COPY services/gen_protos.sh /srv/services/gen_protos.sh
RUN bash /srv/services/gen_protos.sh && \
    test -f /srv/services/_proto/graphrag/v1/graphrag_pb2.py

# Shared Prometheus scaffolding (`_metrics`); kept available so any
# future graphrag-server import resolves without a Dockerfile change.
COPY services/_metrics.py /srv/services/_metrics.py

COPY services/graphrag /srv/services/graphrag

ENV PYTHONPATH=/srv/services

EXPOSE 50054

CMD ["python", "/srv/services/graphrag/graphrag_server.py", "--addr", "[::]:50054"]
