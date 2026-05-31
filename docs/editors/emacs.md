# Emacs — Criteria language server

`criteria langserver` provides diagnostics, document symbols, and go-to-definition for `.hcl` and `.chcl` files.

## Eglot

Add the server to `eglot-server-programs`:

```elisp
(with-eval-after-load 'eglot
  (add-to-list 'eglot-server-programs
               '((hcl-mode) "criteria" "langserver")))
```

If you use `criteria-hcl-mode` (or similar), register that instead:

```elisp
(add-to-list 'eglot-server-programs
             '((criteria-hcl-mode) "criteria" "langserver"))
```

Start the server with `M-x eglot` in a `.chcl` or `.hcl` buffer.

## Supported capabilities

- `textDocument/publishDiagnostics` — errors and warnings on open / save
- `textDocument/documentSymbol` — `imenu` / `xref` symbol listing
- `textDocument/definition` — `M-.` on `next = step.foo`, `var.name`, etc.
