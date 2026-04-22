;;; bpfman-mode.el --- Major mode for bpfman REPL scripts -*- lexical-binding: t; -*-

;; Version: 0.4.0
;; Keywords: languages, bpf
;; Package-Requires: ((emacs "25.1"))

;; This file provides a major mode for editing bpfman REPL scripts
;; (.bpfman files).  The bpfman REPL language has typed let-bindings,
;; conditional blocks, foreach/break/continue iteration,
;; retry/until/timeout polling, logical operators (and/or/not),
;; parenthesised expressions, command substitution, value-threading
;; (|>), variable references with path access, string literals,
;; flags, and comments.  See docs/repl/language.md for the full
;; specification.

;;; Commentary:

;; bpfman-mode provides syntax highlighting and editing support for
;; bpfman REPL scripts.  Statements are separated by ';' or newline;
;; if/elif/else blocks span multiple lines through brace-delimited
;; bodies.
;;
;; Language features:
;;
;;   Comments:     # to end of line (not inside quotes)
;;   Binding:      let prog = [bpfman load file --path foo.o ...]
;;   Literal RHS:  let iface = "eth0" / let pid = $prog.record.id
;;   Cmdsub:       let r = [exec ip link show]
;;   Control:      if EXPR { ... } elif EXPR { ... } else { ... }
;;   Iteration:    foreach item in $list { ... break/continue }
;;   Polling:      retry { ... } until EXPR (timeout 60s, iteration 5)
;;   Logical:      and, or, not, parenthesised (EXPR)
;;   Threading:    $xs |> jq ".foo" |> assert not-empty
;;   Variables:    $prog.id, $prog.maps[0].name, ${prog.id}
;;   Strings:      "double quoted", 'single quoted'
;;   Flags:        --path, -m, --dry-run
;;   Commands:     bpfman (domain gateway), assert, exec, jq, file, ...
;;
;; Highlighting uses a custom font-lock matcher that parses each line
;; structurally, so tokens are fontified according to their position
;; and role rather than by pattern-matching keywords anywhere.  Bracket
;; and brace openers reset the state machine so the first word inside
;; is highlighted as a command.  The thread operator |> does the same
;; for the command on its right-hand side.  Parentheses group
;; expressions without disturbing the surrounding state.
;;
;; Usage:
;;
;;   (require 'bpfman-mode)
;;
;; Files with the .bpfman extension are automatically associated with
;; this mode.

;;; Code:

(defgroup bpfman nil
  "Major mode for bpfman REPL scripts."
  :group 'languages
  :prefix "bpfman-")

;; ---- Word sets ----

(defconst bpfman--commands
  (let ((ht (make-hash-table :test 'equal)))
    (dolist (w '(;; binding and control flow
                 "let" "if" "elif" "else"
                 "foreach" "retry" "until" "break" "continue"
                 ;; domain gateway
                 "bpfman"
                 ;; shell-language builtins
                 "alias" "aliases" "assert" "dump" "exec" "file"
                 "help" "jq" "require" "source"
                 "unalias" "unset" "vars" "version"))
      (puthash w t ht))
    ht)
  "Hash table of top-level bpfman REPL keywords and commands.")

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
                 ;; assertion verbs (prefix unary / command-status)
                 "contains" "fail" "false" "nil" "not" "not-empty"
                 "ok" "path" "true"
                 ;; logical operators
                 "and" "or"
                 ;; foreach / retry auxiliaries
                 "in" "timeout" "iteration"
                 ;; comparison operators: textual (lexicographic)
                 "eq" "ne" "lt" "le" "gt" "ge"
                 ;; comparison operators: numeric
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
(defconst bpfman--tok-delim 7)   ; [ ] { } ; — resets state to command
(defconst bpfman--tok-block 8)   ; {  } — same role but block-scoped

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
        ;; Skip whitespace.
        (goto-char pos)
        (skip-chars-forward " \t" eol)
        (setq pos (point))
        (when (>= pos eol) (throw 'done nil))
        (let ((ch (char-after pos)))
          (cond
           ;; Comment: stop tokenising.
           ((= ch ?#)
            (throw 'done nil))

           ;; Quoted string (single or double).
           ((or (= ch ?\") (= ch ?'))
            (let ((quote-char ch)
                  (start pos))
              (setq pos (1+ pos))
              (while (and (< pos eol)
                          (/= (char-after pos) quote-char))
                (setq pos (1+ pos)))
              (when (< pos eol)
                (setq pos (1+ pos)))   ; consume closing quote
              (push (list bpfman--tok-string start pos) tokens)))

           ;; Thread operator: |>.  Two-char token routed through the
           ;; delimiter channel so the state machine can both fontify
           ;; it and reset to command position (the RHS of |> is a
           ;; command).
           ((and (= ch ?|)
                 (< (1+ pos) eol)
                 (= (char-after (1+ pos)) ?>))
            (push (list bpfman--tok-delim pos (+ pos 2)) tokens)
            (setq pos (+ pos 2)))

           ;; Statement separator, command-substitution, block, or
           ;; expression-group delimiter.  Most of these reset the
           ;; structural state so the next word is treated as a
           ;; command; ( ) are expression grouping and leave state
           ;; alone (handled in the fontifier).
           ((or (= ch ?\[) (= ch ?\])
                (= ch ?{)  (= ch ?})
                (= ch ?\() (= ch ?\))
                (= ch ?\;))
            (push (list bpfman--tok-delim pos (1+ pos)) tokens)
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
         ;; Strings are always string face.
         ((= kind bpfman--tok-string)
          (put-text-property beg end 'face 'font-lock-string-face))

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

         ;; Delimiter: [ ] { } ; ( ) |>.  Most open delimiters ([ { ;)
         ;; and block close } reset to command position so the next
         ;; word inside a command substitution or block body -- or
         ;; the first word after a statement separator, elif/else, or
         ;; a thread operator -- is treated as a command.  The close
         ;; bracket `]' returns to argument position so words trailing
         ;; a nested cmdsub are not mistaken for commands.  Parens
         ;; ( ) are expression grouping and leave the surrounding
         ;; state untouched.  `|>' is the thread operator: highlight
         ;; as a keyword and reset to command position (the RHS of
         ;; a thread is a command).
         ((= kind bpfman--tok-delim)
          (let ((dch (char-after beg)))
            (cond
             ((= dch ?|)
              (put-text-property beg end 'face 'font-lock-keyword-face)
              (setq state 'start))
             ((= dch ?\])
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
               ;; First word of a statement or block.  "let" starts
               ;; a binding; "foreach" binds an iteration variable;
               ;; "if"/"elif"/"else"/"retry"/"until" are control
               ;; flow; "break"/"continue" are standalone; everything
               ;; else is a command.
               (cond
                ((string= text "let")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'let-name))
                ((string= text "foreach")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'foreach-name))
                ((or (string= text "if")
                     (string= text "elif")
                     (string= text "else")
                     (string= text "retry")
                     (string= text "until"))
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
               ;; builtin face so `$a eq $b' and `assert not-empty
               ;; $x' read right.
               (when (gethash text bpfman--subcommands)
                 (put-text-property beg end 'face 'font-lock-builtin-face)))))))))))

(defun bpfman--ident-p (str)
  "Return non-nil if STR is a valid bpfman identifier."
  (and (> (length str) 0)
       (string-match-p "\\`[a-zA-Z_][a-zA-Z0-9_]*\\'" str)))

(defun bpfman--fontify-region (beg end)
  "Fontify the buffer region from BEG to END line by line."
  (save-excursion
    (goto-char beg)
    (beginning-of-line)
    (while (< (point) end)
      (let ((bol (line-beginning-position))
            (eol (line-end-position)))
        (bpfman--fontify-line-tokens
         (bpfman--tokenise-line bol eol))
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
  "Major mode for editing bpfman REPL scripts.

Statements are let-bindings (let name = expr), if-blocks
(if EXPR { ... } elif EXPR { ... } else { ... }), foreach
iteration (foreach name in EXPR { ... }), retry/until polling
(retry { ... } until EXPR), or plain commands.  Statements are
separated by a newline or ';'.  Comments begin with # and
extend to end of line.

Expressions support logical operators (`and', `or', `not'),
parenthesised grouping, and the thread operator `|>' which
feeds the LHS as the last argument of the RHS command.  Within
a retry block, `timeout DURATION' and `iteration N' are
primary expressions that terminate polling.

Commands inside a let RHS or if-condition are wrapped in square
brackets: `let r = [exec ip link show]'.  Variable references use
the $ sigil: $prog.id, ${prog.maps[0].name}.  Strings may be
single- or double-quoted; $ is literal inside quotes.

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
