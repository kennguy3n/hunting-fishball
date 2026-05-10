# Phase 3 embedding gRPC microservice container.
FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    libstdc++6 \
    libgomp1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /srv

COPY services/embedding/requirements.txt /srv/requirements.txt
RUN pip install -r /srv/requirements.txt

COPY proto /srv/proto
COPY services/gen_protos.sh /srv/services/gen_protos.sh
RUN bash /srv/services/gen_protos.sh && \
    test -f /srv/services/_proto/embedding/v1/embedding_pb2.py

# Shared Prometheus scaffolding imported as `_metrics` from the
# embedding server (PYTHONPATH=/srv/services).
COPY services/_metrics.py /srv/services/_metrics.py

COPY services/embedding /srv/services/embedding

ENV PYTHONPATH=/srv/services
ENV EMBEDDING_MODEL=sentence-transformers/all-MiniLM-L6-v2

EXPOSE 50052

CMD ["python", "/srv/services/embedding/embedding_server.py", "--addr", "[::]:50052"]
