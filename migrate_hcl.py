#!/usr/bin/env python3
"""
HCL migration script: moves all non-header blocks out of the workflow block to top level.

Header attributes that STAY in the workflow block:
  version, initial_state, target_state, environment (string attribute only, not block)
"""
import re
import sys

HEADER_ATTRS = {"version", "initial_state", "target_state"}

def get_attr_key(line):
    m = re.match(r'^\s*(\w+)\s*=', line)
    if m:
        return m.group(1)
    return None

def is_header_attr_line(line):
    """True if this line is a simple key=value attribute (not block) for a header key."""
    stripped = line.strip()
    if not stripped or stripped.startswith("//") or stripped.startswith("#"):
        return False
    # Simple attribute: key = value (not opening a block)
    m = re.match(r'^\s*(\w+)\s*=\s*', line)
    if m:
        key = m.group(1)
        # environment = "..." is header, but environment { ... } is not
        if key == "environment":
            rest = line[m.end():].strip()
            return not rest.startswith("{")
        return key in HEADER_ATTRS
    return False

def find_workflow_blocks(src):
    """
    Find all workflow blocks in src.
    Returns list of (start_pos, end_pos, name) where start_pos is the position of 'workflow'
    and end_pos is the position just after the closing '}'.
    """
    blocks = []
    # Find 'workflow "' at word boundary
    for m in re.finditer(r'(?m)^[ \t]*workflow\s+"[^"]+"\s*\{', src):
        start = m.start()
        brace_start = src.index('{', m.start())
        depth = 0
        i = brace_start
        in_str = False
        while i < len(src):
            c = src[i]
            if in_str:
                if c == '"' and src[i-1:i] != '\\':
                    in_str = False
            else:
                if c == '"':
                    in_str = True
                elif c == '{':
                    depth += 1
                elif c == '}':
                    depth -= 1
                    if depth == 0:
                        blocks.append((start, i + 1))
                        break
            i += 1
    return blocks

def extract_top_level_items(body, indent="  "):
    """
    Given the body text between the workflow { and }, extract top-level items.
    Returns (header_lines, body_items) where:
    - header_lines: list of lines that are header attributes
    - body_items: list of strings (each a top-level block or attribute to move out)
    """
    lines = body.split('\n')
    header_lines = []
    body_items = []
    
    i = 0
    # buffer for pending blank/comment lines
    pending = []
    
    while i < len(lines):
        line = lines[i]
        stripped = line.strip()
        
        if not stripped or stripped.startswith("//") or stripped.startswith("#"):
            pending.append(line)
            i += 1
            continue
        
        if is_header_attr_line(line):
            # Keep in workflow block; flush pending to header
            header_lines.extend(pending)
            pending = []
            header_lines.append(line)
            i += 1
            continue
        
        # Not a header attr. Collect this block/item.
        # Count braces to find end of this top-level item
        item_lines = list(pending)
        pending = []
        item_lines.append(line)
        
        depth = 0
        in_str = False
        for ch in line:
            if in_str:
                if ch == '"':
                    in_str = False
            else:
                if ch == '"':
                    in_str = True
                elif ch == '{':
                    depth += 1
                elif ch == '}':
                    depth -= 1
        
        if depth > 0:
            # Multi-line block - keep collecting until depth 0
            i += 1
            while i < len(lines) and depth > 0:
                l = lines[i]
                item_lines.append(l)
                for ch in l:
                    if in_str:
                        if ch == '"':
                            in_str = False
                    else:
                        if ch == '"':
                            in_str = True
                        elif ch == '{':
                            depth += 1
                        elif ch == '}':
                            depth -= 1
                i += 1
        else:
            i += 1
        
        body_items.append(item_lines)
    
    # Any leftover pending (trailing blanks at end of workflow body) - discard
    return header_lines, body_items

def unindent(lines, strip_spaces=2):
    """Remove strip_spaces spaces from the beginning of each non-empty line."""
    result = []
    for line in lines:
        if line.strip():
            if line.startswith(" " * strip_spaces):
                result.append(line[strip_spaces:])
            else:
                result.append(line)
        else:
            result.append(line)
    return result

def migrate_workflow_block(src):
    """Find workflow blocks and migrate them."""
    blocks = find_workflow_blocks(src)
    if not blocks:
        return src
    
    # Process from last to first to preserve positions
    result = src
    for start, end in reversed(blocks):
        block_src = result[start:end]
        
        # Find the opening brace
        brace_pos = block_src.index('{')
        header_line = block_src[:brace_pos + 1]  # includes the '{'
        body = block_src[brace_pos + 1:-1]  # between { and }
        
        # Get leading indent of the workflow keyword
        leading_match = re.match(r'^(\s*)', result[start:])
        wf_indent = leading_match.group(1) if leading_match else ""
        
        header_attrs, body_items = extract_top_level_items(body)
        
        # Strip leading and trailing blank lines from header_attrs
        while header_attrs and not header_attrs[0].strip():
            header_attrs.pop(0)
        while header_attrs and not header_attrs[-1].strip():
            header_attrs.pop()
        
        # Build new workflow block (header only)
        new_block_lines = [header_line.rstrip()]
        new_block_lines.extend(header_attrs)
        new_block_lines.append(wf_indent + "}")
        new_block = '\n'.join(new_block_lines)
        
        # Build top-level body items (un-indented)
        top_level_parts = []
        for item_lines in body_items:
            unindented = unindent(item_lines, 2)
            top_level_parts.append('\n'.join(unindented))
        
        if top_level_parts:
            top_level_str = "\n" + "\n".join(top_level_parts)
        else:
            top_level_str = ""
        
        replacement = new_block + top_level_str
        result = result[:start] + replacement + result[end:]
    
    return result


def migrate_go_file(src):
    """
    Find all backtick-quoted strings containing 'workflow "' and migrate the HCL inside them.
    """
    result = []
    i = 0
    while i < len(src):
        if src[i] == '`':
            j = i + 1
            while j < len(src) and src[j] != '`':
                j += 1
            if j < len(src):
                content = src[i+1:j]
                if 'workflow "' in content:
                    migrated = migrate_workflow_block(content)
                    result.append('`')
                    result.append(migrated)
                    result.append('`')
                else:
                    result.append(src[i:j+1])
                i = j + 1
            else:
                result.append(src[i:])
                i = len(src)
        else:
            result.append(src[i])
            i += 1
    return ''.join(result)


def main():
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('file')
    parser.add_argument('--go', action='store_true')
    parser.add_argument('--dry-run', action='store_true')
    args = parser.parse_args()
    
    with open(args.file, 'r') as f:
        src = f.read()
    
    if args.go:
        result = migrate_go_file(src)
    else:
        result = migrate_workflow_block(src)
    
    if args.dry_run:
        print(result)
    else:
        with open(args.file, 'w') as f:
            f.write(result)
        print(f"Migrated: {args.file}")

if __name__ == '__main__':
    main()
