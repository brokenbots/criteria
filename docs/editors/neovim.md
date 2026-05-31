# Neovim — Criteria language server

`criteria langserver` provides diagnostics, document symbols, and go-to-definition for `.hcl` and `.chcl` files.

## Minimal `nvim-lspconfig` setup

```lua
local lspconfig = require('lspconfig')
local configs = require('lspconfig.configs')

if not configs.criteria then
  configs.criteria = {
    default_config = {
      cmd = { 'criteria', 'langserver' },
      filetypes = { 'hcl', 'criteria-hcl' },
      root_dir = lspconfig.util.root_pattern('*.chcl', '*.hcl'),
      settings = {},
    },
  }
end

lspconfig.criteria.setup({})
```

Add `criteria-hcl` to your filetype detection (e.g. in `~/.config/nvim/ftdetect/criteria.vim`):

```vim
autocmd BufRead,BufNewFile *.chcl set filetype=criteria-hcl
```

## Supported capabilities

- `textDocument/publishDiagnostics` — errors and warnings on open / save
- `textDocument/documentSymbol` — outline of steps, adapters, variables, etc.
- `textDocument/definition` — go to declaration from `next = step.foo`, `var.name`, etc.
