"""Local adapter entrypoint: go-plugin-compatible gRPC server.

`serve({...})` checks the go-plugin magic-cookie handshake, starts a gRPC
server on a Unix socket, prints the connection line to stdout, and blocks
until the host disconnects.
"""

import asyncio
import inspect
import os
import sys
import tempfile
from concurrent import futures
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable, Dict, List, Optional, Protocol, Union

import grpc

from criteria.v2 import adapter_pb2, adapter_pb2_grpc

from .helpers import Helpers
from .schema import dict_to_schema_proto


# Handler return types
ExecuteResultLike = Union[adapter_pb2.ExecuteResult, dict, None]


class OpenSessionHandler(Protocol):
    def __call__(self, session_id: str, config: dict, helpers: Helpers) -> Union[None, Awaitable[None]]: ...


class ExecuteHandler(Protocol):
    def __call__(self, req: adapter_pb2.ExecuteRequest, helpers: Helpers) -> Union[ExecuteResultLike, Awaitable[ExecuteResultLike]]: ...


class CloseSessionHandler(Protocol):
    def __call__(self, session_id: str, helpers: Helpers) -> Union[None, Awaitable[None]]: ...


class SnapshotHandler(Protocol):
    def __call__(self, session_id: str, helpers: Helpers) -> Union[bytes, Awaitable[bytes]]: ...


class RestoreHandler(Protocol):
    def __call__(self, session_id: str, state: bytes, helpers: Helpers) -> Union[None, Awaitable[None]]: ...


class InspectHandler(Protocol):
    def __call__(self, session_id: str, helpers: Helpers) -> Union[adapter_pb2.InspectResponse, Awaitable[adapter_pb2.InspectResponse]]: ...


class LogHandler(Protocol):
    def __call__(self, session_id: str, step_name: str, helpers: Helpers) -> Union[None, Awaitable[None]]: ...


class PermissionHandler(Protocol):
    def __call__(self, request_iterator, context) -> Any: ...


@dataclass
class ServeConfig:
    name: str
    version: str
    source_url: str = ""
    description: str = ""
    capabilities: List[str] = field(default_factory=list)
    platforms: List[str] = field(default_factory=list)
    config_schema: Optional[adapter_pb2.AdapterSchemaProto] = None
    input_schema: Optional[adapter_pb2.AdapterSchemaProto] = None
    output_schema: Optional[adapter_pb2.AdapterSchemaProto] = None
    secrets: List[Dict[str, Any]] = field(default_factory=list)
    permissions: List[str] = field(default_factory=list)
    compatible_environments: List[str] = field(default_factory=list)
    container_image: str = ""
    supported_features: List[str] = field(default_factory=list)
    max_chunk_bytes: int = 0
    open_session: Optional[OpenSessionHandler] = None
    execute: ExecuteHandler = field(default=None)  # type: ignore[assignment]
    close_session: Optional[CloseSessionHandler] = None
    snapshot: Optional[SnapshotHandler] = None
    restore: Optional[RestoreHandler] = None
    inspect: Optional[InspectHandler] = None
    log: Optional[LogHandler] = None
    permissions_handler: Optional[PermissionHandler] = None


# go-plugin protocol constants
_MAGIC_COOKIE_KEY = "CRITERIA_PLUGIN"
_MAGIC_COOKIE_VALUE = "7a1bf31f-c805-4e75-a31c-22195c9fdd4c"
_GO_PLUGIN_CORE_VERSION = 1
_GO_PLUGIN_PROTOCOL_VERSION = 2


def _emit_manifest(config: ServeConfig) -> int:
    """Print adapter manifest YAML to stdout and exit."""
    import yaml  # type: ignore[import-untyped]

    secrets_map = {}
    for s in config.secrets:
        name = s.get("name", "")
        secrets_map[name] = {
            "required": s.get("required", False),
            "description": s.get("description", ""),
        }

    manifest: Dict[str, Any] = {
        "api_version": "criteria.dev/v2",
        "kind": "Adapter",
        "metadata": {
            "name": config.name,
            "version": config.version,
        },
        "spec": {
            "description": config.description,
            "source_url": config.source_url,
            "capabilities": config.capabilities,
            "platforms": config.platforms,
            "config_schema": _schema_proto_to_dict(config.config_schema),
            "input_schema": _schema_proto_to_dict(config.input_schema),
            "output_schema": _schema_proto_to_dict(config.output_schema),
            "secrets": secrets_map,
            "permissions": config.permissions,
            "compatible_environments": config.compatible_environments,
            "container_image": config.container_image,
            "supported_features": config.supported_features,
            "max_chunk_bytes": config.max_chunk_bytes,
        },
    }
    print(yaml.dump(manifest, sort_keys=False))
    return 0


def _schema_proto_to_dict(schema: Optional[adapter_pb2.AdapterSchemaProto]) -> Optional[Dict[str, Any]]:
    if schema is None:
        return None
    out: Dict[str, Any] = {}
    for key, field_proto in schema.fields.items():
        out[key] = {
            "type": field_proto.type,
            "required": field_proto.required,
            "description": field_proto.description,
            "default": field_proto.default_str,
            "sensitive": field_proto.sensitive,
        }
    return out


class _AdapterServicer(adapter_pb2_grpc.AdapterServiceServicer):
    def __init__(self, config: ServeConfig):
        self._config = config
        self._sessions: Dict[str, Helpers] = {}

    @staticmethod
    def _run_handler(handler_result: Any) -> Any:
        """Await a coroutine if the handler is async, otherwise return directly."""
        if inspect.isawaitable(handler_result):
            return asyncio.run(handler_result)
        return handler_result

    def Info(self, request, context):
        resp = adapter_pb2.InfoResponse(
            name=self._config.name,
            version=self._config.version,
            description=self._config.description,
            capabilities=self._config.capabilities,
            platforms=self._config.platforms,
            sdk_protocol_version="2",
            source_url=self._config.source_url,
            config_schema=self._config.config_schema,
            input_schema=self._config.input_schema,
            output_schema=self._config.output_schema,
            permissions=self._config.permissions,
            compatible_environments=self._config.compatible_environments,
            container_image=self._config.container_image,
            supported_features=self._config.supported_features,
            max_chunk_bytes=self._config.max_chunk_bytes,
        )
        for s in self._config.secrets:
            name = s.get("name", "")
            desc = s.get("description", "")
            resp.secrets[name] = desc
        return resp

    def OpenSession(self, request, context):
        sid = request.session_id
        secrets = dict(request.secrets)
        config = dict(request.config)
        helpers = Helpers(
            session_id=sid,
            secrets_map=secrets,
            config=config,
            allowed_outcomes=list(request.allowed_outcomes),
        )
        self._sessions[sid] = helpers
        if self._config.open_session is not None:
            try:
                self._run_handler(self._config.open_session(sid, config, helpers))
            except Exception as e:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(str(e))
                return adapter_pb2.OpenSessionResponse()
        return adapter_pb2.OpenSessionResponse()

    def Execute(self, request, context):
        sid = request.session_id
        helpers = self._sessions.get(sid)
        if helpers is None:
            context.set_code(grpc.StatusCode.FAILED_PRECONDITION)
            context.set_details("session not found")
            return
        if self._config.execute is None:
            context.set_code(grpc.StatusCode.UNIMPLEMENTED)
            context.set_details("execute not implemented")
            return

        # Merge execute-time secret_inputs into the session secrets
        for k, v in request.secret_inputs.items():
            helpers.secrets._secrets[k] = v

        try:
            result = self._run_handler(self._config.execute(request, helpers))
        except Exception as e:
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(str(e))
            return

        # Flush any log events buffered during the handler
        for ev in helpers.log._flush():
            yield ev

        # Normalise handler result into an ExecuteResult proto
        if result is None:
            yield adapter_pb2.ExecuteEvent(
                result=adapter_pb2.ExecuteResult(outcome="success")
            )
        elif isinstance(result, adapter_pb2.ExecuteResult):
            yield adapter_pb2.ExecuteEvent(result=result)
        elif isinstance(result, dict):
            outcome = result.get("outcome", "success")
            outputs_json = b""
            if "output" in result:
                import json
                outputs_json = json.dumps(result["output"]).encode("utf-8")
            yield adapter_pb2.ExecuteEvent(
                result=adapter_pb2.ExecuteResult(outcome=outcome, outputs_json=outputs_json)
            )
        else:
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(f"handler returned unexpected type {type(result)}")
            return

    def Log(self, request, context):
        sid = request.session_id
        helpers = self._sessions.get(sid)
        if helpers is None:
            context.set_code(grpc.StatusCode.FAILED_PRECONDITION)
            context.set_details("session not found")
            return
        if self._config.log is not None:
            try:
                self._run_handler(self._config.log(sid, request.step_name, helpers))
            except Exception as e:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(str(e))
                return
        # TODO: yield actual log events when stream-based logging is implemented.
        return

    def Permissions(self, request_iterator, context):
        if self._config.permissions_handler is not None:
            try:
                yield from self._config.permissions_handler(request_iterator, context)
                return
            except Exception as e:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(str(e))
                return

        # Default permissive handler: allow all permission requests
        for ev in request_iterator:
            req = ev.request
            if req.request_id:
                yield adapter_pb2.PermissionDecision(
                    request_id=req.request_id,
                    decision="allow",
                )

    def Pause(self, request, context):
        return adapter_pb2.PauseResponse()

    def Resume(self, request, context):
        return adapter_pb2.ResumeResponse()

    def Snapshot(self, request, context):
        sid = request.session_id
        helpers = self._sessions.get(sid)
        if self._config.snapshot is not None and helpers is not None:
            try:
                state = self._run_handler(self._config.snapshot(sid, helpers))
                return adapter_pb2.SnapshotResponse(state=state, schema_version=1)
            except Exception as e:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(str(e))
                return adapter_pb2.SnapshotResponse(state=b"", schema_version=1)
        return adapter_pb2.SnapshotResponse(state=b"", schema_version=1)

    def Restore(self, request, context):
        sid = request.session_id
        helpers = self._sessions.get(sid)
        if self._config.restore is not None and helpers is not None:
            try:
                self._run_handler(self._config.restore(sid, request.state, helpers))
            except Exception as e:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(str(e))
                return adapter_pb2.RestoreResponse()
        return adapter_pb2.RestoreResponse()

    def Inspect(self, request, context):
        sid = request.session_id
        helpers = self._sessions.get(sid)
        if self._config.inspect is not None and helpers is not None:
            try:
                return self._run_handler(self._config.inspect(sid, helpers))
            except Exception as e:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(str(e))
                return adapter_pb2.InspectResponse()
        return adapter_pb2.InspectResponse()

    def CloseSession(self, request, context):
        sid = request.session_id
        helpers = self._sessions.pop(sid, None)
        if self._config.close_session is not None and helpers is not None:
            try:
                self._run_handler(self._config.close_session(sid, helpers))
            except Exception as e:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(str(e))
                return adapter_pb2.CloseSessionResponse()
        return adapter_pb2.CloseSessionResponse()


def serve(config: Union[dict, ServeConfig]) -> int:
    """Start the adapter in local (go-plugin) mode.

    Accepts either a ``ServeConfig`` dataclass or a plain ``dict`` (matching the
    workstream specification).  When a dict is passed it is converted to
    ``ServeConfig`` automatically.

    Checks the magic-cookie environment variable, starts a gRPC server on a
    Unix socket, prints the go-plugin protocol line to stdout, and blocks
    until the host disconnects.

    Returns the process exit code.
    """
    if isinstance(config, dict):
        config = ServeConfig(**config)

    if len(sys.argv) > 1 and sys.argv[1] == "--emit-manifest":
        return _emit_manifest(config)

    # go-plugin handshake check
    if os.environ.get(_MAGIC_COOKIE_KEY) != _MAGIC_COOKIE_VALUE:
        print(
            f"criteria_adapter_sdk: {_MAGIC_COOKIE_KEY} mismatch; "
            "this binary must be launched by the Criteria host",
            file=sys.stderr,
        )
        return 1

    socket_path = os.path.join(
        tempfile.gettempdir(),
        f"criteria-py-adapter-{os.getpid()}.sock",
    )

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    adapter_pb2_grpc.add_AdapterServiceServicer_to_server(
        _AdapterServicer(config), server
    )
    server.add_insecure_port(f"unix://{socket_path}")
    server.start()

    # go-plugin protocol line: core_version|proto_version|network|address|type|cert
    protocol_line = (
        f"{_GO_PLUGIN_CORE_VERSION}|{_GO_PLUGIN_PROTOCOL_VERSION}|"
        f"unix|{socket_path}|grpc|"
    )
    print(protocol_line)
    sys.stdout.flush()

    try:
        server.wait_for_termination()
    finally:
        server.stop(0)
        try:
            os.unlink(socket_path)
        except FileNotFoundError:
            pass

    return 0
