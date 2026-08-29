#!/usr/bin/env bash
# validate-docs.sh — extract HCL fenced blocks from docs/*.md and validate each.
set -euo pipefail

BINDIR="${BINDIR:-./bin}"
BIN="${CRITERIA_BIN:-${BINDIR}/criteria}"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# The validation binary must be built with version.Version=dev (or no version
# ldflag) so that CRITERIA_OVERRIDE_VERSION is consulted. A release-style
# embedded version ignores the override.
export CRITERIA_OVERRIDE_VERSION="${CRITERIA_OVERRIDE_VERSION:-0.5.9}"

python3 - "$TMPDIR" "$BIN" <<'PY'
import re, os, subprocess, sys

tmp = sys.argv[1]
bin_path = sys.argv[2]
errors = []

for doc in ['docs/LANGUAGE-SPEC.md']:
    with open(doc) as f:
        content = f.read()
    blocks = re.findall(r'```hcl\n(.*?)\n```', content, re.DOTALL)
    workflows = [b for b in blocks if 'workflow' in b]
    for i, block in enumerate(workflows):
        d = os.path.join(tmp, f'example_{i+1}')
        os.makedirs(d, exist_ok=True)
        fname = os.path.join(d, 'workflow.hcl')
        with open(fname, 'w') as out:
            out.write(block)
        # Create dummy subworkflow directories when needed
        if 'subworkflow' in block:
            for line in block.split('\n'):
                if 'source' in line and '=' in line:
                    src = line.split('=')[1].strip().strip('"').strip("'")
                    if src.startswith('./'):
                        sw_dir = os.path.join(d, src[2:])
                        os.makedirs(sw_dir, exist_ok=True)
                        with open(os.path.join(sw_dir, 'workflow.hcl'), 'w') as sw:
                            sw.write(
                            'workflow {\n'
                            '  name = "child"\n'
                            '  version = "1"\n'
                            '  initial_state = "done"\n'
                            '  target_state = "done"\n'
                            '}\n'
                            'state "done" {\n'
                            '  terminal = true\n'
                            '  success = true\n'
                            '}\n'
                        )
        result = subprocess.run(
            [bin_path, 'validate', fname],
            capture_output=True, text=True
        )
        if result.returncode != 0:
            errors.append(f'{doc} example {i+1}: {result.stdout.strip() or result.stderr.strip()}')

if errors:
    for e in errors:
        print(e, file=sys.stderr)
    sys.exit(1)
print('All doc examples validated.')
PY
