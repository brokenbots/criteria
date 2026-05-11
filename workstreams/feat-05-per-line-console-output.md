# feat-05 — Per-line step+adapter console output

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** D (features) · **Owner:** Workstream executor · **Depends on:** none. · **Unblocks:** none.

## Context

Today the concise console output ([internal/run/console_sink.go](../internal/run/console_sink.go)) renders step transitions, agent messages, and tool calls but **doesn't carry per-line step framing**. A reader scrolling through output sees:

```
[2/7] build_step  (shell)
  agent: Starting build...
  → npm run build
  → read package.json
  ✓ success in 1.2s
[3/7] deploy_step  (copilot)
  agent: Deploying to staging...
  → POST /api/deploy
  ✓ success in 5.3s
```

The middle lines (`agent:`, `→ npm run build`) lose the step framing the moment the cursor scrolls past the `[i/N]` header. Users have asked for **per-line framing with inline tool emojis**:

```
[2/7 build_step · shell(exec)]   ⚡ npm run build
[2/7 build_step · shell(exec)]   📄 read package.json
[3/7 deploy_step · copilot(agent)] 🌐 POST /api/deploy
[3/7 deploy_step · copilot(agent)] 🔍 grep "error" logs/
[3/7 deploy_step · copilot(agent)] ✏️ edit config.yaml
```

Per the user's choices:

1. **New format becomes the default in concise mode.** JSON mode is unchanged.
2. **Emoji table is hardcoded by tool-name category** — file ops 📄, shell/exec ⚡, network/http 🌐, search/grep 🔍, write/edit ✏️, fallback →.

This workstream reworks `consoleStepSink` to:

- Prefix every line with `[i/N step_name · adapter_name(adapter_type)]`.
- Replace the existing single `→` symbol on tool calls with a category-mapped emoji.
- Render BOTH agent message lines AND tool invocations with the prefix.
- Keep the `step entered` and `step outcome` lines (they carry `[i/N]` already; rework to match the new format).
- Preserve all existing behavior in JSON mode (no prefix, no emojis).

The format is **default**, with no escape hatch — the user accepted the non-backwards-compatible default-change. No `--output=concise-classic` mode.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with the existing rendering at [internal/run/console_sink.go](../internal/run/console_sink.go), in particular:
  - `ConsoleSink` struct (lines 22-32).
  - `consoleStepSink` (lines 266-325) — per-step adapter event rendering.
  - `OnStepEntered` (around line 94) — step header rendering.
  - `OnStepOutcome` (around line 115) — outcome line rendering.
  - `idxByStep` map (lines 39-40) — step position lookup.
- The output mode pipeline at [internal/cli/apply_output.go:13-76](../internal/cli/apply_output.go#L13-L76) — `resolveOutputMode`, `buildLocalSink`.

## In scope

### Step 1 — Define the new line format

The canonical concise-mode output line shape is:

```
<prefix><sep><body>
```

Where:

- **`<prefix>`** is `[I/N STEP · ADAPTER(TYPE)]` where:
  - `I` is the 1-based step index from `idxByStep[step] + 1`.
  - `N` is the total step count from `len(c.Steps)`.
  - `STEP` is the step name (no truncation).
  - `ADAPTER(TYPE)` is the adapter ref name and parenthesised type, e.g. `copilot(agent)` or `shell(exec)`. The type comes from the `adapterName` argument to `OnStepEntered` (which already carries the adapter type — verify the field semantics; if it carries only the ref-name without the type, extend the engine event to include type, or look it up via the FSMGraph reference held by ConsoleSink).
- **`<sep>`** is a single space.
- **`<body>`** is the per-event content:
  - For tool calls: `<emoji> <tool-name> <one-line summary>`.
  - For agent messages: `agent: <message line>` (multi-line messages have one prefixed line per content line).
  - For permission events: `· permission <granted|denied>: <tool-name>`.

The `[I/N STEP · ADAPTER(TYPE)]` prefix is rendered in **dim** ANSI color (`\x1b[2m...\x1b[0m`) so it visually recedes and the body stands out. Color is gated by `Color bool` on `ConsoleSink` (already present at line 28); when false, the prefix is plain.

When the adapter type is unknown (e.g. an event arrives before `OnStepEntered` for that step — defensive case), render `ADAPTER(?)` rather than crashing.

### Step 2 — Define the emoji category table

New file: `internal/run/tool_emoji.go`.

```go
package run

import "strings"

// toolEmoji returns a 1-rune-or-grapheme inline marker for the given tool name.
// The mapping is by case-insensitive substring match against well-known tool
// name conventions, with a fallback marker for unknown tools.
//
// Categories (in priority order):
//   - File operations: read, file, open, cat, ls, list, dir, find — 📄
//   - Write/edit:      write, edit, modify, create, save, append, replace — ✏️
//   - Shell/exec:      shell, exec, run, bash, sh, cmd, command — ⚡
//   - Network/HTTP:    http, fetch, get, post, put, delete, request, curl, api — 🌐
//   - Search/grep:     search, grep, find_text, query, lookup — 🔍
//   - Fallback:        → (right-arrow)
//
// The match is case-insensitive substring matching on the tool name. Earlier
// categories win for ambiguous names (e.g. "grep_files" matches search before
// file).
func toolEmoji(toolName string) string {
    n := strings.ToLower(toolName)
    for _, cat := range emojiCategories {
        for _, kw := range cat.keywords {
            if strings.Contains(n, kw) {
                return cat.emoji
            }
        }
    }
    return "→"   // fallback
}

type emojiCategory struct {
    emoji    string
    keywords []string
}

var emojiCategories = []emojiCategory{
    {emoji: "🔍", keywords: []string{"search", "grep", "find_text", "query", "lookup"}},
    {emoji: "🌐", keywords: []string{"http", "fetch", "request", "curl", "api", "post", "put", "delete"}},
    {emoji: "✏️",  keywords: []string{"write", "edit", "modify", "create", "save", "append", "replace"}},
    {emoji: "⚡", keywords: []string{"shell", "exec", "bash", " sh ", "cmd", "command", "run"}},
    {emoji: "📄", keywords: []string{"read", "file", "open", "cat", "ls", "list", "dir", "find"}},
}
```

The category order is intentional: **search** wins over file (so `grep_files` → 🔍), **network** wins over write/edit (so `http_post` → 🌐), **write/edit** wins over shell (so `edit_command` → ✏️). Document the ordering with the test cases below.

The `" sh "` keyword (with surrounding spaces) avoids false positives like `crash` matching `sh`.

The `get` keyword is intentionally NOT in the network list because too many file-ops tools have `get` in the name. Network identification relies on `http`/`fetch`/`request`/`curl`/`api` plus the explicit verbs `post`/`put`/`delete`. `GET` requests will fall through to the fallback `→` unless their tool name carries another network keyword. Document.

### Step 3 — Implement the new rendering in `consoleStepSink`

Edit [internal/run/console_sink.go](../internal/run/console_sink.go):

1. Extend `consoleStepSink` to carry the prefix needed for per-line rendering:
   ```go
   type consoleStepSink struct {
       parent *ConsoleSink
       step   string

       // prefix is the precomputed "[I/N STEP · ADAPTER(TYPE)] " string,
       // populated by ConsoleSink.StepEventSink at construction time.
       // Empty string disables prefixing (defensive default).
       prefix string
   }
   ```

2. Change `ConsoleSink.StepEventSink` to construct the prefix:
   ```go
   func (c *ConsoleSink) StepEventSink(step string) adapter.EventSink {
       prefix := c.buildLinePrefix(step)
       return &consoleStepSink{parent: c, step: step, prefix: prefix}
   }

   func (c *ConsoleSink) buildLinePrefix(step string) string {
       idx, ok := c.idxByStep[step]
       if !ok {
           return ""   // unknown step: defensive no-op
       }
       total := len(c.Steps)
       adapterRef, adapterType := c.adapterFor(step)
       if adapterRef == "" {
           adapterRef = "?"
       }
       if adapterType == "" {
           adapterType = "?"
       }
       inner := fmt.Sprintf("[%d/%d %s · %s(%s)]", idx+1, total, step, adapterRef, adapterType)
       return c.color("2", inner) + " "
   }

   // adapterFor returns the adapter ref-name and type for a step. Sourced from
   // a new map ConsoleSink.adapterByStep populated in OnStepEntered.
   func (c *ConsoleSink) adapterFor(step string) (string, string) {
       if a, ok := c.adapterByStep[step]; ok {
           return a.refName, a.kind
       }
       return "", ""
   }
   ```

3. Add the `adapterByStep` map to `ConsoleSink`:
   ```go
   type ConsoleSink struct {
       // ... existing fields ...
       adapterByStep map[string]struct{ refName, kind string }
   }
   ```
   Initialise in the constructor (find via grep — likely `NewConsoleSink`).

4. Update `OnStepEntered` to populate `adapterByStep`. The current signature is `OnStepEntered(step, adapterName, attempt)`. Change to `OnStepEntered(ctx, step, adapterName, adapterType, attempt)` — **this is an `engine.Sink` interface change** that touches the engine. Coordinate carefully:
   - Find the `engine.Sink` interface in `internal/engine/`.
   - Add `adapterType string` parameter to `OnStepEntered`.
   - Update every implementation (`LocalSink`, `ConsoleSink`, `MultiSink`, any test fakes).
   - Update every call site in the engine to pass the type from `StepNode.AdapterRef` and the looked-up `AdapterType` from the FSMGraph.
   
   **OR**, keep the signature stable and look up the adapter type from a graph reference held by ConsoleSink. To do this:
   - Add a new field `Graph *workflow.FSMGraph` to `ConsoleSink`.
   - Populate it in `apply_local.go` when constructing the sink (around line 65).
   - In `buildLinePrefix`, look up `adapter := c.Graph.Adapters[adapterRef]; adapterType := adapter.Type`.
   
   **Pick the second option** — it's a smaller blast radius. The Sink interface stays unchanged.

5. Modify `renderAgentMessage` to prefix every output line:
   ```go
   func (ss *consoleStepSink) renderAgentMessage(data any) {
       eventType := lookupString(data, "event_type")
       if eventType == "assistant.message_delta" {
           return
       }
       content := lookupString(data, "content")
       if strings.TrimSpace(content) == "" {
           return
       }
       agentTag := ss.parent.color("36", "agent:")
       for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
           ss.parent.writeln(ss.prefix + agentTag + " " + line)
       }
   }
   ```
   Note: the previous behavior indented continuation lines with extra spaces; the new behavior emits the full prefix on every line. The visual is denser but every line is independently grep-able.

6. Modify `renderToolInvocation` to use the prefix and the emoji table:
   ```go
   func (ss *consoleStepSink) renderToolInvocation(data any) {
       name := lookupString(data, "name")
       if name == "" {
           name = "tool"
       }
       args := lookupString(data, "arguments")
       summary := summariseToolArgs(args)
       emoji := toolEmoji(name)
       line := ss.prefix + emoji + " " + name
       if summary != "" {
           line += " " + summary
       }
       ss.parent.writeln(truncateLine(line, 160))   // raise from 120 to 160 to accommodate the prefix
   }
   ```

7. Modify the permission handlers similarly:
   ```go
   case "permission.granted":
       ss.parent.writeln(ss.prefix + "· permission granted: " + lookupString(data, "tool"))
   case "permission.denied":
       ss.parent.writeln(ss.prefix + "· permission denied: " + lookupString(data, "tool"))
   case "limit.reached":
       ss.parent.writeln(ss.prefix + ss.parent.color("33", "limit reached"))
   ```

### Step 4 — Update the step header line

The existing `OnStepEntered` (around line 94) emits `[i/N] <step_name>  (<adapter_type>)[ attempt=N]`. Update to match the new format consistency:

```go
func (c *ConsoleSink) OnStepEntered(step, adapterName string, attempt int) {
    c.adapterByStep[step] = ... // (per Step 3)
    idx := c.idxByStep[step]
    total := len(c.Steps)
    adapterRef, adapterType := c.adapterFor(step)
    if adapterRef == "" { adapterRef = "?" }
    if adapterType == "" { adapterType = "?" }
    line := fmt.Sprintf("[%d/%d %s · %s(%s)]", idx+1, total, c.color("1", step), adapterRef, adapterType)
    if attempt > 1 {
        line += fmt.Sprintf(" attempt=%d", attempt)
    }
    c.writeln(c.color("1;36", "▶") + " " + line)
}
```

The header carries the `▶` symbol prefix (matching the existing `OnRunStarted` pattern at line 71). The dim-prefix style is the per-event lines, not the header.

### Step 5 — Update the step outcome line

The existing `OnStepOutcome` (around line 115) emits `  ✓ success in <duration>`. Update to carry the same `[i/N step · adapter(type)]` framing for consistency:

```go
func (c *ConsoleSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
    prefix := c.buildLinePrefix(step)
    var symbol, color string
    if err == nil && (outcome == "success" || outcome == "ok") {
        symbol = "✓"
        color = "1;32"
    } else {
        symbol = "✗"
        color = "1;31"
    }
    body := fmt.Sprintf("%s %s in %s", outcome, formatDuration(duration), c.adapterLifecycleTag(step))
    if err != nil {
        body = fmt.Sprintf("%s: %v (%s)%s", outcome, err, formatDuration(duration), c.adapterLifecycleTag(step))
    }
    c.writeln(prefix + c.color(color, symbol) + " " + body)
}
```

(The `adapterLifecycleTag` helper is the existing `[adapter: ...]` aggregation — preserve it; pull from the existing `stepLifecycle` map. Refactor as needed.)

### Step 6 — Update transition / wait / approval rendering

The other `On*` methods (`OnStepTransition`, `OnStepResumed`, `OnVariableSet`, `OnStepOutputCaptured`, `OnAdapterLifecycle`, `OnForEachEntered`, `OnStepIterationStarted`, `OnStepIterationCompleted`, etc.) currently use a mix of indentation and symbols. For consistency, audit each and apply the same pattern:

- If the event is **step-scoped** (carries a step name), prefix with `buildLinePrefix(step)`.
- If the event is **run-scoped** (no step), no prefix; use the existing `▶`/`✓`/`✗`/`·` symbols.

Specifically:

- `OnStepTransition(from, to, viaOutcome)` — run-scoped (the transition is between steps); keep existing format.
- `OnStepResumed(step, attempt, reason)` — step-scoped; prefix and use the `↻` symbol.
- `OnVariableSet(name, value, source)` — run-scoped; keep `·` style.
- `OnStepOutputCaptured(step, outputs)` — step-scoped; prefix.
- `OnAdapterLifecycle(stepName, adapterName, status, detail)` — accumulates into `stepLifecycle` map and renders only at outcome time. No change.
- `OnForEachEntered(node, count)` / `OnStepIterationStarted` / `OnStepIterationCompleted` — step-scoped; prefix.

Where the audit reveals an `On*` method that should be step-scoped but isn't currently (e.g. it doesn't take a step name argument), do NOT change the engine's call signature. Instead, render without prefix and document the gap in reviewer notes; a follow-up workstream can extend the engine's events.

### Step 7 — Tests

New file: `internal/run/console_sink_perline_test.go`.

Required tests:

1. `TestConsoleSink_PerLineFormat_AgentMessage` — drive a fake adapter event sequence (`OnRunStarted`, `OnStepEntered`, then `Adapter("agent.message", {content: "hello"})`, `OnStepOutcome`). Assert: the agent message line contains the `[I/N STEP · ADAPTER(TYPE)]` prefix and the agent's content.

2. `TestConsoleSink_PerLineFormat_ToolInvocation_HappyEmoji` — drive a tool invocation with name `"read_file"`. Assert: the line contains `📄`, the tool name, and the prefix.

3. `TestConsoleSink_PerLineFormat_ToolInvocation_ShellEmoji` — name `"shell_exec"`. Assert: contains `⚡`.

4. `TestConsoleSink_PerLineFormat_ToolInvocation_NetworkEmoji` — name `"http_get"`. Assert: contains `🌐`.

5. `TestConsoleSink_PerLineFormat_ToolInvocation_SearchEmoji` — name `"grep_files"`. Assert: contains `🔍`. (Confirms search wins over file via the priority order.)

6. `TestConsoleSink_PerLineFormat_ToolInvocation_WriteEmoji` — name `"edit_file"`. Assert: contains `✏️`. (Confirms write wins over file.)

7. `TestConsoleSink_PerLineFormat_ToolInvocation_FallbackArrow` — name `"unknown_thing"`. Assert: contains `→`.

8. `TestConsoleSink_PerLineFormat_MultilineAgent_PrefixOnEveryLine` — content `"line1\nline2\nline3"`. Assert: 3 lines emitted, each with the prefix.

9. `TestConsoleSink_PerLineFormat_NoColorMode_PrefixIsPlain` — `Color = false`. Assert: prefix is `[I/N ...]` without ANSI escapes.

10. `TestConsoleSink_PerLineFormat_ColorMode_PrefixIsDim` — `Color = true`. Assert: prefix is wrapped in `\x1b[2m...\x1b[0m`.

11. `TestConsoleSink_PerLineFormat_UnknownStep_NoPrefix` — adapter event arrives for a step not registered in `idxByStep`. Assert: line has empty prefix (defensive); no panic.

12. `TestConsoleSink_PerLineFormat_StepEnteredHeader_NewFormat` — drive `OnStepEntered("build", "shell.compile", 1)` (assuming graph has shell adapter "compile" of type "shell"). Assert: header line is `▶ [1/N build · compile(shell)]`.

13. `TestConsoleSink_PerLineFormat_StepOutcome_Success` — drive `OnStepOutcome("build", "success", 1*time.Second, nil)`. Assert: line is `[1/N build · compile(shell)] ✓ success in 1s` with the prefix.

14. `TestConsoleSink_PerLineFormat_StepOutcome_Error` — drive with non-nil error. Assert: line uses `✗` and contains the error.

15. `TestConsoleSink_PerLineFormat_LineWidth_LongPrefix` — long step name + long tool name. Assert: line is truncated at 160 chars (the new max from Step 3).

16. `TestConsoleSink_PerLineFormat_JsonModeUnchanged` — construct via `JSON` output mode (no ConsoleSink wrapping). Drive same events. Assert: ND-JSON output is byte-identical to pre-feat-05 main. (This is the load-bearing regression check — JSON consumers must not see any change.)

New file: `internal/run/tool_emoji_test.go`. Unit tests for the categoriser:

17. `TestToolEmoji_FileOps` — `read_file`, `open_path`, `list_dir`, `cat`, `ls`, `find_files` — all return 📄 (except `find_text` which is search).
18. `TestToolEmoji_WriteEdit` — `write_file`, `edit`, `modify_doc`, `create`, `save`, `append`, `replace` — all return ✏️.
19. `TestToolEmoji_ShellExec` — `shell`, `exec_command`, `bash_run`, `sh ` (with trailing space) — all return ⚡.
20. `TestToolEmoji_Network` — `http_post`, `fetch_url`, `request_get`, `curl`, `api_call`, `delete_resource` — all return 🌐.
21. `TestToolEmoji_Search` — `search`, `grep`, `find_text`, `query`, `lookup` — all return 🔍.
22. `TestToolEmoji_Fallback` — `weird_thing`, `xyz`, `` (empty) — all return →.
23. `TestToolEmoji_PriorityOrder_GrepFiles` — `grep_files` returns 🔍, not 📄.
24. `TestToolEmoji_PriorityOrder_HttpRead` — `http_read` returns 🌐, not 📄.
25. `TestToolEmoji_PriorityOrder_EditCommand` — `edit_command` returns ✏️, not ⚡.
26. `TestToolEmoji_CaseInsensitive` — `READ_FILE` returns 📄.
27. `TestToolEmoji_FalsePositive_CrashIsNotShell` — `crash_handler` returns → (the `" sh "` keyword has spaces).

### Step 8 — Update CHANGELOG awareness in reviewer notes

This workstream changes the **default** concise-mode output. Document in reviewer notes:

- Screenshot or text capture of pre-feat-05 output for a sample workflow (e.g. `examples/hello`).
- Same for post-feat-05.
- Confirmation that JSON mode (`--output=json`) is byte-identical (per Test #16).

The actual `CHANGELOG.md` is off-limits to this workstream (per the convention); the cleanup gate / release process picks up the change from the PR title and labels.

### Step 9 — Validation

```sh
go test -race -count=2 ./internal/run/...
go test -race -count=20 ./internal/run/ -run PerLineFormat   # high-pressure for the new path
make ci

# Manual: run the example workflow and visually inspect output
go run ./cmd/criteria apply examples/hello
go run ./cmd/criteria apply examples/hello --output=json | head -20   # confirm JSON unchanged
```

All four must exit 0. The visual inspection produces the new format; the JSON inspection produces the same format as `main`.

If any existing test asserts the old concise output format (likely in `internal/run/console_sink_test.go` or `internal/cli/apply_test.go`), update it to the new format. **This is in scope** — the format change is intentional. Document each updated golden file in reviewer notes.

## Behavior change

**Behavior change: yes — visible UX change in concise mode.**

Observable differences in concise mode (`--output=auto` on a TTY, or `--output=concise`):

1. Every step-scoped line carries a `[I/N step · adapter(type)]` prefix in dim color (or plain when `--no-color` / `NO_COLOR`).
2. Tool invocations show a category emoji (📄/⚡/🌐/🔍/✏️) instead of the `→` arrow. Unknown tools still show `→` as fallback.
3. Step header line uses the `▶ [I/N step · adapter(type)]` format consistently.
4. Step outcome line carries the prefix.
5. Multi-line agent messages no longer indent continuation lines with `   ` — instead, every line carries the prefix.
6. Line truncation cap raised from 120 to 160 chars to accommodate the prefix.

**JSON mode is unchanged.** The proto envelope, ND-JSON record, and event ordering are byte-identical. Test #16 is the lock-in.

No proto change. No SDK change. No CLI flag change.

## Reuse

- Existing `ConsoleSink`, `consoleStepSink`, `idxByStep`, `Steps` fields.
- Existing `color`, `writeln`, `truncate`, `truncateLine`, `formatDuration`, `lookupString`, `summariseToolArgs` helpers.
- Existing `OnStepEntered`, `OnStepOutcome`, `OnStepTransition`, etc. interface methods (signatures unchanged per Step 3 decision).
- Existing `LocalSink`, `MultiSink` in [internal/run/](../internal/run/) — no changes.
- `apply_output.go` `buildLocalSink` — extend to populate `ConsoleSink.Graph` reference.
- `protojson` / `structpb` patterns from existing code.

## Out of scope

- A `--output=concise-classic` escape hatch. Per user choice, the new format is default with no escape hatch.
- Configurable emoji map via env var or workflow header. Hardcoded table.
- Per-tool-name custom emoji (e.g. exact "git" → 🌳). Category-based only; finer mapping is a follow-up.
- ANSI colors beyond dim-prefix and the existing palette.
- Truecolor / 256-color usage. ANSI 16-color only.
- Wrapping long lines instead of truncating. Truncate at 160 chars.
- Changing the line-width cap dynamically based on terminal width. Fixed 160.
- Adding a "verbose" mode that shows all events including the dropped Log chunks.
- Modifying `LocalSink` or `MultiSink`.
- Modifying the engine's event types or `engine.Sink` interface.
- Modifying the proto envelope.
- Adapter-side emoji declaration (e.g. `adapter.Info().DisplayEmoji`). Out of scope.
- A "compact" option that omits the prefix. Future workstream if demand.

## Files this workstream may modify

- [`internal/run/console_sink.go`](../internal/run/console_sink.go) — Steps 3, 4, 5, 6.
- New file: [`internal/run/tool_emoji.go`](../internal/run/) — Step 2.
- New file: [`internal/run/console_sink_perline_test.go`](../internal/run/) — Step 7 tests #1–16.
- New file: [`internal/run/tool_emoji_test.go`](../internal/run/) — Step 7 tests #17–27.
- [`internal/run/console_sink_test.go`](../internal/run/console_sink_test.go) (if it exists) — update golden assertions to new format.
- [`internal/cli/apply_output.go`](../internal/cli/apply_output.go) — populate `ConsoleSink.Graph` reference (Step 3 option B). Likely a one-line addition.
- [`internal/cli/apply_test.go`](../internal/cli/apply_test.go) (if it asserts console output) — update.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Generated proto files.
- [`docs/workflow.md`](../docs/workflow.md) — output format is not part of the language; out-of-scope.
- [`internal/engine/`](../internal/engine/) — Sink interface signatures unchanged.
- `internal/run/local_sink.go` (or wherever LocalSink lives) — JSON mode is unchanged.
- [`.golangci.yml`](../.golangci.yml).
- `cmd/criteria-adapter-*/`.

## Tasks

- [x] Define the new line format (Step 1).
- [x] Implement `tool_emoji.go` with category table (Step 2).
- [x] Extend `consoleStepSink` with `prefix` and rework rendering (Step 3).
- [x] Update step header line (Step 4).
- [x] Update step outcome line (Step 5).
- [x] Audit other On* methods and apply prefix where step-scoped (Step 6).
- [x] Add 27 unit tests across two test files (Step 7).
- [x] Capture pre/post output samples in reviewer notes (Step 8).
- [x] Update any existing golden-format tests to the new format.
- [x] Validation including manual visual inspection (Step 9).

## Exit criteria

- Every step-scoped concise-mode line carries the `[I/N step · adapter(type)]` prefix.
- Tool calls show category emojis per the table.
- The 27 unit tests pass under `-race -count=20`.
- JSON mode output is byte-identical to pre-feat-05 (Test #16 is the lock-in).
- `make ci` exits 0.
- Manual inspection of `criteria apply examples/hello` shows the new format.
- No new `//nolint` directives added.
- No baseline cap change required.

## Tests

The Step 7 list (27 tests). Coverage of `consoleStepSink` ≥ 90%; coverage of `toolEmoji` ≥ 100% (small file, easily achievable).

Specifically:

- The category-priority tests (#23–25) are load-bearing — they prevent silent regression of the priority order.
- The JSON-unchanged test (#16) is THE lock-in for non-regression of the machine-readable contract.
- The `--no-color` test (#9) ensures the dim-prefix doesn't bleed when color is disabled.

## Risks

| Risk | Mitigation |
|---|---|
| Emoji rendering varies across terminals (some don't render emoji glyphs) | Document that the tool emoji set requires UTF-8 + emoji-capable terminal. The `→` fallback + `?` placeholder ensure no character appears uninterpretable. The `--no-color` env doesn't affect emoji rendering — emoji is content, not styling. |
| The 160-char cap is too tight on small terminals (80-col) | The line truncates with an ellipsis; readers see the prefix and the start of the body. Acceptable. The prefix is dim so visual focus is on the body. |
| The prefix-on-every-line style feels noisy compared to the previous format | The user explicitly chose the new format as default. The dim color reduces the visual weight. If feedback is negative post-merge, a follow-up workstream can add a configuration; for now, the contract is the default. |
| The hardcoded `idxByStep` lookup is racy if events arrive out of order | Existing implementation already uses `idxByStep` populated in `OnStepEntered`. The pattern is single-writer (engine drives Sink methods sequentially per step). No race expected. Tests under `-race -count=20` confirm. |
| The `Graph` reference held by ConsoleSink creates a lifetime coupling | The graph is read-only and lives for the duration of the run. The reference is freed when the sink goes out of scope. Standard Go ownership; no risk. |
| `OnStepEntered` arrives before the prefix can be built (because adapterByStep is populated by `OnStepEntered` itself) | The header line uses the same `buildLinePrefix` path; populate `adapterByStep` BEFORE calling `buildLinePrefix` in `OnStepEntered`. Step 4's snippet has the order correct. Test #12 covers. |
| A user has scripts grepping the old concise format (`agent: ...` without prefix) | The user explicitly accepted the breaking change. Document in the reviewer notes for the release-process picker. |
| Tool name with no recognised category but containing whitespace breaks the substring match | Substring matching tolerates whitespace; the test case `"weird_thing"` covers. The `" sh "`-with-spaces edge case is the deliberate guard against false positives. |
| Long tool names overflow the truncation in unhelpful ways (e.g. emoji + name + truncated args) | Truncation always preserves the prefix and emoji; the body truncates from the right. Test #15 covers. |
| Future engine changes change the order of `OnStepEntered` and the first adapter event arriving for the same step | The defensive empty-prefix path (Test #11) handles the case. No crash, just a missing prefix on the early event. |

## Reviewer Notes

### Implementation summary

**Option B chosen (stable Sink interface):** `ConsoleSink` gains a `Graph *workflow.FSMGraph` field and `adapterByStep map[string]struct{refName, kind string}`. The `NewConsoleSink` signature adds a `*workflow.FSMGraph` parameter (nil-safe). No `engine.Sink` interface changes.

**Files created:**
- `internal/run/tool_emoji.go` — emoji categoriser (`toolEmoji(string) string`), 5 categories + fallback `→`.
- `internal/run/tool_emoji_test.go` — 11 tests covering all 27 workstream-specified cases #17–27.
- `internal/run/console_sink_perline_test.go` — 16 tests covering workstream cases #1–16; uses `minimalGraph()` helper to build `*workflow.FSMGraph` test fixtures directly (no parser dependency).

**Files modified:**
- `internal/run/console_sink.go` — added `Graph`, `adapterByStep` fields; new helpers `buildLinePrefix`, `adapterFor`, `resolveAdapter`, `adapterLifecycleTag`; updated `OnStepEntered`, `OnStepOutcome`, `OnStepResumed`, `OnStepOutputCaptured`, `OnForEachEntered`, `OnStepIterationStarted`, `OnStepIterationCompleted`, `OnStepIterationItem`, `StepEventSink`, `consoleStepSink`, `renderAgentMessage`, `renderToolInvocation`, permission/limit handlers.
- `internal/cli/apply_output.go` — `buildLocalSink` signature adds `graph *workflow.FSMGraph`; passes to `NewConsoleSink`.
- `internal/cli/apply_local.go` — 3 `buildLocalSink` call sites updated to pass `graph`.
- `internal/cli/apply_output_test.go` — 2 test call sites updated to pass `nil`.
- `internal/run/console_sink_test.go` — all 10 existing tests updated: `NewConsoleSink` calls pass `nil`; assertions updated to new prefix format, `▶` header, emoji for bash tools.

### Workstream doc note — adapter display order (CORRECTED)

The initial implementation had `type(name)` order (e.g. `shell(compile)`). Per the reviewer, the correct format is `name(type)` — the adapter instance ref-name first, the parenthesized type second (e.g. `compile(shell)`, `default(shell)`). The implementation notes in the first submission incorrectly claimed the spec examples used type(name); the reviewer's interpretation of the spec is authoritative. Fixed in second submission.

### idxByStep is already 1-based

The workstream spec uses `idx+1` in the format-string snippet (Step 3), but `NewConsoleSink` already stores `idxByStep[s] = i+1` (1-based). The implementation uses `idx` directly from the map to avoid double-incrementing. Test #12 confirms the header shows `[1/N ...]` for the first step.

### Pre-feat-05 output (from `main` before this workstream)

```
[2/7] build_step  (shell)
  agent: Starting build...
  → npm run build
  → read package.json
  ✓ success in 1.2s
```

### Post-feat-05 output (`examples/hello` with this workstream)

```
▶ hello  steps=1
▶ [1/1 say_hello · default(shell)]
[1/1 say_hello · default(shell)] ✓ success in 1ms  [adapter: started → exited]
[1/1 say_hello · default(shell)] · outputs: stdout, stderr, exit_code
  → done
  output greeting (string) = "Execution complete"
✔ run completed in 2ms
```

(Prefix is dim-colored on a real TTY; shown here without ANSI for readability.)

### Post-feat-05 output (`examples/plugins/greeter` end-to-end)

```
▶ greeter_example  steps=1
▶ [1/1 greet · default(greeter)]
[1/1 greet · default(greeter)] ✓ success in 307µs  [adapter: started → exited]
[1/1 greet · default(greeter)] · outputs: greeting
  → done
✔ run completed in 477µs
```

### JSON mode — byte-for-byte assertion

Test #16 (`TestConsoleSink_PerLineFormat_JsonModeUnchanged`) asserts exact byte-for-byte ND-JSON output for a fixed deterministic event sequence (fixed RunID `"run-json-1"`, fixed duration `100ms`, no wall-clock fields). Any change to LocalSink payload structure or field encoding will fail this test.

### Validation (second submission)

```
go test -race -count=2  ./internal/run/...      → ok (27+3 new tests pass: added OkIsSuccess, OutcomeDefaulted, OutcomeUnknown)
go test -race -count=20 ./internal/run/ -run PerLineFormat → ok
make lint-imports                               → Import boundaries OK
make ci                                         → exit 0 (all packages green)
```

No new `//nolint` directives. No baseline cap change. No proto/SDK changes.

### Review 2026-05-11 — changes-requested

#### Summary

`make ci` is green, but the implementation does not meet the workstream contract yet. The rendered prefix uses `type(name)` instead of the specified `name(type)`, step outcome rendering still treats only `"success"` as a success path, some step-scoped warning/error lines are still unprefixed, and the JSON regression test does not prove the required byte-identical contract.

#### Plan Adherence

- **Steps 2-3:** largely implemented. Tool emoji mapping, per-line agent/tool rendering, and graph-backed adapter lookup are in place.
- **Step 4:** not accepted. The header and per-line prefix render `shell(default)` / `greeter(default)` instead of the specified `default(shell)` / `default(greeter)`.
- **Step 5:** not accepted. The implementation still renders only `outcome == "success"` as a success line; the workstream explicitly called for `"success"` and `"ok"`.
- **Step 6:** not accepted. `OnStepOutcomeDefaulted` and `OnStepOutcomeUnknown` are step-scoped lines and still use the old unprefixed format.
- **Step 7:** incomplete. Existing tests encode the reversed adapter order, do not cover the `"ok"` outcome success path, do not cover the defaulted/unknown outcome warning lines, and Test #16 does not lock in byte-identical JSON output.
- **Step 8:** incomplete. The executor notes document and justify the reversed adapter order instead of matching the workstream contract, and the JSON note overstates what the current test proves.

#### Required Remediations

- **Blocker — `internal/run/console_sink.go:105-125`, `internal/run/console_sink.go:351-396`, `internal/run/console_sink_perline_test.go:26-257`, `workstreams/feat-05-per-line-console-output.md:532-577`**  
  The adapter label order is reversed. The workstream defines the prefix as `[I/N step · ADAPTER(TYPE)]`, where `ADAPTER` is the adapter ref/name and `TYPE` is the parenthesized adapter type. Current code and tests render `type(name)` and the implementation notes claim the spec is inverted.  
  **Acceptance criteria:** render `default(shell)` for `adapter "shell" "default"` and equivalent `name(type)` formatting everywhere (header, agent lines, tool lines, outcome lines); update the tests to assert that shape; correct the workstream notes so they no longer contradict the spec.

- **Blocker — `internal/run/console_sink.go:128-147`**  
  `OnStepOutcome` still marks only `"success"` as successful. The workstream explicitly requires `"success"` and `"ok"` to take the green-check success path when `err == nil`.  
  **Acceptance criteria:** `OnStepOutcome(..., "ok", ..., nil)` renders as a success line with the prefixed green check, and a regression test proves it.

- **Blocker — `internal/run/console_sink.go:266-275`**  
  `OnStepOutcomeDefaulted` and `OnStepOutcomeUnknown` remain unprefixed despite the exit criterion that every step-scoped concise-mode line carries the new `[I/N step · adapter(type)]` prefix.  
  **Acceptance criteria:** both lines use `buildLinePrefix(step)` and dedicated tests cover both paths.

- **Blocker — `internal/run/console_sink_perline_test.go:280-311`, `workstreams/feat-05-per-line-console-output.md:575-577`**  
  The JSON regression check is too weak for the stated contract. Test #16 currently proves only “still JSON, no concise prefix/emoji,” not “byte-identical to pre-feat-05.” The reviewer note makes the stronger claim without evidence.  
  **Acceptance criteria:** replace Test #16 with a deterministic byte-for-byte assertion for the JSON-mode output of a fixed event sequence or fixed `runApply` path, so that changes in payload content/order/line count fail the test; update the notes to reflect the actual evidence.

#### Test Intent Assessment

- The new per-line tests do exercise the main rendering path, multiline agent output, color/no-color behavior, emoji priority, and truncation.
- The current suite is not strong enough on the load-bearing edges:
  - it bakes in the wrong adapter label order,
  - it omits the `"ok"` success-path behavior from Step 5,
  - it omits the step-scoped defaulted/unknown outcome lines,
  - and it does not make a byte-for-byte JSON contract regression possible.

#### Validation Performed

- `make build` → passed
- `go test -race -count=2 ./internal/run/...` → passed
- `go test -race -count=20 ./internal/run/ -run PerLineFormat` → passed
- `make lint-imports` → passed
- `make ci` → passed
- `go run ./cmd/criteria apply examples/hello --output=concise` → rendered `shell(default)`, which confirms the current adapter order mismatch
- `go run ./cmd/criteria apply examples/hello --output=json` → remained JSON output, but this manual check does not replace the missing byte-identical regression test
