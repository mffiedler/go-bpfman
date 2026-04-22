# Emacs support for bpfman

This directory contains Emacs tooling for the bpfman REPL language.

## Files

- [`bpfman-mode.el`](bpfman-mode.el) — major mode for editing
  `.bpfman` scripts: syntax highlighting, comment handling, and paired
  `()`/`{}`/`[]` delimiters so `show-paren-mode` picks up block and
  command-substitution structure.
- [`syntax-gallery.bpfman`](syntax-gallery.bpfman) — a deliberately
  inert `.bpfman` file that exercises every construct in the
  language. Use it to verify highlighting after changes to the mode.
  It is not intended to be evaluated.

## Installing

### Load directly

```elisp
(load-file "/path/to/go-bpfman/emacs/bpfman-mode.el")
```

### Via `load-path`

```elisp
(add-to-list 'load-path "/path/to/go-bpfman/emacs/")
(require 'bpfman-mode)
```

### With `use-package`

```elisp
(use-package bpfman-mode
  :load-path "/path/to/go-bpfman/emacs/"
  :mode "\\.bpfman\\'")
```

The mode auto-associates with files ending in `.bpfman`. Invoke
`M-x bpfman-mode` to force it on a buffer that is not so named.

## Verifying highlighting

Open `syntax-gallery.bpfman` in Emacs:

```
emacs syntax-gallery.bpfman
```

Scroll top to bottom. The gallery is organised into 18 numbered
sections, each comment-delimited. Scan each section and confirm
tokens are fontified according to this table:

| Face                          | Applied to                                                      |
|-------------------------------|-----------------------------------------------------------------|
| `font-lock-keyword-face`      | `let`, `if`, `elif`, `else`, `alias`, `assert`, `exec`, `json`, `file`, `require`, `dump`, `vars`, `unset`, `source`, `help`, `version`, `bpfman` |
| `font-lock-builtin-face`      | domain subcommands (`program`, `link`, `show`, `attach`, ...), attach kinds (`xdp`, `tc`, `tracepoint`, ...), comparison operators (`eq`, `==`, `<`, ...), assertion verbs (`not-empty`, `ok`, `fail`, `path`, `contains`, `nil`, `not`, `true`, `false`) |
| `font-lock-variable-name-face`| `$var` references, braced `${var}`, the identifier after `let`, adapter refs (`file:$var`) |
| `font-lock-string-face`       | `"double-quoted"` and `'single-quoted'` strings                 |
| `font-lock-comment-face`      | `# to end of line` (outside quotes)                             |
| `font-lock-constant-face`     | `--long` and `-x` flags                                         |
| (no face)                     | plain argument-position words (paths, numeric IDs), `[ ] { } ;` delimiters |

Specific lines to watch:

- Section 3 covers every variable-reference form, including the
  braced `${name.path[0]}` syntax.
- Section 4 exercises single and three-deep nested `[cmd]`
  substitution. After the inner `]` closes, the trailing argument in
  `[exec echo [json parse '"a"'] [json parse '"b"']]` is an argument
  to `exec echo` (not a fresh command), so it should not be
  keyword-highlighted.
- Section 5 has both the word (textual) and symbol (numeric)
  comparison operators. Both must highlight as builtin.
- Section 11 shows `alias b = bpfman` — the `=` in `alias` is part
  of alias syntax rather than `let` assignment, and should display
  plainly.
- Section 17 places `=` and `==` on adjacent lines to verify the
  tokeniser distinguishes them.

## Maintaining

When the language gains or loses a keyword/builtin, update:

1. The relevant hash tables in `bpfman-mode.el`
   (`bpfman--commands` and `bpfman--subcommands`).
2. The state-machine transitions in `bpfman--fontify-line-tokens`
   if the new construct introduces a distinct statement position.
3. `syntax-gallery.bpfman` with a representative example.
4. The face table above if a new face is used.

Bump `bpfman-mode.el`'s `Version:` header on user-visible changes.
