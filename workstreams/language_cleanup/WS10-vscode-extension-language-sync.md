# WS10 — VSCode extension language sync and `.chcl` support

**Phase:** Language Cleanup · **Track:** Editor tooling · **Owner:** Workstream executor · **Depends on:** WS07 (spec current so extension matches it). · **Unblocks:** Publishing the formal language plugin. · **Base branch:** `main` · **Repo:** `criteria-vscode-extension-v1`

## Context

The extension in `criteria-vscode-extension-v1` was built against the pre-WS01 language and has not been updated since. It has the right architecture — diagnostics, go-to-definition, workspace index, syntax highlighting — but the implementation is broken for every workflow written in the current language:

| File | Root cause |
|---|---|
| `src/hclParser.ts:29` | `workflow` regex uses old label form `workflow "name" {`; current form has no label — `directoryHasWorkflowBlock` always returns `false` for current workflows |
| `src/hclParser.ts:38` | Still indexes `shared_variable` block kind (removed WS02; now `data "internal"`) |
| `src/definition.ts:29` | `next = "node_name"` quoted-string routing (removed WS02; now `next = step.foo` traversal) |
| `src/definition.ts:39` | `initial_state = "name"` / `target_state = "name"` — still work but need traversal alternative |
| `src/definition.ts:109` | `shared.<name>` namespace (removed WS02; now `data.internal.<name>.value`) |
| `src/definition.ts:139` | `endsWith('.hcl')` — `.chcl` files get no diagnostics or go-to-definition |
| `src/diagnostics.ts:19` | `WITH_LOCATION` regex matches `\.hcl` in file paths — `.chcl` paths not recognized |
| `src/package.json` | `extensions: []` — neither `.hcl` nor `.chcl` registered; no automatic language association |

## Prerequisites

- WS07 merged.

## In scope

### Step 1 — `src/hclParser.ts`: update block patterns

**`workflow` single-label pattern → no-label:**

The old pattern `workflow\s+"([^"]+)"\s*\{` never matches current workflows. Replace with a two-pass approach: detect the `workflow {` opener, then extract `name` from the next few lines.

```typescript
// NEW: detect no-label workflow block opener
{ re: /^(workflow)\s*\{/, kind: 'workflow' as BlockKind, extractNameFromBody: true },
```

Because the name is now a body attribute (`name = "foo"`), update `scanFile` to look ahead when `extractNameFromBody` is true:

```typescript
// After matching a no-label block, scan forward up to 5 lines for: name = "value"
const nameAttrRe = /^\s*name\s*=\s*"([^"]+)"/;
for (let look = lineIdx + 1; look < Math.min(lineIdx + 6, lines.length); look++) {
  const m = nameAttrRe.exec(lines[look]);
  if (m) { decl.name = m[1]; break; }
}
```

**`shared_variable` → `data` (two-label):**

Remove `shared_variable` from `SINGLE_LABEL`. Add `data` to `DOUBLE_LABEL`:

```typescript
// ADD to DOUBLE_LABEL:
{ re: /^(data)\s+"([^"]+)"\s+"([^"]+)"\s*\{/, kind: 'data' as BlockKind },
```

Update `BlockKind` union type:
```typescript
export type BlockKind =
  | 'workflow' | 'step' | 'state' | 'wait' | 'approval' | 'switch'
  | 'adapter' | 'subworkflow' | 'variable' | 'local' | 'data'   // 'data' replaces 'shared_variable'
  | 'environment' | 'output';
```

**`directoryHasWorkflowBlock`:** Update regex to match no-label form:
```typescript
// BEFORE:
const workflowRe = /^workflow\s+"[^"]+"\s*\{/m;
// AFTER:
const workflowRe = /^workflow\s*\{/m;
```

**`.chcl` file support:** Wherever the scanner filters files by extension (any `**.hcl` glob), update to include `**.chcl`. Check `workspaceIndex.ts` as well.

### Step 2 — `src/definition.ts`: update reference patterns

**`next` traversal (replace quoted-string pattern):**

```typescript
// BEFORE: next = "node_name"
const nextRe = /\bnext\s*=\s*"([^"]+)"/g;

// AFTER: next = step.foo  /  next = state.foo  /  next = switch.foo  /  next = return  /  next = continue
const nextTraversalRe = /\bnext\s*=\s*(?:(step|state|switch|wait|approval)\.([a-zA-Z_][\w]*)|(return|continue))\b/g;
```

When matched:
- `step.<name>` → look up `step` kind
- `state.<name>` → look up `state` kind
- `switch.<name>` → look up `switch` kind
- `wait.<name>` / `approval.<name>` → look up respective kinds
- `return` / `continue` — no definition jump (built-in keywords)

**`initial_state` / `target_state` (already string-form, still works):** These attributes still use quoted strings for the node name, so the existing regex continues to work. No change needed.

**`shared.<name>` → `data.internal.<name>`:** Remove the `shared.` handler. Add:
```typescript
// data.internal.<name>
const dataRe = /\bdata\.internal\.([a-zA-Z_][\w]*)/g;
for (const m of line.matchAll(dataRe)) {
  const mStart = m.index ?? 0;
  const mEnd = mStart + m[0].length;
  if (charPos >= mStart && charPos <= mEnd) {
    return { kind: 'data', name: m[1] };
  }
}
```

Update the `provideDefinition` switch to handle `'data'`:
```typescript
case 'data':
  decl = this.index.findByKind(dir, 'data', ref.name);
  break;
```

**`.chcl` extension guard:** Change `endsWith('.hcl')` to a helper:
```typescript
function isCriteriaFile(fileName: string): boolean {
  return fileName.endsWith('.hcl') || fileName.endsWith('.chcl');
}
```
Replace all `endsWith('.hcl')` checks with `isCriteriaFile(document.fileName)`.

### Step 3 — `src/diagnostics.ts`: match `.chcl` in paths

Update the `WITH_LOCATION` regex to match both extensions in file paths:

```typescript
// BEFORE:
const WITH_LOCATION = /^(Error|Warning):\s+(.+\.hcl):(\d+),(\d+):\s+(.+)$/;

// AFTER:
const WITH_LOCATION = /^(Error|Warning):\s+(.+\.(?:hcl|chcl)):(\d+),(\d+):\s+(.+)$/;
```

### Step 4 — `src/config.ts` and `src/extension.ts`: `.chcl` file recognition

In `isCriteriaFile` (in `config.ts` or wherever it is defined):
```typescript
// BEFORE:
export function isCriteriaFile(doc: vscode.TextDocument): boolean {
  return doc.fileName.endsWith('.hcl');
}

// AFTER:
export function isCriteriaFile(doc: vscode.TextDocument): boolean {
  return doc.fileName.endsWith('.hcl') || doc.fileName.endsWith('.chcl');
}
```

In `src/extension.ts`, update any document selector that gates activation to include `.chcl`:
```typescript
const selector: vscode.DocumentSelector = [
  { language: 'criteria-hcl' },
  { pattern: '**/*.hcl' },
  { pattern: '**/*.chcl' },
];
```

### Step 5 — `package.json`: register extensions

```json
"languages": [
  {
    "id": "criteria-hcl",
    "aliases": ["Criteria HCL", "criteria"],
    "extensions": [".chcl", ".hcl"],
    "configuration": "./language-configuration.json"
  }
]
```

Also update any `when` clauses in `menus` that check `resourceExtname == '.hcl'`:
```json
"when": "resourceExtname == '.hcl' || resourceExtname == '.chcl'"
```

### Step 6 — `src/workspaceIndex.ts`: scan `.chcl` files

Find the `vscode.workspace.findFiles` or `glob` call that scans for workflow files:

```typescript
// BEFORE:
const files = await vscode.workspace.findFiles('**/*.hcl', ...);

// AFTER:
const hclFiles = await vscode.workspace.findFiles('**/*.hcl', ...);
const chclFiles = await vscode.workspace.findFiles('**/*.chcl', ...);
const files = [...hclFiles, ...chclFiles];
```

### Step 7 — Build and smoke-test

```sh
cd /path/to/criteria-vscode-extension-v1
npm install
npm run build     # or esbuild.mjs
# Open VSCode with the extension loaded (F5 in extension dev host)
# Open an example .chcl workflow (e.g. examples/phase3-fold/fold-demo.hcl renamed to .chcl)
# Verify: syntax highlighting, inline diagnostics on save, go-to-definition on `next = step.greet`
```

## Out of scope

- Completions (block/attribute autocomplete) — WS11 scope.
- Hover documentation — WS11 scope.
- Publishing to the VS Code Marketplace — separate publishing step after QA.
- Converting the extension to use the `criteria langserver` LSP backend — after WS11 lands.

## Reuse pointers

- `src/config.ts:isCriteriaFile` — central file-type guard; update this first so all consumers inherit `.chcl` support.
- `src/hclParser.ts:scanFile` — used by `workspaceIndex.ts` to build the symbol index; updating the block patterns here propagates everywhere.
- `examples/phase3-fold/fold-demo.hcl` — good smoke-test workflow (uses `local`, `variable`, `step`, `state`); copy to `.chcl` for extension testing.

## Behavior change

**User-facing:**
- `.chcl` files now get syntax highlighting, inline diagnostics, and go-to-definition (new).
- Go-to-definition on `next = step.foo` now works (was broken for current-syntax workflows).
- `data "internal"` blocks are indexed and navigable (was `shared_variable` — broken).
- `workflow { name = "..." }` header is correctly detected as a workflow module root (was broken).

**Existing `.hcl` workflows:** fully backward-compatible — all existing behavior preserved.

## Tests required

- Extension compiles without TypeScript errors (`npm run build` clean).
- Open a current-syntax `.chcl` file: syntax highlighting applied, no false "unknown file type" state.
- Save a `.chcl` file with a compile error: inline diagnostic appears at the correct line.
- Go-to-definition on `next = step.greet` in a two-step workflow: jumps to `step "greet"` declaration.
- Go-to-definition on `target = adapter.shell.default`: jumps to `adapter "shell" "default"` declaration.
- `data "internal" "counter"` block appears in the workspace outline.

## Implementation Notes

### Checklist

- [ ] Step 1 — `hclParser.ts`: workflow no-label pattern, data block, directoryHasWorkflowBlock, .chcl globs
- [ ] Step 2 — `definition.ts`: next traversal pattern, data.internal reference, .chcl guard
- [ ] Step 3 — `diagnostics.ts`: WITH_LOCATION regex updated for .chcl paths
- [ ] Step 4 — `config.ts` / `extension.ts`: isCriteriaFile updated, document selector updated
- [ ] Step 5 — `package.json`: .chcl and .hcl extensions registered
- [ ] Step 6 — `workspaceIndex.ts`: scan .chcl files
- [ ] Step 7 — Build passes; smoke-tested in extension dev host

### Reviewer Notes

_To be filled in during review._
