import json
import os
import sys

from . import serve_remote, RemoteIdentity, ServeRemoteOptions


class _NoopService:
    def info(self):
        return {"name": "noop-python-adapter", "version": "0.1.0"}

    def open_session(self, request):
        return {}

    def execute(self, request):
        return {"output": "noop"}

    def log(self, request):
        return {}

    def permissions(self, request):
        return {"permissions": []}

    def close_session(self, request):
        return {}


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
