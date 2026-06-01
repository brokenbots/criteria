"""SDK helpers for adapter authors.

Each handler receives a `Helpers` instance with:

- `session` — per-session keyed get/set
- `outcomes` — outcome validation
- `permission` — request/deny permission correlation
- `log` — structured log sender
- `secrets` — secret-channel-only access + spawn_env helper
- `timestamps` — monotonic timestamps
"""

import time
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

from criteria.v2 import adapter_pb2


@dataclass
class SessionStore:
    """Per-session keyed get/set."""

    _data: Dict[str, Any] = field(default_factory=dict)

    def get(self, key: str, default: Any = None) -> Any:
        return self._data.get(key, default)

    def set(self, key: str, value: Any) -> None:
        self._data[key] = value


@dataclass
class OutcomeValidator:
    """Validate outcome strings against the allowed set."""

    allowed: List[str] = field(default_factory=list)

    def validate(self, outcome: str) -> str:
        if self.allowed and outcome not in self.allowed:
            raise ValueError(
                f"outcome {outcome!r} is not in allowed outcomes {self.allowed}"
            )
        return outcome


@dataclass
class PermissionCorrelator:
    """Request permission and wait for a host decision.

    In the current sync implementation this is a stub that auto-allows.
    Future versions may implement async blocking correlation.
    """

    _pending: Dict[str, Any] = field(default_factory=dict)

    def request(self, tool: str, args_digest: str = "", args_preview: str = "") -> str:
        """Request permission for a tool call. Returns 'allow' or 'deny'."""
        # Stub: always allow. Full correlation requires async bidi stream support.
        return "allow"


@dataclass
class LogSender:
    """Send log events to the host via the Execute RPC stream.

    Log events are buffered while the handler runs and flushed at the end of
    the ``Execute`` call.  Future versions may support real-time streaming.
    """

    _session_id: str = ""
    _step_name: str = ""
    _stream: Optional[Any] = None  # grpc stream context (reserved for future)
    _buffer: List[adapter_pb2.ExecuteEvent] = field(default_factory=list)

    def stdout(self, line: str) -> None:
        """Send a stdout line."""
        self._send("stdout", line.encode("utf-8"))

    def stderr(self, line: str) -> None:
        """Send a stderr line."""
        self._send("stderr", line.encode("utf-8"))

    def agent(self, line: str) -> None:
        """Send an agent log line."""
        self._send("agent", line.encode("utf-8"))

    def adapter_event(self, kind: str, payload: Dict[str, Any]) -> None:
        """Emit a structured adapter event."""
        import json

        data = json.dumps({"kind": kind, "payload": payload}).encode("utf-8")
        self._buffer.append(
            adapter_pb2.ExecuteEvent(
                adapter=adapter_pb2.AdapterEvent(
                    event_kind="adapter",
                    payload_json=data,
                )
            )
        )

    def _send(self, stream_name: str, data: bytes) -> None:
        import json

        payload = json.dumps({"stream": stream_name, "line": data.decode("utf-8", "replace")}).encode("utf-8")
        self._buffer.append(
            adapter_pb2.ExecuteEvent(
                adapter=adapter_pb2.AdapterEvent(
                    event_kind="log",
                    payload_json=payload,
                )
            )
        )

    def _flush(self) -> List[adapter_pb2.ExecuteEvent]:
        """Drain the buffer and return pending events."""
        events = self._buffer[:]
        self._buffer.clear()
        return events


@dataclass
class SecretsHelper:
    """Secret-channel-only secret access and spawn_env builder."""

    _secrets: Dict[str, str] = field(default_factory=dict)

    async def get(self, name: str) -> Optional[str]:
        """Return the secret value for `name` or None.

        There is no env-var fallback — secret values come exclusively
        from the secret channel (OpenSessionRequest.secrets or
        ExecuteRequest.secret_inputs).
        """
        return self._secrets.get(name)

    def spawn_env(self, names: List[str]) -> Dict[str, str]:
        """Build an env-map safe for `subprocess.Popen(env=...)`.

        Only the requested secret names are included; all other env vars
        are stripped so the child process cannot inherit ambient secrets.
        """
        env: Dict[str, str] = {}
        for name in names:
            val = self._secrets.get(name)
            if val is not None:
                env[name] = val
        return env


@dataclass
class TimestampHelper:
    """Monotonic timestamps for events."""

    _start: float = field(default_factory=time.monotonic)

    def elapsed_ms(self) -> int:
        return int((time.monotonic() - self._start) * 1000)

    def now(self) -> float:
        return time.monotonic()


@dataclass
class Helpers:
    """Bundle of convenience helpers passed to adapter handlers."""

    session_id: str = ""
    config: Dict[str, str] = field(default_factory=dict)
    secrets_map: Dict[str, str] = field(default_factory=dict)
    allowed_outcomes: List[str] = field(default_factory=list)
    _session_store: SessionStore = field(default_factory=SessionStore)
    _outcome_validator: Optional[OutcomeValidator] = None
    _permission_correlator: PermissionCorrelator = field(
        default_factory=PermissionCorrelator
    )
    _log_sender: LogSender = field(default_factory=LogSender)
    _secrets_helper: Optional[SecretsHelper] = None
    _timestamp_helper: TimestampHelper = field(default_factory=TimestampHelper)

    def __post_init__(self) -> None:
        if self._outcome_validator is None:
            self._outcome_validator = OutcomeValidator(self.allowed_outcomes)
        if self._secrets_helper is None:
            self._secrets_helper = SecretsHelper(self.secrets_map)

    @property
    def session(self) -> SessionStore:
        return self._session_store

    @property
    def outcomes(self) -> OutcomeValidator:
        return self._outcome_validator

    @property
    def permission(self) -> PermissionCorrelator:
        return self._permission_correlator

    @property
    def log(self) -> LogSender:
        return self._log_sender

    @property
    def secrets(self) -> SecretsHelper:
        return self._secrets_helper

    @property
    def timestamps(self) -> TimestampHelper:
        return self._timestamp_helper
