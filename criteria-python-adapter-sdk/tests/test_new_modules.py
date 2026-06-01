import json
import sys

import pytest
from pydantic import BaseModel

from criteria.v2 import adapter_pb2

from criteria_adapter_sdk.schema import pydantic_to_schema, dict_to_schema_proto
from criteria_adapter_sdk.helpers import (
    Helpers,
    SecretsHelper,
    OutcomeValidator,
    PermissionCorrelator,
    SessionStore,
    TimestampHelper,
)
from criteria_adapter_sdk.serve import ServeConfig
from criteria_adapter_sdk.testing import TestHost, _NoOpLogSender, _FakePermissions
from criteria_adapter_sdk.library_mode import run_in_process


# ---------------------------------------------------------------------------
# Schema tests
# ---------------------------------------------------------------------------

class SimpleModel(BaseModel):
    name: str
    count: int
    enabled: bool
    ratio: float


class NestedModel(BaseModel):
    title: str
    simple: SimpleModel


class OptionalModel(BaseModel):
    required_field: str
    optional_field: str = "default"


class ListModel(BaseModel):
    tags: list[str]


def test_pydantic_to_schema_simple():
    schema = pydantic_to_schema(SimpleModel)
    assert "name" in schema.fields
    assert schema.fields["name"].type == "string"
    assert schema.fields["name"].required is True

    assert "count" in schema.fields
    assert schema.fields["count"].type == "number"
    assert schema.fields["count"].required is True

    assert "enabled" in schema.fields
    assert schema.fields["enabled"].type == "bool"

    assert "ratio" in schema.fields
    assert schema.fields["ratio"].type == "number"


def test_pydantic_to_schema_nested():
    schema = pydantic_to_schema(NestedModel)
    assert schema.fields["title"].type == "string"
    # nested models map to "object"
    assert schema.fields["simple"].type == "object"


def test_pydantic_to_schema_optional():
    schema = pydantic_to_schema(OptionalModel)
    assert schema.fields["required_field"].required is True
    # optional with default — pydantic reports not required
    assert schema.fields["optional_field"].required is False


def test_pydantic_to_schema_list_string():
    schema = pydantic_to_schema(ListModel)
    assert schema.fields["tags"].type == "list_string"


def test_pydantic_to_schema_rejects_non_model():
    with pytest.raises(TypeError):
        pydantic_to_schema(str)


def test_dict_to_schema_proto():
    schema = dict_to_schema_proto({
        "api_key": {"type": "string", "required": True, "sensitive": True},
        "timeout": {"type": "number", "required": False, "default": 30},
    })
    assert schema is not None
    assert schema.fields["api_key"].type == "string"
    assert schema.fields["api_key"].required is True
    assert schema.fields["api_key"].sensitive is True
    assert schema.fields["timeout"].type == "number"
    assert schema.fields["timeout"].required is False


def test_dict_to_schema_proto_none():
    assert dict_to_schema_proto(None) is None


# ---------------------------------------------------------------------------
# Helpers tests
# ---------------------------------------------------------------------------

def test_secrets_helper_get():
    s = SecretsHelper({"FOO": "bar"})
    assert s.get("FOO") == "bar"
    assert s.get("MISSING") is None


def test_secrets_helper_spawn_env():
    s = SecretsHelper({"A": "1", "B": "2"})
    env = s.spawn_env(["A", "C"])
    assert env == {"A": "1"}
    assert "B" not in env
    assert "C" not in env


def test_outcome_validator():
    v = OutcomeValidator(["success", "failure"])
    assert v.validate("success") == "success"
    with pytest.raises(ValueError):
        v.validate("unknown")


def test_permission_correlator():
    p = PermissionCorrelator()
    assert p.request("tool", "digest", "preview") == "allow"


def test_session_store():
    store = SessionStore()
    assert store.get("x") is None
    store.set("x", 42)
    assert store.get("x") == 42


def test_timestamp_helper():
    t = TimestampHelper()
    assert t.elapsed_ms() >= 0
    assert t.now() > 0


def test_helpers_bundle():
    h = Helpers(session_id="s1", config={"k": "v"}, secrets_map={"SK": "secret"})
    assert h.session.get("x") is None
    h.session.set("x", 1)
    assert h.session.get("x") == 1
    assert h.secrets.get("SK") == "secret"
    assert h.outcomes.validate("success") == "success"
    assert h.permission.request("t") == "allow"
    assert h.timestamps.elapsed_ms() >= 0


def test_log_sender():
    l = _NoOpLogSender()
    l.stdout("hello")
    l.stderr("err")
    l.agent("msg")
    assert len(l.lines) == 3
    assert "[stdout] hello" in l.lines


def test_fake_permissions():
    col: list = []
    fp = _FakePermissions(col)
    assert fp.request("tool", "d", "p") == "allow"
    assert len(col) == 1
    assert col[0]["tool"] == "tool"


# ---------------------------------------------------------------------------
# TestHost tests
# ---------------------------------------------------------------------------

def _make_config(**kwargs):
    defaults = {
        "name": "test-adapter",
        "version": "1.0.0",
        "capabilities": ["multi_turn"],
        "platforms": ["linux/amd64"],
        "permissions": ["read_file"],
        "config_schema": None,
        "input_schema": None,
        "output_schema": None,
        "secrets": [{"name": "API_KEY", "required": True}],
        "execute": lambda req, helpers: adapter_pb2.ExecuteResult(outcome="success", outputs_json=b'{"ok": true}'),
    }
    defaults.update(kwargs)
    return defaults


def test_testhost_info():
    cfg = _make_config()
    host = TestHost(cfg)
    info = host.info()
    assert info.name == "test-adapter"
    assert info.version == "1.0.0"
    assert list(info.capabilities) == ["multi_turn"]
    assert list(info.platforms) == ["linux/amd64"]
    assert list(info.permissions) == ["read_file"]
    assert len(info.secrets) == 1
    assert "API_KEY" in info.secrets
    assert info.secrets["API_KEY"] == ""


def test_testhost_execute_success():
    cfg = _make_config()
    host = TestHost(cfg)
    host.open_session(session_id="s1")
    result = host.execute(session_id="s1", step_name="step1")
    assert result.outcome == "success"
    assert result.output == {"ok": True}
    host.close_session("s1")


def test_testhost_execute_custom_handler():
    def handler(req, helpers):
        return {"outcome": "done", "output": {"msg": "hello"}}

    cfg = _make_config(execute=handler)
    host = TestHost(cfg)
    host.open_session(session_id="s2")
    result = host.execute(session_id="s2", step_name="step1")
    assert result.outcome == "done"
    assert result.output == {"msg": "hello"}


def test_testhost_execute_exception():
    def bad_handler(req, helpers):
        raise RuntimeError("boom")

    cfg = _make_config(execute=bad_handler)
    host = TestHost(cfg)
    host.open_session(session_id="s3")
    result = host.execute(session_id="s3", step_name="step1")
    assert result.outcome == "error"
    assert "boom" in result.output["error"]


def test_testhost_missing_session():
    cfg = _make_config()
    host = TestHost(cfg)
    with pytest.raises(RuntimeError, match="not open"):
        host.execute(session_id="missing", step_name="x")


# ---------------------------------------------------------------------------
# Library mode tests
# ---------------------------------------------------------------------------

def test_library_mode_success():
    def handler(req, helpers):
        return adapter_pb2.ExecuteResult(outcome="ok", outputs_json=b'{"x": 1}')

    cfg = _make_config(execute=handler)
    resp = run_in_process(cfg, step_name="s1")
    assert resp.outcome == "ok"
    assert json.loads(resp.outputs_json) == {"x": 1}


def test_library_mode_dict_result():
    def handler(req, helpers):
        return {"outcome": "done", "output": {"y": 2}}

    cfg = _make_config(execute=handler)
    resp = run_in_process(cfg, step_name="s2")
    assert resp.outcome == "done"
    assert json.loads(resp.outputs_json) == {"y": 2}


# ---------------------------------------------------------------------------
# ServeConfig / serve smoke tests
# ---------------------------------------------------------------------------

def test_serve_config_defaults():
    cfg = ServeConfig(name="a", version="1", execute=lambda r, h: None)
    assert cfg.name == "a"
    assert cfg.version == "1"
    assert cfg.execute is not None
    assert cfg.capabilities == []
    assert cfg.platforms == []
    assert cfg.permissions == []


# ---------------------------------------------------------------------------
# --emit-manifest smoke test via stdout capture
# ---------------------------------------------------------------------------

def test_emit_manifest(monkeypatch, capsys):
    import json as json_mod

    cfg = ServeConfig(
        name="manifest-adapter",
        version="2.0.0",
        source_url="https://example.com",
        capabilities=["tool_calling"],
        platforms=["linux/amd64"],
        permissions=["read_file"],
        secrets=[{"name": "KEY", "required": True}],
        execute=lambda r, h: None,
    )

    monkeypatch.setattr(sys, "argv", ["prog", "--emit-manifest"])
    # Import serve locally so the monkeypatch is active.
    from criteria_adapter_sdk.serve import serve

    # --emit-manifest prints YAML and returns 0.
    code = serve(cfg)
    assert code == 0
    captured = capsys.readouterr()
    assert "manifest-adapter" in captured.out
    assert "2.0.0" in captured.out
    # The emit-manifest prints YAML; we can verify by key presence
    assert "name: manifest-adapter" in captured.out
    assert "capabilities:" in captured.out
    assert "permissions:" in captured.out
