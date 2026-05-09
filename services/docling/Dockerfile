# Phase 3 Docling gRPC microservice container.
FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    libstdc++6 \
    libgomp1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /srv

# Install deps separately so they're cached between source changes.
COPY services/docling/requirements.txt /srv/requirements.txt
RUN pip install -r /srv/requirements.txt

# Generate proto stubs into /srv/services/_proto.
COPY proto /srv/proto
COPY services/gen_protos.sh /srv/services/gen_protos.sh
RUN bash /srv/services/gen_protos.sh && \
    test -f /srv/services/_proto/docling/v1/docling_pb2.py

# Copy server code.
COPY services/docling /srv/services/docling

ENV PYTHONPATH=/srv/services

EXPOSE 50051

CMD ["python", "/srv/services/docling/docling_server.py", "--addr", "[::]:50051"]
