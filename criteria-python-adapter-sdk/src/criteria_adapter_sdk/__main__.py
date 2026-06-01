import json
import os
import sys

from . import serve_remote, RemoteIdentity, ServeRemoteOptions, Service
from criteria.v2 import adapter_pb2


class _NoopService(Service):
    def info(self, request, context):
        return adapter_pb2.InfoResponse(name="noop-python-adapter", version="0.1.0")

    def open_session(self, request, context):
        return adapter_pb2.OpenSessionResponse()

    def execute(self, request_iterator, context):
        yield adapter_pb2.ExecuteEvent(result=adapter_pb2.ExecuteResult(outcome="noop"))

    def log(self, request_iterator, context):
        return iter([])

    def permissions(self, request_iterator, context):
        return iter([])

    def close_session(self, request, context):
        return adapter_pb2.CloseSessionResponse()


def main() -> int:
    host = os.environ.get("CRITERIA_REMOTE_HOST", "")
    token = os.environ.get("CRITERIA_REMOTE_TOKEN", "")
    token_file = os.environ.get("CRITERIA_REMOTE_TOKEN_FILE", "")
    if token_file:
        try:
            token = open(token_file, "r").read().strip()
        except OSError as e:
            print(f"criteria_adapter_sdk: cannot read token file: {e}", file=sys.stderr)
            return 1

    if not host:
        print("criteria_adapter_sdk: CRITERIA_REMOTE_HOST is required", file=sys.stderr)
        return 1

    identity = RemoteIdentity(
        name="noop-python-adapter",
        version="0.1.0",
        digest="sha256:0000000000000000000000000000000000000000000000000000000000000000",
    )

    opts = ServeRemoteOptions(host=host, identity=identity, accept_token=token or None)
    serve_remote(_NoopService(), opts)
    return 0


if __name__ == "__main__":
    sys.exit(main())
