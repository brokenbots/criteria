"""Library mode — direct handler invocation without process spawn.

In library mode the adapter code is imported directly into the host
process (e.g. a Python orchestrator) and driven via a lightweight
in-process wrapper rather than the go-plugin protocol.

Usage:

    from criteria_adapter_sdk.library_mode import run_in_process
    from my_adapter import serve_config

    result = run_in_process(serve_config, session_id="sess-1", step_name="step-1")
"""

import json
from typing import Any, Dict, Optional

from criteria.v2 import adapter_pb2

from .helpers import Helpers, SecretsHelper
from .testing import TestHost


def run_in_process(
    config: Any,
    session_id: str = "lib-session",
    run_id: str = "lib-run",
    trace_id: str = "lib-trace",
    step_name: str = "lib-step",
    step_config: Optional[Dict[str, Any]] = None,
    step_input: Optional[Dict[str, Any]] = None,
    secrets: Optional[Dict[str, str]] = None,
) -> adapter_pb2.ExecuteResult:
    """Execute a single step in library mode and return the gRPC result.

    This is the simplest way to call an adapter directly from Python code
    without spawning a subprocess or standing up a gRPC server.
    """
    host = TestHost(config)
    host.open_session(
        session_id=session_id,
        run_id=run_id,
        trace_id=trace_id,
        secrets=secrets,
    )
    result = host.execute(
        session_id=session_id,
        step_name=step_name,
        config=step_config,
        input_data=step_input,
    )
    host.close_session(session_id)

    return adapter_pb2.ExecuteResult(
        outcome=result.outcome,
        outputs_json=json.dumps(result.output).encode("utf-8") if result.output else b"",
    )


# Backward-compatible alias.
library_mode = run_in_process
