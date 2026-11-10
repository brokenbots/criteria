# KB-394 — ADR-0014 M8 canary: container-mode validation on the rolled engine.
#
# One green run whose event stream + operator logs carry, under CONTAINER
# hosting (engine-side in-process env host per ADR-0014 M3 / KB-378):
#   1. typed crash classification for a killed container-hosted child: the
#      in-process supervisor journal's CrashClassified is consumed as the
#      session's crash reason (never a string heuristic); on_crash = "respawn"
#      cycles the session from the journal into session.respawned and opens a
#      replacement container child, then retries the step green. A container
#      session has no pod recreate arm, so the declared policy owns the
#      outcome — here the declared policy is respawn.
#   2. declared fate at subworkflow re-entry for both fates: the fresh pair
#      releases the inherited engagement and opens a NEW conversation at every
#      re-entry; the reuse pair detaches and re-adopts the inherited
#      engagement, so the same conversation keeps serving the pair.
#
# The harness (../run-canary.sh) drives the container lane end to end:
# docker/podman runtime resolves the lockfile-pinned container image, the
# adapter child launches in-image, and the go-plugin identity handshake
# reaches the engine's in-process host — the same lane the KB-398 runner-side
# probe proved live. Mid-Execute the harness kills the crash_demo executor's
# container so the journal classification lands from the in-process
# supervisor, never a peer process.
#
# mode: standalone
workflow {
  name          = "kb-394-canary-container"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
  policy {
    max_total_steps = 60
  }
}

# The container environment binds every shell instance to the container lane:
# at session open the engine resolves the lockfile's container_image block,
# launches the adapter child in-image, and supervises it from the in-process
# env host (one child process per adapter definition).
# mode body intentionally empty — container images come from the lockfile.
environment "container" "e2e" {}

# Every shell instance runs the same LAN-registry shell adapter family; the
# lockfile pins the artifact digest plus its container image (ref + digest).
# The instances differ only in their declared behavior: crash policy and
# per-scope session fate.
adapter "shell" "crasher" {
  source      = "localhost:5000/criteria-adapter-shell"
  version     = "v0.5.6"
  environment = container.e2e
  # Crash policy: when the container-hosted child dies mid-step, re-open the
  # session from the in-process supervisor's journal classification (typed
  # CrashClassified) and retry the step. There is no pod recreate arm for
  # container sessions — the declared policy owns the outcome.
  on_crash = "respawn"
}

adapter "shell" "freshp" {
  source       = "localhost:5000/criteria-adapter-shell"
  version      = "v0.5.6"
  environment  = container.e2e
  continuation = "fresh"
}

adapter "shell" "reusep" {
  source       = "localhost:5000/criteria-adapter-shell"
  version      = "v0.5.6"
  environment  = container.e2e
  continuation = "reuse"
}

subworkflow "child-fresh" {
  source = "./child-fresh"
}

subworkflow "child-reuse" {
  source = "./child-reuse"
}

# Warm the crasher session before the crash demo. This opens the crasher
# definition's container child (one child per definition, spawned at first
# use) so the crash injection below has a live in-image child to kill.
step "start" {
  target = adapter.shell.crasher
  input {
    command = "echo kb-394 canary start: container session warm"
  }

  outcome "success" { next = step.crash_demo }
  outcome "failure" { next = state.failed }
}

# Crash demo: the harness kills the container hosting the crasher child while
# this step sleeps. The in-process supervisor journals the supervised exit,
# classifies it against the shared crash taxonomy (typed CrashClassified,
# never a heuristic), and the on_crash = "respawn" policy re-opens the
# session on a fresh container and retries the step. The sleep must finish
# well inside the step timeout so a timeout, not a crash, never short-circuits
# the tree.
step "crash_demo" {
  target = adapter.shell.crasher
  input {
    command = "sleep 12"
    # adapter-side input timeout; the engine-enforced bound is the step-level
    # `timeout` attribute (workflow/schema.go), kept far past the sleep so a
    # timeout never masks the injected crash.
    timeout = "2m"
  }

  outcome "success" { next = step.reentry-fresh-1 }
  outcome "failure" { next = state.failed }
}

# Declared fate, fresh pair: the parent speaks on its own engagement first so
# the child body re-declares an inherited session, then two re-entries of the
# same subworkflow. The child-fresh body declares the freshp pair with
# continuation = "fresh" on both sides (compile-enforced); every re-entry
# must release the inherited conversation and open a new one.
step "reentry-fresh-1" {
  target = adapter.shell.freshp
  input {
    command = "echo parent scope speaking before the fresh re-entry"
  }

  outcome "success" { next = step.sw-fresh-1 }
  outcome "failure" { next = state.failed }
}

step "sw-fresh-1" {
  target = subworkflow.child-fresh

  outcome "success" { next = step.sw-fresh-2 }
  outcome "failure" { next = state.failed }
}

step "sw-fresh-2" {
  target = subworkflow.child-fresh

  outcome "success" { next = step.reentry-reuse-1 }
  outcome "failure" { next = state.failed }
}

# Declared fate, reuse pair: two re-entries of the same subworkflow. The
# child-reuse body declares the reusep pair with continuation = "reuse" on
# both sides (compile-enforced); every re-entry must detach and re-adopt the
# inherited engagement, so the same conversation keeps serving the pair.
step "reentry-reuse-1" {
  target = adapter.shell.reusep
  input {
    command = "echo parent scope speaking before the reuse re-entry"
  }

  outcome "success" { next = step.sw-reuse-1 }
  outcome "failure" { next = state.failed }
}

step "sw-reuse-1" {
  target = subworkflow.child-reuse

  outcome "success" { next = step.sw-reuse-2 }
  outcome "failure" { next = state.failed }
}

step "sw-reuse-2" {
  target = subworkflow.child-reuse

  outcome "success" { next = step.done-marker }
  outcome "failure" { next = state.failed }
}

# Completion marker on the crasher: proves the respawned crasher session and
# both fate pairs all survived to the terminal of the same run.
step "done-marker" {
  target = adapter.shell.crasher
  input {
    command = "echo kb-394 canary complete: container lanes green"
  }

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}

output "summary" {
  type        = string
  description = "ADR-0014 M8 container-mode canary completion marker"
  value       = "kb-394-canary-container complete"
}
