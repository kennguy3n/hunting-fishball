#!/usr/bin/env bash
# Generate Python gRPC stubs for the three ML microservices into
# services/_proto. Idempotent — safe to re-run.
#
# The generated package names ("docling.v1", "embedding.v1",
# "memory.v1") collide with the real Python ML libraries we wrap
# (Docling, mem0ai, ...). Post-generation we rewrite the imports in
# every generated file to use the `_proto.<service>.v1` namespace
# instead, so a server module can import both:
#
#   from _proto.docling.v1 import docling_pb2_grpc  # generated stub
#   import docling                                  # real library
#
# The `_proto` package is rooted at services/, which the server
# bootstraps onto sys.path.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")"/.. && pwd)"
OUT="${REPO_ROOT}/services/_proto"
PROTO_DIR="${REPO_ROOT}/proto"

rm -rf "$OUT"
mkdir -p "$OUT"
touch "$OUT/__init__.py"

python3 -m grpc_tools.protoc \
  --proto_path="$PROTO_DIR" \
  --python_out="$OUT" \
  --grpc_python_out="$OUT" \
  $(find "$PROTO_DIR" -name '*.proto')

# Make every generated subdirectory a real package.
find "$OUT" -type d -exec sh -c 'touch "$1/__init__.py"' _ {} \;

# Rewrite generated imports to use the `_proto.<service>.v1` namespace
# so they don't shadow the real ML libraries (`docling`, `mem0ai`, ...).
for f in $(find "$OUT" -name '*_pb2*.py'); do
  python3 - "$f" <<'PY'
import re, sys
path = sys.argv[1]
with open(path) as fh:
    src = fh.read()
src = re.sub(
    r"^from (docling|embedding|memory)\.v1 ",
    r"from _proto.\1.v1 ",
    src,
    flags=re.MULTILINE,
)
with open(path, "w") as fh:
    fh.write(src)
PY
done
