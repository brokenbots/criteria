"""TestHost harness for unit-testing adapter implementations.

Provides `TestHost`, a lightweight in-process host that can drive any
`ServeConfig`-style adapter directly, without spawning a subprocess or
going through the full go-plugin handshake.

Usage (programmatic):

    from criteria_adapter_sdk.testing import TestHost
    from my_adapter import serve_config

    host = TestHost(serve_config)
    host.open_session(session_id="test-1")
    result = host.execute(step_name="step-1", config={"model": "gpt-4"})
    print(result.outcome, result.output)

Usage (CLI):

    python -m criteria_adapter_sdk.testing my_adapter:config
"""

import argparse
import asyncio
import importlib
import inspect
import json
import sys
import traceback
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

from criteria.v2 import adapter_pb2

from .helpers import Helpers, LogSender, SecretsHelper
from .schema import pydantic_to_schema, dict_to_schema_proto


def _maybe_await(result: Any) -> Any:
    """Await a coroutine if needed, otherwise return the value directly."""
    if inspect.isawaitable(result):
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return asyncio.run(result)
        return loop.run_until_complete(result)
    return result


@dataclass
class ExecuteResult:
    """Result returned by TestHost.execute()."""

    outcome: str = ""
    output: Dict[str, Any] = field(default_factory=dict)
    logs: List[str] = field(default_factory=list)
    permission_requests: List[Dict[str, str]] = field(default_factory=list)


class _NoOpLogSender(LogSender):
    """Log sender that collects messages in memory instead of a stream."""

    def __init__(self):
        self.lines: List[str] = []

    def stdout(self, line: str) -> None:
        self.lines.append(f"[stdout] {line}")

    def stderr(self, line: str) -> None:
        self.lines.append(f"[stderr] {line}")

    def agent(self, line: str) -> None:
        self.lines.append(f"[agent] {line}")


class _FakePermissions:
    """Fake permission correlator that records requests but always allows."""

    def __init__(self, collector: List[Dict[str, str]]):
        self._collector = collector

    def request(self, tool: str, args_digest: str = "", args_preview: str = "") -> str:
        self._collector.append({
            "tool": tool,
            "args_digest": args_digest,
            "args_preview": args_preview,
        })
        return "allow"


class AdapterTestHost:
    """In-process test harness for adapter handlers.

    Mimics the host side of the AdapterService gRPC contract by calling
    the user-supplied handler functions directly.
    """

    __test__ = False  # Prevent pytest from collecting this as a test class.

    def __init__(self, config: Any):
        """Create a TestHost wrapping a `ServeConfig`-style dict or dataclass.

        `config` must expose the same fields as `ServeConfig` — at minimum
        `execute` and `info` callables.
        """
        self._config = config
        self._sessions: Dict[str, Dict[str, Any]] = {}

    def info(self) -> adapter_pb2.InfoResponse:
        """Call the adapter info handler."""
        if hasattr(self._config, "info") and callable(self._config.info):
            return self._config.info()
        if isinstance(self._config, dict) and callable(self._config.get("info")):
            return self._config["info"]()
        # Build a default info proto from config metadata.
        info = adapter_pb2.InfoResponse()
        for attr in ("name", "version", "source_url", "sdk_protocol_version"):
            val = getattr(self._config, attr, None)
            if val is None and isinstance(self._config, dict):
                val = self._config.get(attr)
            if val is not None:
                setattr(info, attr, val)
        caps = getattr(self._config, "capabilities", None) or self._config.get("capabilities", [])
        info.capabilities.extend(caps)
        platforms = getattr(self._config, "platforms", None) or self._config.get("platforms", [])
        info.platforms.extend(platforms)
        perms = getattr(self._config, "permissions", None) or self._config.get("permissions", [])
        info.permissions.extend(perms)
        # Schema helpers
        for schema_attr in ("config_schema", "input_schema", "output_schema"):
            schema_val = getattr(self._config, schema_attr, None)
            if schema_val is None and isinstance(self._config, dict):
                schema_val = self._config.get(schema_attr)
            if schema_val is not None:
                if hasattr(info, schema_attr):
                    getattr(info, schema_attr).CopyFrom(schema_val)
        # Secrets
        secrets = getattr(self._config, "secrets", None) or self._config.get("secrets", [])
        for s in secrets:
            if isinstance(s, dict):
                info.secrets[s.get("name", "")] = ""
        return info

    def open_session(
        self,
        session_id: str,
        run_id: str = "test-run",
        trace_id: str = "test-trace",
        user_id: str = "test-user",
        config: Optional[Dict[str, Any]] = None,
        secrets: Optional[Dict[str, str]] = None,
        step_definitions: Optional[List[Any]] = None,
    ) -> None:
        """Open a test session."""
        cfg = config or {}
        sec = secrets or {}
        self._sessions[session_id] = {
            "run_id": run_id,
            "trace_id": trace_id,
            "user_id": user_id,
            "config": cfg,
            "secrets": sec,
            "steps": step_definitions or [],
        }
        handler = getattr(self._config, "open_session", None) or self._config.get("open_session")
        if handler is not None:
            req = adapter_pb2.OpenSessionRequest(
                session_id=session_id,
                allowed_outcomes=[],
            )
            if cfg:
                for k, v in cfg.items():
                    req.config[k] = str(v)
            for k, v in sec.items():
                req.secrets[k] = str(v)
            _maybe_await(handler(req, None))

    def execute(
        self,
        session_id: str,
        step_name: str,
        config: Optional[Dict[str, Any]] = None,
        input_data: Optional[Dict[str, Any]] = None,
    ) -> ExecuteResult:
        """Execute a step in the given session and return the result."""
        session = self._sessions.get(session_id)
        if session is None:
            raise RuntimeError(f"session {session_id!r} not open; call open_session first")

        handler = getattr(self._config, "execute", None) or self._config.get("execute")
        if handler is None:
            raise RuntimeError("config has no execute handler")

        merged_config = dict(session["config"])
        if config:
            merged_config.update(config)

        log_sender = _NoOpLogSender()
        permissions_collector: List[Dict[str, str]] = []
        secrets_helper = SecretsHelper(session["secrets"])
        helpers = Helpers(
            session_id=session_id,
            config=merged_config,
            secrets_map=session["secrets"],
            allowed_outcomes=getattr(self._config, "allowed_outcomes", []) or self._config.get("allowed_outcomes", []),
        )
        helpers._log_sender = log_sender
        helpers._permission_correlator = _FakePermissions(permissions_collector)
        helpers._secrets_helper = secrets_helper

        req = adapter_pb2.ExecuteRequest(
            session_id=session_id,
            step_name=step_name,
        )
        if merged_config:
            for k, v in merged_config.items():
                req.input[k] = str(v)
        if input_data:
            for k, v in input_data.items():
                req.input[k] = str(v)
        for outcome in helpers.allowed_outcomes:
            req.allowed_outcomes.append(outcome)
        for k, v in session["secrets"].items():
            req.secret_inputs[k] = str(v)

        try:
            result = _maybe_await(handler(req, helpers))
        except Exception as exc:
            traceback.print_exc()
            return ExecuteResult(outcome="error", output={"error": str(exc)}, logs=log_sender.lines, permission_requests=permissions_collector)

        if isinstance(result, adapter_pb2.ExecuteResult):
            outcome = result.outcome or ""
            output = json.loads(result.outputs_json) if result.outputs_json else {}
            return ExecuteResult(outcome=outcome, output=output, logs=log_sender.lines, permission_requests=permissions_collector)

        # If user returned a plain dict, normalise it.
        if isinstance(result, dict):
            outcome = result.get("outcome", "")
            output = result.get("output", {})
            return ExecuteResult(outcome=outcome, output=output, logs=log_sender.lines, permission_requests=permissions_collector)

        raise TypeError(f"execute handler returned unexpected type {type(result)}")

    def close_session(self, session_id: str) -> None:
        """Close a test session."""
        handler = getattr(self._config, "close_session", None) or self._config.get("close_session")
        if handler is not None:
            req = adapter_pb2.CloseSessionRequest(session_id=session_id)
            _maybe_await(handler(req, None))
        self._sessions.pop(session_id, None)


def _load_config(ref: str) -> Any:
    """Load a ServeConfig from a module:attribute reference."""
    if ":" not in ref:
        raise ValueError(f"config reference must be 'module:attribute', got {ref!r}")
    module_name, attr_name = ref.split(":", 1)
    mod = importlib.import_module(module_name)
    return getattr(mod, attr_name)


def _main() -> None:
    parser = argparse.ArgumentParser(description="criteria-py-adapter-test CLI")
    parser.add_argument("config", help="module:attribute reference to a ServeConfig")
    parser.add_argument("--session-id", default="test-session")
    parser.add_argument("--step-name", default="test-step")
    parser.add_argument("--config-json", default="{}")
    parser.add_argument("--input-json", default="{}")
    args = parser.parse_args()

    cfg = _load_config(args.config)
    host = AdapterTestHost(cfg)
    host.open_session(session_id=args.session_id)
    result = host.execute(
        session_id=args.session_id,
        step_name=args.step_name,
        config=json.loads(args.config_json),
        input_data=json.loads(args.input_json),
    )
    print(json.dumps({"outcome": result.outcome, "output": result.output, "logs": result.logs}, indent=2))
    host.close_session(args.session_id)


# Backward-compatible alias used by the initial implementation.
TestHost = AdapterTestHost


if __name__ == "__main__":
    _main()
