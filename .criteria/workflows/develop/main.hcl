# Develop Subworkflow
# ===================
# Implements one workstream end-to-end:
#   prepare_branch → develop (LLM) → ci_gate (shell, with one auto-retry on
#   flake) → cache_diff → 4-axis parallel reviews → verdict aggregate →
#   (skip owner if unanimous approve) → owner adjudication → commit (shell) →
#   finalize_ok (sets status="ok").
#
# Optimizations vs the v1 design:
#   • ci_retry — one automatic retry of `make ci` before invoking the LLM
#     repair agent (CI flakes are the most common transient failure).
#   • cache_diff — runs `git diff origin/main...HEAD` once into a shared file;
#     all four reviewers read the file instead of each invoking git diff.
#   • verdict_aggregate + check_unanimous — when all four reviewers emit
#     "VERDICT: approved", skip the owner adjudication LLM call and go
#     straight to commit. Saves one expensive agent invocation on the happy
#     path.
#   • shell commit — git add/commit/push is deterministic; no LLM session
#     needed once the owner has approved.
#
# Failure-propagation workaround: the engine ignores a subworkflow's terminal
# `success=false` flag at the parent (internal/engine/node_step.go:477-480).
# Until that is fixed, we project `output "status"` based on a shared variable
# that defaults to "failed" and is flipped to "ok" only along the success
# path. The parent (bootstrap.hcl) switches on this status.

workflow "develop" {
  version       = "1"
  initial_state = "prepare_branch"
  target_state  = "returned"
}

policy {
  max_total_steps = 500
}

variable "workstream_file" {
  type        = "string"
  default     = ""
  description = "Path to the workstream markdown file, relative to project_dir."
}

variable "max_retries" {
  type        = "number"
  default     = 3
  description = "Maximum developer→owner cycles before requesting operator assistance."
}

variable "project_dir" {
  type        = "string"
  default     = ""
  description = "Absolute path to the criteria engine project root."
}

variable "developer_model" {
  type        = "string"
  default     = "claude-sonnet-4.6"
}

variable "reviewer_model" {
  type        = "string"
  default     = "gpt-5.4"
}

shared_variable "cycle_count" {
  type  = "number"
  value = 0
}

shared_variable "terminal_status" {
  type  = "string"
  value = "failed"
}

output "status" {
  type  = "string"
  value = shared.terminal_status
}

adapter "copilot" "developer" {
  config {
    model            = var.developer_model
    reasoning_effort = "high"
    max_turns        = 30
    system_prompt    = trimfrontmatter(file("agents/developer.agent.md"))
  }
}

adapter "copilot" "owner" {
  config {
    model            = var.reviewer_model
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("agents/owner.agent.md"))
  }
}

adapter "copilot" "repair" {
  config {
    model            = var.developer_model
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("agents/repair.agent.md"))
  }
}

adapter "shell" "ci" {
  config {}
}

subworkflow "review_axis" {
  source = "./review_axis"
}

# ── Restart-safe branch preparation ──────────────────────────────────────────

step "prepare_branch" {
  target     = adapter.shell.ci
  timeout    = "180s"
  max_visits = 10
  input {
    command           = "sh .criteria/workflows/bootstrap/scripts/prepare-workstream-branch.sh \"${var.workstream_file}\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "route_branch_state" }
  outcome "failure" { next = "failed" }
}

switch "route_branch_state" {
  condition {
    match = steps.prepare_branch.stdout == "already_merged"
    next  = step.finalize_ok
  }
  condition {
    match = steps.prepare_branch.stdout == "existing_local"
    next  = step.ci_gate
  }
  condition {
    match = steps.prepare_branch.stdout == "existing_remote"
    next  = step.ci_gate
  }
  default { next = step.develop_init }
}

# ── Initial implementation pass ──────────────────────────────────────────────

step "develop_init" {
  target      = adapter.copilot.developer
  allow_tools = ["*"]
  timeout     = "30m"
  input {
    prompt = "Read ${var.workstream_file} for the full task scope. Branch state classifier: `${steps.prepare_branch.stdout}` (one of: created, existing_local, existing_remote, existing_dirty; the branch name is `basename '${var.workstream_file}' .md`). If `created`, implement every acceptance-criterion item from a clean slate. If `existing_*`, inspect the current state, preserve useful work, and complete only missing items. Write tests. Run `make ci` to confirm; if it fails, fix before declaring ready. Update ${var.workstream_file} with implementation notes and check off completed items.\n\nEnd your final message with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Deterministic CI gate with single auto-retry on flake ────────────────────
# If `make ci` fails, retry ONCE before invoking the LLM repair agent. CI
# flakes (network blips, race conditions in tests) are the most common
# transient failure and don't warrant a token-expensive repair session.

step "ci_gate" {
  target     = adapter.shell.ci
  timeout    = "1200s"
  max_visits = 30
  input {
    command           = "make ci"
    working_directory = var.project_dir
  }
  outcome "success" { next = "cache_diff" }
  outcome "failure" { next = "ci_retry" }
}

step "ci_retry" {
  target     = adapter.shell.ci
  timeout    = "1200s"
  max_visits = 5
  input {
    command           = "echo '[ci_retry] re-running make ci once before invoking LLM repair'; make ci"
    working_directory = var.project_dir
  }
  outcome "success" { next = "cache_diff" }
  outcome "failure" { next = "repair_ci" }
}

step "repair_ci" {
  target      = adapter.copilot.repair
  allow_tools = ["read", "edit", "execute", "shell"]
  timeout     = "20m"
  max_visits  = 10
  input {
    prompt = "`make ci` failed twice (initial + one retry). Fix all failures with the smallest correct changes; do not refactor or expand scope. Do not raise the lint baseline cap or add to .golangci.baseline.yml — fix the finding instead.\n\n--- ci stdout (last attempt) ---\n${steps.ci_retry.stdout}\n--- ci stderr (last attempt) ---\n${steps.ci_retry.stderr}\n--- end ---\n\nEnd your final message with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Cache the diff for reviewers ─────────────────────────────────────────────
# Writes .criteria/tmp/diff.patch + diff.stat once so all 4 reviewers can read
# the same file instead of each invoking `git diff origin/main...HEAD`.

step "cache_diff" {
  target     = adapter.shell.ci
  timeout    = "60s"
  max_visits = 10
  input {
    command           = "sh .criteria/workflows/develop/scripts/cache-diff.sh"
    working_directory = var.project_dir
  }
  outcome "success" { next = "route_diff" }
  outcome "failure" { next = "failed" }
}

switch "route_diff" {
  condition {
    match = steps.cache_diff.stdout == "no_changes"
    next  = step.commit
  }
  condition {
    match = steps.cache_diff.stdout == "ok"
    next  = step.specialized_reviews
  }
  default { next = state.failed }
}

# ── Parallel specialist reviews — 4 axes ─────────────────────────────────────
# Reviewers always emit RESULT: success when their review is complete (regardless
# of whether the verdict is approved or changes_requested) — see the comment in
# review_axis/main.hcl explaining the engine's isSuccessOutcome strictness.
# on_failure = "continue" so a real reviewer failure (broken tooling) doesn't
# cancel the other in-flight reviewers; any_failed only fires if at least one
# reviewer truly errors out.

step "specialized_reviews" {
  target       = subworkflow.review_axis
  parallel     = ["security", "quality", "workstream", "api_compat"]
  parallel_max = 4
  on_failure   = "continue"
  max_visits   = 20
  input {
    review_kind     = each.value
    workstream_file = var.workstream_file
    project_dir     = var.project_dir
    reviewer_model  = var.reviewer_model
  }
  outcome "success"       { next = "_continue" }
  outcome "failure"       { next = "_continue" }
  outcome "all_succeeded" { next = "verdict_aggregate" }
  outcome "any_failed"    { next = "failed" }
}

# ── Verdict aggregation: skip owner_review on unanimous approval ────────────

step "verdict_aggregate" {
  target     = adapter.shell.ci
  timeout    = "30s"
  max_visits = 10
  input {
    command           = <<-CMD
      mkdir -p .criteria/tmp
      cat > .criteria/tmp/verdict_agg_input.txt <<'__END_REPORTS__'
      ${steps.specialized_reviews[0].report}
      ${steps.specialized_reviews[1].report}
      ${steps.specialized_reviews[2].report}
      ${steps.specialized_reviews[3].report}
      __END_REPORTS__
      sh .criteria/workflows/develop/scripts/aggregate-verdicts.sh < .criteria/tmp/verdict_agg_input.txt
    CMD
    working_directory = var.project_dir
  }
  outcome "success" { next = "check_unanimous" }
  outcome "failure" { next = "owner_review" }
}

switch "check_unanimous" {
  condition {
    match = steps.verdict_aggregate.stdout == "unanimous"
    next  = step.commit
  }
  default { next = step.owner_review }
}

# ── Owner adjudication (only when reviewers disagree) ───────────────────────

step "owner_review" {
  target      = adapter.copilot.owner
  allow_tools = ["read", "search", "edit", "execute"]
  timeout     = "20m"
  max_visits  = 20
  input {
    prompt = "You are the workstream owner for ${var.workstream_file}. Read the workstream, current diff (`git diff origin/main...HEAD`), and the four specialist reviewer reports below. Each report contains a `VERDICT: approved` or `VERDICT: changes_requested` line followed by findings. Decide which requests are legitimate, in scope, and mandatory. Reject overreach, duplicates, speculative rewrites, or anything contradicting the workstream non-goals.\n\nRecord your verdict under `## Owner Review Notes` in ${var.workstream_file}. If changes are needed, write only must-fix items. If complete, record owner approval.\n\n--- security ---\n${steps.specialized_reviews[0].report}\n--- quality ---\n${steps.specialized_reviews[1].report}\n--- workstream ---\n${steps.specialized_reviews[2].report}\n--- api_compat ---\n${steps.specialized_reviews[3].report}\n--- end ---\n\nEnd your final message with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approved"          { next = "commit" }
  outcome "changes_requested" { next = "count_cycle" }
  outcome "failure"           { next = "failed" }
}

# ── Cycle counter + max-retries operator gate ────────────────────────────────

step "count_cycle" {
  target     = adapter.shell.ci
  max_visits = 30
  input {
    command           = "echo $(( ${shared.cycle_count} + 1 ))"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "check_limit"
    shared_writes = { cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

switch "check_limit" {
  condition {
    match = shared.cycle_count >= var.max_retries
    next  = approval.request_user_assist
  }
  default { next = step.develop }
}

approval "request_user_assist" {
  approvers = ["operator"]
  reason    = "The developer/owner loop has reached max_retries cycles without convergence. Inspect the workstream md for owner notes. Approve to continue with a fresh cycle, or reject to fail the workstream."
  outcome "approved" { next = "reset_counter" }
  outcome "rejected" { next = "failed" }
}

step "reset_counter" {
  target     = adapter.shell.ci
  max_visits = 10
  input {
    command           = "echo 0"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "develop"
    shared_writes = { cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

# ── Iteration loop: developer addresses owner must-fix list ──────────────────

step "develop" {
  target      = adapter.copilot.developer
  allow_tools = ["*"]
  timeout     = "30m"
  max_visits  = 20
  input {
    prompt = "The workstream owner has requested changes for ${var.workstream_file}. Read only the owner-approved must-fix list under `## Owner Review Notes`; do not chase raw specialist reviewer suggestions the owner rejected. Address every must-fix item completely, then run `make ci`.\n\nEnd your final message with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Commit + push (deterministic shell, no LLM) ──────────────────────────────
# Owner approved (or unanimous specialist approval); the work is done. A
# deterministic shell step commits and pushes — no LLM judgment required.

step "commit" {
  target     = adapter.shell.ci
  timeout    = "120s"
  max_visits = 5
  input {
    command           = "set -eu; branch=$(git branch --show-current); if [ -z \"$branch\" ] || [ \"$branch\" = \"main\" ]; then echo 'refusing to commit on main' >&2; exit 1; fi; git add -A; if git diff --cached --quiet; then echo 'no changes to commit; ensuring branch is pushed'; else git commit -m \"feat: complete ${var.workstream_file}\"; fi; git push --set-upstream origin \"$branch\" 2>/dev/null || git push origin \"$branch\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "finalize_ok" }
  outcome "failure" { next = "failed" }
}

# ── Set status output to "ok" on the success path ───────────────────────────
# This is the only place that flips terminal_status away from its default
# "failed" value. The bootstrap parent reads this via the projected output.

step "finalize_ok" {
  target     = adapter.shell.ci
  timeout    = "10s"
  max_visits = 5
  input {
    command           = "printf '%s' 'ok'"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "returned"
    shared_writes = { terminal_status = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

state "returned" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
