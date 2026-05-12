# Round-18 Task 9 cross-encoder reranker gRPC microservice container.
FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    libstdc++6 \
    libgomp1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /srv

COPY services/reranker/requirements.txt /srv/requirements.txt
RUN pip install -r /srv/requirements.txt

COPY proto /srv/proto
COPY services/gen_protos.sh /srv/services/gen_protos.sh
RUN bash /srv/services/gen_protos.sh && \
    test -f /srv/services/_proto/reranker/v1/reranker_pb2.py

# Shared Prometheus scaffolding imported as `_metrics` from the
# reranker server (PYTHONPATH=/srv/services).
COPY services/_metrics.py /srv/services/_metrics.py

COPY services/reranker /srv/services/reranker

ENV PYTHONPATH=/srv/services
ENV RERANKER_MODEL=cross-encoder/ms-marco-MiniLM-L-6-v2

EXPOSE 50056

CMD ["python", "/srv/services/reranker/reranker_server.py", "--addr", "[::]:50056"]
