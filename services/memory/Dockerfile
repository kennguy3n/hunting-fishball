# Phase 3 Mem0 gRPC microservice container.
FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    libstdc++6 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /srv

COPY services/memory/requirements.txt /srv/requirements.txt
RUN pip install -r /srv/requirements.txt

COPY proto /srv/proto
COPY services/gen_protos.sh /srv/services/gen_protos.sh
RUN bash /srv/services/gen_protos.sh && \
    test -f /srv/services/_proto/memory/v1/memory_pb2.py

COPY services/memory /srv/services/memory

ENV PYTHONPATH=/srv/services

EXPOSE 50053

CMD ["python", "/srv/services/memory/memory_server.py", "--addr", "[::]:50053"]
