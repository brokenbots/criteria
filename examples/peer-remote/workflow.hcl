# Peer-mode remote adapter demo (T-10 / KB-20).
#
# Two adapter peers (shell + noop) dial the host's remote shim and execute
# steps under per-scope sessions: each adapter instance gets its own rotated
# accept token and scope key, so no peer can present another scope's
# credentials. See README.md for bring-up steps, the structured-log tour, and
# the kill -9 crash-diagnosis demo.
#
# mode: standalone
workflow {
  name = "peer-remote-demo"
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"
  policy {
    max_total_steps = 20
  }
}

# The remote shim listens for phone-home connections. per_scope_sessions
# rotates a fresh accept token per adapter instance (written under the run
# data directory and announced via the provision_wanted lifecycle event);
# the static accept_token is kept only for legacy runner dials and is
# ignored when per_scope_sessions is enabled.
environment "remote" "default" {
  listen_address     = "0.0.0.0:7778"
  accept_token       = "change-me-legacy-token"
  per_scope_sessions = true
}

adapter "shell" "main" {
  source  = "ghcr.io/brokenbots/criteria-adapter-shell"
  version = "0.5.3"
  environment = remote.default
  # Crash policy: when the adapter session crashes mid-step (e.g. the peer's
  # adapter child is SIGKILLed), re-open the session and retry the step.
  on_crash = "respawn"
}

adapter "noop" "gate" {
  source  = "ghcr.io/brokenbots/criteria-adapter-noop"
  version = "0.5.2"
  environment = remote.default
}

# Success path: runs on the shell peer, output streams back to the host.
step "greet" {
  target = adapter.shell.main
  input {
    command = "echo hello from the shell peer"
  }

  outcome "success" { next = step.deliberate_failure }
  outcome "failure" { next = state.failed }
}

# Failure path: the adapter reports a failure outcome (exit 3) and the
# workflow ROUTES it — adapter failure is a result, not a crash. The
# recovery step shows the run completing successfully afterwards.
step "deliberate_failure" {
  target = adapter.shell.main
  input {
    command = "exit 3"
  }

  outcome "failure" { next = step.recover }
  outcome "success" { next = state.failed }
}

step "recover" {
  target = adapter.shell.main
  input {
    command = "echo recovered from the deliberate failure"
  }

  outcome "success" { next = step.crash_demo }
  outcome "failure" { next = state.failed }
}

# Crash demo: this step sleeps so an operator can SIGKILL the peer's adapter
# child mid-flight (see README.md). The host classifies the crash from the
# peer's supervision journal and respawns the session; restarting the peer
# container lets the retry complete.
step "crash_demo" {
  target = adapter.shell.main
  input {
    command = "sleep 300"
    timeout = "2m"
  }

  outcome "success" { next = step.isolation_gate }
  outcome "failure" { next = state.failed }
}

# Multi-adapter: a second adapter type on a second peer. Each adapter type
# runs in its own per-scope session with its own rotated token.
step "isolation_gate" {
  target = adapter.noop.gate
  input {
    delay_ms = "500"
  }

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}

output "summary" {
  type        = string
  description = "Demo completion marker"
  value       = "peer-remote-demo complete"
}

# NOTE: wait { ... } and approval { ... } nodes are not exercised here — they
# require a server-compatible orchestrator (criteria apply --server ...); a
# local-only run rejects those node kinds.