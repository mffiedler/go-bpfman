;;; bpfman-mode.el --- Major mode for bpfman-shell scripts -*- lexical-binding: t; -*-

;; Version: 0.6.0
;; Keywords: languages, bpf
;; Package-Requires: ((emacs "25.1"))

;; This file provides a major mode for editing bpfman-shell scripts
;; (.bpfman files). The language has typed let-bindings for
;; expressions and command captures via the bind sigil '<-', the
;; lifecycle primitives 'guard' and 'defer', conditional blocks,
;; foreach/break/continue iteration, 'poll' retry blocks
;; with 'timeout' and 'every' clauses plus explicit 'retry',
;; value-returning user
;; commands via 'def name(params) { ... return EXPR }', logical
;; operators (and/or/not), parenthesised expressions,
;; value-threading (|>), variable references with path access,
;; string literals, flags, comments, list literals, and the
;; '<expr> matches { ... }' expression operator. See
;; cmd/bpfman-shell/shell/GRAMMAR.md for the full specification.

;;; Commentary:

;; bpfman-mode provides syntax highlighting and editing support for
;; bpfman-shell scripts. Statements are separated by ';' or newline;
;; if/elif/else blocks span multiple lines through brace-delimited
;; bodies.
;;
;; Language features:
;;
;;   Comments:     # to end of line (not inside quotes)
;;   Expression:   let pid = $prog.record.program_id
;;   Destructure:  let (a b) = [$foo.a $bar.b]
;;   Bind:         let prog <- bpfman program load --path foo.o
;;   Guard:        guard prog <- bpfman program load --path foo.o
;;   Defer:        defer bpfman program unload $prog
;;   Tuple bind:   let (rc prog) <- bpfman ...
;;   Discard:      let _ <- ip link del foo
;;   Control:      if EXPR { ... } elif EXPR { ... } else { ... }
;;   Iteration:    foreach item in $list { ... break/continue }
;;                 foreach (a b) in (zip $xs $ys) { ... }
;;   Poll:         poll timeout 5s every 250ms { BODY }
;;   Retry:        retry / retry "msg" / retry unless EXPR
;;   Def:          def attach(iface prog) { ... return $link }
;;   Return:       return EXPR (only inside def bodies)
;;   Matches:      $value matches { ... } or assert $x matches { ... }
;;   Lists:        [1 2 3], [$a $b (range 10)]
;;   Logical:      and, or, not, parenthesised (EXPR)
;;   Threading:    $xs |> jq ".foo"
;;   Variables:    $prog.id, $prog.maps[0].name, ${prog.id}
;;   Strings:      "double quoted", 'single quoted'
;;                 Interpolation inside double quotes via "${name}"
;;                 (bare variable) or "${$expr}" (expression
;;                 starting with '$'); single quotes keep '$'
;;                 literal.
;;   Flags:        --path, -m, --dry-run
;;   Commands:     registered builtins (bpfman, jq, file, exec,
;;                 print, range, zip, u32le, u64le, start, wait,
;;                 kill, jobs, reap, fire, trace, tempdir, net,
;;                 import, ...) plus user-defined commands;
;;                 unknown names fall through to subprocess
;;                 execution.
;;
;; Highlighting uses a custom font-lock matcher that parses each line
;; structurally, so tokens are fontified according to their position
;; and role rather than by pattern-matching keywords anywhere. Brace
;; openers reset the state machine so the first word inside a block
;; is highlighted as a command. The bind sigil '<-' and the thread
;; operator '|>' do the same for the command on their right-hand
;; side. Parentheses group expressions without disturbing the
;; surrounding state, except after 'let'/'guard' where '(' opens a
;; tuple-target list.
;;
;; Usage:
;;
;;   (require 'bpfman-mode)
;;
;; Files with the .bpfman extension are automatically associated with
;; this mode.

;;; Code:

(defgroup bpfman nil
  "Major mode for bpfman-shell scripts."
  :group 'languages
  :prefix "bpfman-")

;; ---- Word sets ----

(defconst bpfman--commands
  (let ((ht (make-hash-table :test 'equal)))
    (dolist (w '(;; binding and lifecycle
                 "let" "guard" "defer"
                 ;; control flow
                 "if" "elif" "else"
                 "foreach" "break" "continue"
                 ;; poll/retry and value-publishing exit from def
                 "poll" "retry" "return"
                 ;; user-defined commands
                 "def"
                 ;; domain gateway
                 "bpfman"
                 ;; assertion keywords
                 "assert" "require"
                 ;; pure-builtin call producers (jq, range, zip,
                 ;; u32le, u64le); the parser-visible registry
                 ;; that mirrors shell/syntax/purebuiltin.go
                 "jq" "range" "zip" "u32le" "u64le"
                 ;; shell builtins registered by
                 ;; internal/builtins/*.go
                 "defs" "exec" "file" "fire" "import" "jobs"
                 "kill" "net" "print" "reap" "start" "tempdir"
                 "trace" "wait"))
      (puthash w t ht))
    ht)
  "Hash table of top-level bpfman-shell keywords and commands.")

(defconst bpfman--subcommands
  (let ((ht (make-hash-table :test 'equal)))
    (dolist (w '(;; domain commands (after "bpfman" prefix)
                 "dispatcher" "doctor" "gc" "link" "list" "load"
                 "program" "programs" "show"
                 ;; subcommands
                 "attach" "checkup" "delete" "detach" "explain" "file"
                 "get" "image"
                 "status" "temp" "unload"
                 ;; attach types
                 "fentry" "fexit" "kprobe" "tc" "tcx" "tracepoint"
                 "uprobe" "xdp"
                 ;; show subviews
                 "links" "maps" "paths" "summary"
                 ;; assertion command heads ('assert ok CMD',
                 ;; 'assert fail CMD'). 'not' also appears here as
                 ;; the optional leading negation ('assert not ok
                 ;; CMD'). "true" and "false" are bare boolean
                 ;; literals, not predicates -- they get default
                 ;; face like other literals.
                 "fail" "not" "ok"
                 ;; named predicates registered as pure builtins
                 ;; (shell/syntax/purebuiltin.go). 'not-empty' is
                 ;; the expression-position unary predicate;
                 ;; 'path-exists' is the file-system predicate
                 ;; (renamed from the older 'path' verb).
                 "contains" "empty" "missing" "not-empty"
                 "null" "path-exists" "present"
                 ;; logical operators
                 "and" "or"
                 ;; foreach separator
                 "in"
                 ;; poll clause keywords (recognised only
                 ;; between 'poll' and the block body)
                 "timeout" "every" "unless"
                 ;; matches expression operator and its optional
                 ;; 'exhaustive' modifier
                 "matches" "exhaustive"
                 ;; comparison operators (semantics chosen by
                 ;; operand type: number-vs-number numeric,
                 ;; string-vs-string textual, bool-vs-bool only
                 ;; supports == and !=, cross-type errors)
                 "==" "!=" "<" "<=" ">" ">="
                 ;; arithmetic operators (binary '+', '-', '*',
                 ;; '/', '%'; unary '-' reuses the same token).
                 ;; '-' appears here too: the line tokeniser only
                 ;; emits a bare '-' when it is not part of a flag
                 ;; ('-x', '--long'), so a standalone dash in
                 ;; expression position fontifies as an operator.
                 "+" "-" "*" "/" "%"))
      (puthash w t ht))
    ht)
  "Hash table of bpfman subcommands, attach types, subviews, and operators.")

;; ---- Token kind constants ----

(defconst bpfman--tok-word 0)
(defconst bpfman--tok-varref 1)
(defconst bpfman--tok-flag 2)
(defconst bpfman--tok-assign 3)
(defconst bpfman--tok-string 4)
(defconst bpfman--tok-select 5)
(defconst bpfman--tok-adapter-ref 6)
(defconst bpfman--tok-delim 7)   ; { } ; |> <- -- resets state to command
(defconst bpfman--tok-block 8)   ; { } -- same role but block-scoped

(defconst bpfman--adapter-prefixes '("file")
  "Known adapter prefixes for inline file:$var syntax.")

;; ---- Line tokeniser ----

(defun bpfman--tokenise-line (bol eol)
  "Tokenise the buffer region from BOL to EOL.
Return a list of (KIND BEG END) triples.  Stops at an unquoted #."
  (let ((pos bol)
        tokens)
    (catch 'done
      (while (< pos eol)
        ;; Skip whitespace, including the newline at a backslash-
        ;; continuation boundary. The fontifier passes a logical-
        ;; line range that may span several physical lines glued
        ;; by '\\\n'; treating the embedded newlines as whitespace
        ;; lets the state machine carry flag and argument context
        ;; across the continuation.
        (goto-char pos)
        (skip-chars-forward " \t\r\n" eol)
        (setq pos (point))
        (when (>= pos eol) (throw 'done nil))
        (let ((ch (char-after pos)))
          (cond
           ;; Backslash before a newline: line continuation. Skip
           ;; the backslash and the newline so the next physical
           ;; line's content reads as a continuation of this one.
           ((and (= ch ?\\)
                 (< (1+ pos) eol)
                 (or (= (char-after (1+ pos)) ?\n)
                     (and (= (char-after (1+ pos)) ?\r)
                          (< (+ pos 2) eol)
                          (= (char-after (+ pos 2)) ?\n))))
            (setq pos (+ pos
                         (if (= (char-after (1+ pos)) ?\n) 2 3))))

           ;; Comment: stop tokenising.
           ((= ch ?#)
            (throw 'done nil))

           ;; Quoted string (single or double).  Double-quoted
           ;; strings honour the same backslash escapes the shell
           ;; tokeniser does (\\, \", \$, \n, \t, \r): a backslash
           ;; followed by any character consumes both, so an
           ;; embedded "\"" does not terminate the string at the
           ;; escaped quote.  Single-quoted strings are fully
           ;; literal -- a backslash inside them is just a
           ;; backslash, matching the runtime semantics.
           ((or (= ch ?\") (= ch ?'))
            (let ((quote-char ch)
                  (start pos))
              (setq pos (1+ pos))
              (while (and (< pos eol)
                          (/= (char-after pos) quote-char))
                (if (and (= quote-char ?\")
                         (= (char-after pos) ?\\)
                         (< (1+ pos) eol))
                    ;; Escape sequence in a double-quoted string:
                    ;; consume the backslash and the following
                    ;; character together so an escaped quote does
                    ;; not close the string.
                    (setq pos (+ pos 2))
                  (setq pos (1+ pos))))
              (when (< pos eol)
                (setq pos (1+ pos)))   ; consume closing quote
              (push (list bpfman--tok-string start pos) tokens)))

           ;; Thread operator: |>. Two-char token routed through the
           ;; delimiter channel so the state machine can both fontify
           ;; it and reset to command position (the RHS of |> is a
           ;; command).
           ((and (= ch ?|)
                 (< (1+ pos) eol)
                 (= (char-after (1+ pos)) ?>))
            (push (list bpfman--tok-delim pos (+ pos 2)) tokens)
            (setq pos (+ pos 2)))

           ;; Bind sigil: <-. Two-char token, same delimiter channel
           ;; as |>: the RHS is a command form, so fontify as a
           ;; keyword and reset the state machine to command
           ;; position. A bare '<' or '<=' is a comparison operator
           ;; and falls through to the word path.
           ((and (= ch ?<)
                 (< (1+ pos) eol)
                 (= (char-after (1+ pos)) ?-))
            (push (list bpfman--tok-delim pos (+ pos 2)) tokens)
            (setq pos (+ pos 2)))

           ;; Statement separator, block, or expression-group
           ;; delimiter. '{' and ';' reset the structural state so
           ;; the next word is treated as a command; '(' and ')'
           ;; are expression grouping (or tuple-target delimiters
           ;; after let/guard) and the fontifier inspects state to
           ;; decide. Brackets ('[' ']') are not DSL delimiters --
           ;; the only legal place for them is inside a varref's
           ;; '[N]' index, which the varref tokeniser consumes
           ;| internally; an unquoted bracket here is a runtime
           ;; tokenisation error but is silently passed through by
           ;; the editor so a mid-edit buffer does not blow up.
           ((or (= ch ?{) (= ch ?})
                (= ch ?\() (= ch ?\))
                (= ch ?\;))
            (push (list bpfman--tok-delim pos (1+ pos)) tokens)
            (setq pos (1+ pos)))
           ((or (= ch ?\[) (= ch ?\]))
            (setq pos (1+ pos)))

           ;; Variable reference: $name.path or ${name.path}.
           ((= ch ?$)
            (let ((start pos))
              (setq pos (1+ pos))
              (if (and (< pos eol) (= (char-after pos) ?{))
                  ;; Braced form.
                  (progn
                    (setq pos (1+ pos))
                    (while (and (< pos eol) (/= (char-after pos) ?}))
                      (setq pos (1+ pos)))
                    (when (< pos eol)
                      (setq pos (1+ pos))) ; consume }
                    (push (list bpfman--tok-varref start pos) tokens))
                ;; Bare form: $ident(.field|[n])*
                (when (and (< pos eol)
                           (let ((c (char-after pos)))
                             (or (and (>= c ?a) (<= c ?z))
                                 (and (>= c ?A) (<= c ?Z))
                                 (= c ?_))))
                  (goto-char pos)
                  (skip-chars-forward "a-zA-Z0-9_" eol)
                  ;; Consume .field and [n] segments.
                  (while (and (< (point) eol)
                              (let ((c (char-after (point))))
                                (or (= c ?.) (= c ?\[))))
                    (if (= (char-after (point)) ?.)
                        (progn
                          (forward-char 1)
                          (skip-chars-forward "a-zA-Z0-9_" eol))
                      ;; [n]
                      (forward-char 1)
                      (skip-chars-forward "0-9" eol)
                      (when (and (< (point) eol)
                                 (= (char-after (point)) ?\]))
                        (forward-char 1))))
                  (setq pos (point))
                  (push (list bpfman--tok-varref start pos) tokens)))))

           ;; = or == distinction: == is a comparison operator (word),
           ;; = alone is an assignment operator.
           ((= ch ?=)
            (if (and (< (1+ pos) eol) (= (char-after (1+ pos)) ?=))
                (progn
                  (push (list bpfman--tok-word pos (+ pos 2)) tokens)
                  (setq pos (+ pos 2)))
              (push (list bpfman--tok-assign pos (1+ pos)) tokens)
              (setq pos (1+ pos))))

           ;; Flag: --long or -x (short).
           ((and (= ch ?-)
                 (< (1+ pos) eol)
                 (let ((next (char-after (1+ pos))))
                   (or (= next ?-)
                       (and (>= next ?a) (<= next ?z))
                       (and (>= next ?A) (<= next ?Z)))))
            (let ((start pos))
              (setq pos (1+ pos))
              (goto-char pos)
              (if (and (< pos eol) (= (char-after pos) ?-))
                  ;; Long flag: --word(-word)*.
                  (progn
                    (setq pos (1+ pos))
                    (goto-char pos)
                    (skip-chars-forward "a-zA-Z0-9" eol)
                    (while (and (< (point) eol)
                                (= (char-after (point)) ?-)
                                (< (1+ (point)) eol)
                                (let ((c (char-after (1+ (point)))))
                                  (or (and (>= c ?a) (<= c ?z))
                                      (and (>= c ?A) (<= c ?Z))
                                      (and (>= c ?0) (<= c ?9)))))
                      (forward-char 1)
                      (skip-chars-forward "a-zA-Z0-9" eol))
                    (setq pos (point)))
                ;; Short flag: -x (single letter).
                (skip-chars-forward "a-zA-Z" eol)
                (setq pos (point)))
              (push (list bpfman--tok-flag start pos) tokens)))

           ;; Plain word (with adapter-ref detection).
           (t
            (let ((start pos))
              (goto-char pos)
              (skip-chars-forward "^ \t\n#'\"$[](){}|;" eol)
              (setq pos (point))
              (when (> pos start)
                ;; Check for adapter prefix: word ends with
                ;; "<adapter>:" and next char is $.
                (let ((text (buffer-substring-no-properties start pos))
                      (is-adapter nil))
                  (when (and (< pos eol) (= (char-after pos) ?$))
                    (dolist (prefix bpfman--adapter-prefixes)
                      (when (string= text (concat prefix ":"))
                        (setq is-adapter t))))
                  (if is-adapter
                      ;; Consume the $varref part too.
                      (progn
                        (setq pos (1+ pos)) ; skip $
                        (if (and (< pos eol) (= (char-after pos) ?{))
                            ;; Braced form.
                            (progn
                              (setq pos (1+ pos))
                              (while (and (< pos eol) (/= (char-after pos) ?}))
                                (setq pos (1+ pos)))
                              (when (< pos eol)
                                (setq pos (1+ pos)))) ; consume }
                          ;; Bare form: ident(.field|[n])*
                          (when (and (< pos eol)
                                     (let ((c (char-after pos)))
                                       (or (and (>= c ?a) (<= c ?z))
                                           (and (>= c ?A) (<= c ?Z))
                                           (= c ?_))))
                            (goto-char pos)
                            (skip-chars-forward "a-zA-Z0-9_" eol)
                            (while (and (< (point) eol)
                                        (let ((c (char-after (point))))
                                          (or (= c ?.) (= c ?\[))))
                              (if (= (char-after (point)) ?.)
                                  (progn
                                    (forward-char 1)
                                    (skip-chars-forward "a-zA-Z0-9_" eol))
                                (forward-char 1)
                                (skip-chars-forward "0-9" eol)
                                (when (and (< (point) eol)
                                           (= (char-after (point)) ?\]))
                                  (forward-char 1))))
                            (setq pos (point))))
                        (push (list bpfman--tok-adapter-ref start pos) tokens))
                    ;; Normal word.
                    (push (list (if (string= text "select")
                                    bpfman--tok-select
                                  bpfman--tok-word)
                                start pos)
                          tokens))))))))))
    (nreverse tokens)))

;; ---- Structural font-lock ----

(defun bpfman--fontify-interp-string (beg end)
  "Fontify a string token in [BEG, END) with interpolation awareness.
Literal runs (including the enclosing quote marks) get
`font-lock-string-face'. The \"${\" and \"}\" delimiters of
an interpolation get `font-lock-keyword-face' so they read as
operators against the surrounding string; the body in between
gets `font-lock-variable-name-face' -- typical bodies are
either a bare variable reference or an expression starting
with '$'. Only touches double-quoted strings; single-quoted
strings are fully literal and stay pure `font-lock-string-face'."
  (if (and (> end beg) (/= (char-after beg) ?\"))
      ;; Single-quoted (or the degenerate empty range): keep the
      ;; simple behaviour.
      (put-text-property beg end 'face 'font-lock-string-face)
    (let ((pos beg)
          (lit-start beg))
      (while (< pos end)
        (cond
         ;; Backslash escape inside a double-quoted string consumes
         ;; the next character too, so `\$' does not trigger the
         ;; interpolation scanner.  The runtime collapses the
         ;; backslash and the following character into the decoded
         ;; literal; here we just need to skip past the pair so the
         ;; literal/interp split lines up.
         ((and (= (char-after pos) ?\\) (< (1+ pos) end))
          (setq pos (+ pos 2)))
         ;; Interpolation start: "${...}".
         ((and (= (char-after pos) ?$)
               (< (1+ pos) end)
               (= (char-after (1+ pos)) ?{))
          ;; Flush the literal run that precedes this "${".
          (when (< lit-start pos)
            (put-text-property lit-start pos 'face 'font-lock-string-face))
          ;; Face the "${" opener.
          (put-text-property pos (+ pos 2) 'face 'font-lock-keyword-face)
          ;; Locate the matching "}" using a brace-depth counter
          ;; so nested braces (unlikely today, but cheap to
          ;; support) do not close the interpolation early.
          (let ((body-start (+ pos 2))
                (body-end (+ pos 2))
                (depth 1))
            (while (and (< body-end end) (> depth 0))
              (let ((c (char-after body-end)))
                (cond
                 ((= c ?{) (setq depth (1+ depth)))
                 ((= c ?}) (setq depth (1- depth)))))
              (unless (= depth 0)
                (setq body-end (1+ body-end))))
            (if (and (< body-end end) (= depth 0))
                (progn
                  (when (> body-end body-start)
                    (put-text-property body-start body-end
                                       'face 'font-lock-variable-name-face))
                  (put-text-property body-end (1+ body-end)
                                     'face 'font-lock-keyword-face)
                  (setq pos (1+ body-end))
                  (setq lit-start pos))
              ;; Unterminated "${..." — keep the remainder as a
              ;; string so the line still reads as a string in
              ;; the common case of mid-edit state.
              (put-text-property pos end 'face 'font-lock-string-face)
              (setq pos end)
              (setq lit-start end))))
         ;; Plain literal character: advance one byte.
         (t
          (setq pos (1+ pos)))))
      (when (< lit-start end)
        (put-text-property lit-start end 'face 'font-lock-string-face)))))

(defun bpfman--fontify-line-tokens (tokens)
  "Apply faces to TOKENS based on their structural role in the line.
TOKENS is a list of (KIND BEG END) as returned by `bpfman--tokenise-line'."
  (when tokens
    (let ((rest tokens)
          ;; Possible states: start, let-name, let-eq, command,
          ;; subcommand, args.
          (state 'start)
          tok kind beg end)
      (while rest
        (setq tok (car rest)
              kind (nth 0 tok)
              beg (nth 1 tok)
              end (nth 2 tok)
              rest (cdr rest))
        (cond
         ;; Strings.  Double-quoted strings get interp-aware
         ;; fontification so "${...}" segments stand out from the
         ;; surrounding literal runs; single-quoted strings and the
         ;; degenerate empty case stay pure string-face.
         ((= kind bpfman--tok-string)
          (bpfman--fontify-interp-string beg end))

         ;; Variable references are always variable-name face.
         ((= kind bpfman--tok-varref)
          (put-text-property beg end 'face 'font-lock-variable-name-face)
          (when (eq state 'start)
            (setq state 'args)))

         ;; Adapter references (file:$var.path) are variable-name face.
         ((= kind bpfman--tok-adapter-ref)
          (put-text-property beg end 'face 'font-lock-variable-name-face)
          (when (eq state 'start)
            (setq state 'args)))

         ;; Flags are always constant face.
         ((= kind bpfman--tok-flag)
          (put-text-property beg end 'face 'font-lock-constant-face)
          (when (memq state '(start saw-ident))
            (setq state 'args)))

         ;; `select' projection keyword.
         ((= kind bpfman--tok-select)
          (put-text-property beg end 'face 'font-lock-keyword-face)
          (setq state 'args))

         ;; Delimiter: { } ; ( ) |> <-. Block openers '{' and
         ;; statement separator ';' reset to command position; the
         ;; block closer '}' does the same so the first word of the
         ;; following statement is treated as a command. Parens '('
         ;; and ')' are expression grouping (or, after let/guard,
         ;; tuple-target delimiters), and the fontifier consults
         ;; state to decide. '|>' is the thread operator and '<-'
         ;; is the bind sigil: both fontify as keywords and reset
         ;; to command position because the RHS is a command form.
         ((= kind bpfman--tok-delim)
          (let ((dch (char-after beg)))
            (cond
             ((= dch ?|)
              (put-text-property beg end 'face 'font-lock-keyword-face)
              (setq state 'start))
             ((= dch ?<)
              (put-text-property beg end 'face 'font-lock-keyword-face)
              (setq state 'start))
             ;; `(` after `def NAME` opens the parameter list; switch
             ;; to a sub-state that fontifies words inside as
             ;; parameter names.
             ((and (= dch ?\() (eq state 'def-params))
              (setq state 'def-params-list))
             ;; `)` closing the def parameter list returns to start;
             ;; the body's `{` (a delimiter that also resets to
             ;; start) follows.
             ((and (= dch ?\)) (eq state 'def-params-list))
              (setq state 'start))
             ;; `(` after `let` or `guard` opens a tuple-target list
             ;; or destructure-target list: '(rc prim) <- COMMAND'
             ;; or '(a b c) = EXPR'. Fontify words inside as
             ;; variable names up to the closing ')'; the trailing
             ;; '<-' / '=' then resets the state. The same shape
             ;; covers `foreach (a b) in EXPR`, so the open paren
             ;; in `foreach-name` state enters the same sub-state.
             ((and (= dch ?\() (or (eq state 'let-name)
                                   (eq state 'foreach-name)))
              (setq state 'tuple-targets))
             ((and (= dch ?\)) (eq state 'tuple-targets))
              (setq state 'args))
             ((or (= dch ?\() (= dch ?\))))
             (t
              (setq state 'start)))))

         ;; Assignment operator.
         ((= kind bpfman--tok-assign)
          (if (eq state 'let-eq)
              (progn
                (put-text-property beg end 'face 'font-lock-keyword-face)
                (setq state 'start))
            ;; Equals outside let context; treat as plain.
            (setq state 'args)))

         ;; Plain word: face depends on position.
         ((= kind bpfman--tok-word)
          (let ((text (buffer-substring-no-properties beg end)))
            (pcase state
              ('start
               ;; First word of a statement or block. "let" starts
               ;; a binding; "foreach" binds an iteration variable;
               ;; "if"/"elif"/"else"/"poll"/"retry"/"return" are
               ;; control flow; "break"/"continue" are standalone;
               ;; everything else is a command.
               (cond
                ((string= text "let")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'let-name))
                ((string= text "guard")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'let-name))
                ((string= text "defer")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'start))
                ((string= text "foreach")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'foreach-name))
                ((string= text "def")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'def-name))
                ((or (string= text "if")
                     (string= text "elif")
                     (string= text "else")
                     (string= text "poll")
                     (string= text "retry")
                     (string= text "return"))
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'args))
                ((or (string= text "break")
                     (string= text "continue"))
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'args))
                (t
                 (when (gethash text bpfman--commands)
                   (put-text-property beg end 'face 'font-lock-keyword-face))
                 (setq state 'subcommand))))

              ('let-name
               ;; Word after "let": variable name.
               (when (bpfman--ident-p text)
                 (put-text-property beg end 'face 'font-lock-variable-name-face))
               (setq state 'let-eq))

              ('foreach-name
               ;; Word after "foreach": iteration variable name.
               (when (bpfman--ident-p text)
                 (put-text-property beg end 'face 'font-lock-variable-name-face))
               (setq state 'args))

              ('def-name
               ;; Word after "def": user-defined command name.  The
               ;; opening '(' of the parameter list is handled by the
               ;; delimiter branch and switches state to
               ;; def-params-list.
               (when (bpfman--ident-p text)
                 (put-text-property beg end 'face 'font-lock-variable-name-face))
               (setq state 'def-params))

              ('def-params-list
               ;; Word inside a def parameter list. Parameter names
               ;; are whitespace-separated identifiers; the grammar
               ;; rejects any token whose text contains ','. Fontify
               ;; the identifier portion (up to the first non-ident
               ;; character) so a mid-edit "a," still highlights 'a'
               ;; even though the parser would reject it.
               (let ((ident-end end))
                 (save-excursion
                   (goto-char beg)
                   (skip-chars-forward "a-zA-Z0-9_" end)
                   (setq ident-end (point)))
                 (when (and (> ident-end beg)
                            (bpfman--ident-p
                             (buffer-substring-no-properties beg ident-end)))
                   (put-text-property beg ident-end
                                      'face 'font-lock-variable-name-face))))

              ('tuple-targets
               ;; Word inside a parenthesised name list. Three
               ;; surface forms share this state: the bind tuple
               ;; '(rc prim) <- CMD', the let-destructure
               ;; '(a b ...) = EXPR', and the multi-var foreach
               ;; 'foreach (a b ...) in EXPR'. All accept
               ;; whitespace-separated identifiers and the discard
               ;; marker '_'; comma is rejected by the parser.
               (let ((ident-end end))
                 (save-excursion
                   (goto-char beg)
                   (skip-chars-forward "a-zA-Z0-9_" end)
                   (setq ident-end (point)))
                 (when (and (> ident-end beg)
                            (bpfman--ident-p
                             (buffer-substring-no-properties beg ident-end)))
                   (put-text-property beg ident-end
                                      'face 'font-lock-variable-name-face))))

              ('let-eq
               ;; Expected = after variable name; got a word instead.
               ;; Fall through to args.
               (setq state 'args))

              ('subcommand
               ;; Second word after command: subcommand position.
               (when (gethash text bpfman--subcommands)
                 (put-text-property beg end 'face 'font-lock-builtin-face))
               (setq state 'args))

              ('args
               ;; In argument position: subcommands (comparison
               ;; operators and assertion verbs) still get the
               ;; builtin face so `$a == $b' and `assert not-empty
               ;; $x' read right.
               (when (gethash text bpfman--subcommands)
                 (put-text-property beg end 'face 'font-lock-builtin-face)))))))))))

(defun bpfman--ident-p (str)
  "Return non-nil if STR is a valid bpfman identifier."
  (and (> (length str) 0)
       (string-match-p "\\`[a-zA-Z_][a-zA-Z0-9_]*\\'" str)))

(defun bpfman--logical-line-end (bol)
  "Return the buffer position of the end of the logical line starting at BOL.
A logical line is a physical line plus any following physical
lines joined by an unescaped trailing backslash, with quote
state tracked across the boundary so a backslash inside a
single-quoted string does not look like a continuation. Comments
truncate the line for the continuation check; a '#' inside
quotes is literal text and does not end the line."
  (save-excursion
    (goto-char bol)
    (let ((in-single nil)
          (in-double nil))
      (catch 'done
        (while t
          (let ((eol (line-end-position))
                (last-non-space nil))
            (while (< (point) eol)
              (let ((ch (char-after)))
                (cond
                 ;; In a double-quoted string, '\\X' is an escape
                 ;; pair: the runtime decodes \n, \t, \", \$, etc.
                 ;; Outside strings or in a single-quoted span, a
                 ;; bare '\\' is just one character (handled by the
                 ;; default branch below).
                 ((and (= ch ?\\) in-double (< (1+ (point)) eol))
                  (forward-char 2)
                  (setq last-non-space (1- (point))))
                 ((and (= ch ?\') (not in-double))
                  (setq in-single (not in-single))
                  (forward-char 1)
                  (setq last-non-space (1- (point))))
                 ((and (= ch ?\") (not in-single))
                  (setq in-double (not in-double))
                  (forward-char 1)
                  (setq last-non-space (1- (point))))
                 ((and (= ch ?#) (not in-single) (not in-double))
                  ;; A trailing comment cannot host a continuation:
                  ;; a backslash in the comment body is literal.
                  ;; Skip to EOL without recording it as
                  ;; non-whitespace.
                  (goto-char eol))
                 ((or (= ch ?\s) (= ch ?\t) (= ch ?\r))
                  (forward-char 1))
                 (t
                  (forward-char 1)
                  (setq last-non-space (1- (point)))))))
            (if (and last-non-space
                     (= (char-after last-non-space) ?\\)
                     (not in-single)
                     (not in-double)
                     (< eol (point-max)))
                (progn
                  (forward-char 1)
                  ;; Loop re-assesses next physical line.
                  )
              (throw 'done eol))))))))

(defun bpfman--fontify-region (beg end)
  "Fontify the buffer region from BEG to END one logical line at a time.
Backslash-continued lines fontify as one logical line so the
state machine carries flag/argument context across the
continuation; multi-line guard or load statements highlight
the same way they read."
  (save-excursion
    (goto-char beg)
    (beginning-of-line)
    (while (< (point) end)
      (let* ((bol (line-beginning-position))
             (lol (bpfman--logical-line-end bol)))
        (bpfman--fontify-line-tokens
         (bpfman--tokenise-line bol lol))
        (goto-char lol)
        (forward-line 1)))))

;; ---- Syntax table ----

(defvar bpfman-mode-syntax-table
  (let ((st (make-syntax-table)))
    ;; # starts a comment to end of line.
    (modify-syntax-entry ?# "<" st)
    (modify-syntax-entry ?\n ">" st)
    ;; Double-quoted strings.
    (modify-syntax-entry ?\" "\"" st)
    ;; Single-quoted strings.
    (modify-syntax-entry ?' "\"" st)
    ;; Underscores are word constituents.
    (modify-syntax-entry ?_ "w" st)
    ;; Brackets, braces, and parens are paired delimiters for
    ;; show-paren-mode.
    (modify-syntax-entry ?\[ "(]" st)
    (modify-syntax-entry ?\] ")[" st)
    (modify-syntax-entry ?{ "(}" st)
    (modify-syntax-entry ?} "){" st)
    (modify-syntax-entry ?\( "()" st)
    (modify-syntax-entry ?\) ")(" st)
    st)
  "Syntax table for `bpfman-mode'.")

;; ---- Mode definition ----

;;;###autoload
(define-derived-mode bpfman-mode prog-mode "Bpfman"
  "Major mode for editing bpfman-shell scripts.

Statements bind values via 'let NAME = EXPR' (expression
binding) or 'let NAME <- COMMAND' (capture a command's primary
result), with 'guard NAME <- COMMAND' as the halt-on-failure
variant. Tuple targets '(rc prim)' bind both the result
envelope and the primary; the destructure form '(a b ...)'
positionally binds list elements; '_' discards a slot at any
binding site. 'defer COMMAND' registers a cleanup that runs
LIFO at scope exit. Control statements are if-blocks, foreach
iteration (single-var 'foreach x in EXPR' or multi-var
'foreach (a b) in EXPR'), 'poll timeout DUR every DUR { BODY }'
retry blocks with explicit 'retry', and 'def NAME(PARAMS) { BODY }'
for user-defined commands. A def body may finish with 'return
EXPR' to publish a value to bind-position callers. Statements
are separated by a newline or ';'. Comments begin with '#' and
extend to end of line.

Expressions support logical operators ('and', 'or', 'not'),
parenthesised grouping, and the thread operator '|>' which
feeds the LHS as the last argument of the RHS command. List
literals are bracket-delimited and whitespace-separated:
'[1 2 3]', '[$a $b (range 10)]'. The 'matches' operator on the
expression layer attaches a structural matcher to the LHS
value: 'EXPR matches { ... }' or 'EXPR matches exhaustive { ... }'.

Variable references use the '$' sigil: $prog.id,
${prog.maps[0].name}, ${$n * 2}.

Strings are single- or double-quoted. Single quotes are fully
literal; double-quoted strings support '${...}' interpolation
where the braces contain either a bare variable reference
('${name}', '${name.path}') or an expression that begins with
'$' ('${$n * 2}', '${$x |> jq \".y\"}'). A bare '$' inside a
double-quoted string is a lex-time error; use single quotes
when you need a literal '$'.

\\{bpfman-mode-map}"
  :syntax-table bpfman-mode-syntax-table
  (setq-local comment-start "# ")
  (setq-local comment-end "")
  (setq-local comment-start-skip "#+ *")
  ;; font-lock handles comments and strings via the syntax table
  ;; (syntactic fontification).  Structural highlighting -- commands,
  ;; subcommands, variables, flags -- is layered on top by
  ;; jit-lock-register, which runs our fontifier after font-lock's
  ;; syntactic pass.
  (setq-local font-lock-defaults '(nil))
  (jit-lock-register #'bpfman--fontify-region)
  (setq-local indent-tabs-mode nil))

;;;###autoload
(add-to-list 'auto-mode-alist '("\\.bpfman\\'" . bpfman-mode))

(provide 'bpfman-mode)
;;; bpfman-mode.el ends here
