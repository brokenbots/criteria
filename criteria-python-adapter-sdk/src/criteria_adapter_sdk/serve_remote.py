import json
import os
import socket
import ssl
import tempfile
import threading
from concurrent import futures
from dataclasses import dataclass
from typing import Optional

import grpc

from criteria.v2 import adapter_pb2, adapter_pb2_grpc


@dataclass
class RemoteIdentity:
    name: str
    version: str
    digest: str


@dataclass
class ServeRemoteOptions:
    host: str
    identity: RemoteIdentity
    accept_token: Optional[str] = None
    tls: Optional[ssl.SSLContext] = None
    socket_path: Optional[str] = None


class _AdapterServicer(adapter_pb2_grpc.AdapterServiceServicer):
    def __init__(self, handler: "Service"):
        self._handler = handler

    def Info(self, request, context):
        return self._handler.info(request, context)


class Service:
    def info(self, request, context):
        raise NotImplementedError


def _dial_remote(host: str, tls: Optional[ssl.SSLContext] = None) -> socket.socket:
    if os.path.isabs(host) or host.startswith("/"):
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.connect(host)
        return sock

    # TCP
    if ":" in host:
        hostname, port_str = host.rsplit(":", 1)
        port = int(port_str)
    else:
        hostname = host
        port = 443

    if tls is not None:
        sock = socket.create_connection((hostname, port))
        return tls.wrap_socket(sock, server_hostname=hostname)
    return socket.create_connection((hostname, port))


def _send_handshake(conn: socket.socket, identity: RemoteIdentity, token: Optional[str] = None) -> None:
    msg = {
        "name": identity.name,
        "version": identity.version,
        "digest": identity.digest,
        "token": token,
        "sdk_protocol_version": 2,
    }
    line = json.dumps({k: v for k, v in msg.items() if v is not None}) + "\n"
    conn.sendall(line.encode("utf-8"))


def _bridge_sockets(a: socket.socket, b: socket.socket) -> None:
    def forward(src: socket.socket, dst: socket.socket):
        try:
            while True:
                data = src.recv(65536)
                if not data:
                    break
                dst.sendall(data)
        except (OSError, BrokenPipeError):
            pass
        finally:
            try:
                src.shutdown(socket.SHUT_RD)
            except OSError:
                pass
            try:
                dst.shutdown(socket.SHUT_WR)
            except OSError:
                pass

    t1 = threading.Thread(target=forward, args=(a, b), daemon=True)
    t2 = threading.Thread(target=forward, args=(b, a), daemon=True)
    t1.start()
    t2.start()
    t1.join()
    t2.join()


def serve_remote(service: Service, opts: ServeRemoteOptions) -> None:
    if not opts.host:
        raise ValueError("serveRemote: host is required")

    socket_path = opts.socket_path or os.path.join(
        tempfile.gettempdir(),
        f"criteria-py-adapter-{os.getpid()}.sock",
    )

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=1))
    adapter_pb2_grpc.add_AdapterServiceServicer_to_server(_AdapterServicer(service), server)
    server.add_insecure_port(f"unix://{socket_path}")
    server.start()

    conn = _dial_remote(opts.host, opts.tls)
    try:
        _send_handshake(conn, opts.identity, opts.accept_token)
    except Exception as e:
        conn.close()
        server.stop(0)
        raise RuntimeError(f"serveRemote: handshake failed: {e}") from e

    local = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        local.connect(socket_path)
    except Exception as e:
        conn.close()
        server.stop(0)
        raise RuntimeError(f"serveRemote: connect to internal socket failed: {e}") from e

    try:
        _bridge_sockets(conn, local)
    finally:
        conn.close()
        local.close()
        server.stop(0)
