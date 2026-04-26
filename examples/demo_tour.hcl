# Example: end-to-end "tour" workflow used by `make demo`.
#
# Showcases what the engine can do today (Phase 1.3): linear flow,
# FSM-based loops, branching, and a self-loop retry pattern. All
# steps use the shell adapter; richer agent steps land in 1.4.
#
# Total walltime ~30 s on a developer laptop. Tail it live in Parapet.
workflow "demo_tour" {
  version       = "0.1"
  initial_state = "boot"
  target_state  = "done"

  policy {
    max_total_steps = 40
  }

  # ---- Stage 1: setup -----------------------------------------------------

  step "boot" {
    adapter = "shell"
    input {
      command = "printf '=== Overlord demo tour ===\\n'; printf 'phase 1: setup\\n'; rm -f /tmp/overlord-demo-counter /tmp/overlord-demo-retry; echo 0 > /tmp/overlord-demo-counter; sleep 1; printf 'ready.\\n'"
    }
    timeout = "30s"
    outcome "success" { transition_to = "discover" }
    outcome "failure" { transition_to = "aborted" }
  }

  step "discover" {
    adapter = "shell"
    input {
      command = "printf 'phase 2: discovering work...\\n'; for t in alpha beta gamma; do printf '  -> queued task %s\\n' \"$t\"; sleep 0.4; done"
    }
    timeout = "30s"
    outcome "success" { transition_to = "work" }
    outcome "failure" { transition_to = "aborted" }
  }

  # ---- Stage 2: loop body -------------------------------------------------
  # Counter file drives the loop. Each `work` iteration increments it; the
  # `loop_check` step decides whether to loop back or move on.

  step "work" {
    adapter = "shell"
    input {
      command = "n=$(cat /tmp/overlord-demo-counter); n=$((n+1)); echo $n > /tmp/overlord-demo-counter; printf '\\n=== iteration %s ===\\n' \"$n\"; printf 'computing'; for i in 1 2 3; do printf '.'; sleep 0.3; done; printf '\\ncompleted iteration %s\\n' \"$n\""
    }
    timeout = "30s"
    outcome "success" { transition_to = "loop_check" }
    outcome "failure" { transition_to = "aborted" }
  }

  # The shell adapter maps exit code 0 -> "success" and non-zero -> "failure".
  # We co-opt that here as a branch: "success" loops back to `work`, "failure"
  # exits the loop. This pattern gets cleaner once 1.4 lands richer outcome
  # routing.
  step "loop_check" {
    adapter = "shell"
    input {
      command = "n=$(cat /tmp/overlord-demo-counter); if [ $n -lt 3 ]; then printf 'loop_check: n=%s < 3, looping\\n' \"$n\"; exit 0; else printf 'loop_check: n=%s, exiting loop\\n' \"$n\"; exit 9; fi"
    }
    timeout = "10s"
    outcome "success" { transition_to = "work" }     # loop
    outcome "failure" { transition_to = "verify" }   # done with loop
  }

  # ---- Stage 3: self-loop retry ------------------------------------------
  # First entry simulates a transient failure; second entry succeeds.
  # Demonstrates a step transitioning to itself on `failure`.

  step "verify" {
    adapter = "shell"
    input {
      command = "printf 'phase 3: verifying...\\n'; if [ ! -f /tmp/overlord-demo-retry ]; then touch /tmp/overlord-demo-retry; printf 'verify: simulated transient failure (will retry)\\n'; exit 1; else printf 'verify: ok\\n'; rm -f /tmp/overlord-demo-retry; fi"
    }
    timeout = "10s"
    outcome "success" { transition_to = "celebrate" }
    outcome "failure" { transition_to = "verify" }   # retry
  }

  # ---- Stage 4: finish ----------------------------------------------------

  step "celebrate" {
    adapter = "shell"
    input {
      command = "printf '\\n=== ALL DONE ===\\n'; printf 'Counter reached: %s\\n' \"$(cat /tmp/overlord-demo-counter)\"; rm -f /tmp/overlord-demo-counter; sleep 1; printf 'celebration complete.\\n'"
    }
    timeout = "10s"
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "aborted" }
  }

  state "done" { terminal = true }
  state "aborted" {
    terminal = true
    success  = false
  }
}
