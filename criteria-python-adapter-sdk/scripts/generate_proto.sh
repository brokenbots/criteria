#!/usr/bin/env bash
# Proto generation script for criteria-python-adapter-sdk.
# Pinned to the criteria monorepo v2 proto digest:
#   adapter.proto  sha256:fe8db3f2f35d671789f5ef3f6eb2995b3eceadfe17fe3ef289e353da207650b6
#   options.proto  sha256:e6bd421c4a30828185888ba4cb3a7372efc29c25a26acca63e52157130b74097
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT_DIR="${ROOT_DIR}/src/criteria/v2"
PROTO_DIR="${ROOT_DIR}/protos"

python -m grpc_tools.protoc \
    --proto_path="${PROTO_DIR}" \
    --python_out="${ROOT_DIR}/src" \
    --grpc_python_out="${ROOT_DIR}/src" \
    "${PROTO_DIR}/criteria/v2/adapter.proto" \
    "${PROTO_DIR}/criteria/v2/options.proto"

# Fix relative imports in generated grpc file so it works as a package.
# The generated file imports adapter_pb2 as a top-level module; we need
# a relative import so it resolves inside the criteria.v2 package.
sed -i 's/^import adapter_pb2 as/from . import adapter_pb2 as/' "${OUT_DIR}/adapter_pb2_grpc.py"

echo "Generated Python bindings in ${OUT_DIR}"
