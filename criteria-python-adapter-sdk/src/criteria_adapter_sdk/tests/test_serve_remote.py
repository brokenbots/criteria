import json
import os
import socket
import tempfile
import time
from concurrent import futures
from threading import Thread

import grpc
import pytest

from criteria_adapter_sdk.serve_remote import serve_remote, RemoteIdentity, ServeRemoteOptions, Service
from criteria.v2 import adapter_pb2, adapter_pb2_grpc


def _pick_sock(prefix: str) -> str:
    return os.path.join(tempfile.gettempdir(), f"criteria-py-test-{prefix}-{os.getpid()}-{int(time.time()*1000)}.sock")


def _readline(conn: socket.socket) -> str:
    buf = b""
    while b"\n" not in buf:
        chunk = conn.recv(1024)
        if not chunk:
            raise ConnectionError("closed before newline")
        buf += chunk
    return buf.split(b"\n", 1)[0].decode("utf-8")


class _FakeService(Service):
    def __init__(self, identity: RemoteIdentity):
        self.identity = identity

    def info(self, request, context):
        return adapter_pb2.InfoResponse(
            name=self.identity.name,
            version=self.identity.version,
            description="test",
            sdk_protocol_version="2",
        )


def test_handshake_and_info_round_trip():
    host_sock = _pick_sock("host")
    adapter_sock = _pick_sock("adapter")

    # Host side
    host_server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    host_server.bind(host_sock)
    host_server.listen(1)

    conn_accepted = None

    def accept_conn():
        nonlocal conn_accepted
        conn_accepted, _ = host_server.accept()

    accept_thread = Thread(target=accept_conn, daemon=True)
    accept_thread.start()

    identity = RemoteIdentity(name="test-adapter", version="1.0.0", digest="sha256:abc123")
    svc = _FakeService(identity)

    adapter_thread = Thread(
        target=serve_remote,
        args=(svc, ServeRemoteOptions(host=host_sock, identity=identity, accept_token="secret-token", socket_path=adapter_sock)),
        daemon=True,
    )
    adapter_thread.start()

    accept_thread.join(timeout=5)
    assert conn_accepted is not None, "adapter did not connect"

    line = _readline(conn_accepted)
    handshake = json.loads(line)
    assert handshake["name"] == "test-adapter"
    assert handshake["version"] == "1.0.0"
    assert handshake["digest"] == "sha256:abc123"
    assert handshake["token"] == "secret-token"
    assert handshake["sdk_protocol_version"] == 2

    # Connect to adapter's internal gRPC server
    channel = grpc.insecure_channel(f"unix://{adapter_sock}")
    stub = adapter_pb2_grpc.AdapterServiceStub(channel)
    resp = stub.Info(adapter_pb2.InfoRequest())
    assert resp.name == "test-adapter"
    assert resp.version == "1.0.0"

    # Cleanup
    channel.close()
    conn_accepted.close()
    host_server.close()
    adapter_thread.join(timeout=2)

    try:
        os.unlink(host_sock)
    except FileNotFoundError:
        pass
    try:
        os.unlink(adapter_sock)
    except FileNotFoundError:
        pass
