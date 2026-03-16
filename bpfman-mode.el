;;; bpfman-mode.el --- Major mode for bpfman REPL scripts -*- lexical-binding: t; -*-

;; Version: 0.2.0
;; Keywords: languages, bpf
;; Package-Requires: ((emacs "25.1"))

;; This file provides a major mode for editing bpfman REPL scripts
;; (.bpfman files).  The bpfman REPL language supports variable
;; assignment, structured field access, string literals, flags, and
;; comments.  See docs/REPL-LANG.md for the full specification.

;;; Commentary:

;; bpfman-mode provides syntax highlighting and editing support for
;; bpfman REPL scripts.  The language is line-oriented: one line, one
;; command, one result.
;;
;; Language features:
;;
;;   Comments:     # to end of line (not inside quotes)
;;   Assignment:   let prog = load file --path foo.o
;;   Variables:    $prog.id, $prog.maps[0].name, ${prog.id}
;;   Strings:      "double quoted", 'single quoted'
;;   Flags:        --path, -m, --dry-run
;;   Commands:     load, show, program, link, etc.
;;
;; Highlighting uses a custom font-lock matcher that parses each line
;; structurally, so tokens are fontified according to their position
;; and role rather than by pattern-matching keywords anywhere.
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
    (dolist (w '("assert" "dispatcher" "doctor" "dump" "exec" "file"
                 "gc" "help" "json" "let" "link" "list" "load"
                 "program" "programs" "require" "set" "show" "source"
                 "unset" "vars" "version"))
      (puthash w t ht))
    ht)
  "Hash table of top-level bpfman REPL commands.")

(defconst bpfman--subcommands
  (let ((ht (make-hash-table :test 'equal)))
    (dolist (w '(;; subcommands
                 "attach" "checkup" "delete" "detach" "explain" "file"
                 "get" "image" "list" "load" "parse" "program"
                 "programs" "status" "temp" "unload"
                 ;; attach types
                 "fentry" "fexit" "kprobe" "tc" "tcx" "tracepoint"
                 "uprobe" "xdp"
                 ;; show subviews
                 "links" "maps" "paths" "summary"
                 ;; assertion verbs (assert/require)
                 "contains" "eq" "fail" "false" "ge" "gt" "le"
                 "lt" "ne" "nil" "not" "not-empty" "ok" "path"
                 "true"))
      (puthash w t ht))
    ht)
  "Hash table of bpfman subcommands, attach types, and subviews.")

;; ---- Token kind constants ----

(defconst bpfman--tok-word 0)
(defconst bpfman--tok-varref 1)
(defconst bpfman--tok-flag 2)
(defconst bpfman--tok-assign 3)
(defconst bpfman--tok-string 4)
(defconst bpfman--tok-select 5)
(defconst bpfman--tok-adapter-ref 6)

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

           ;; Standalone = (assignment operator).
           ((= ch ?=)
            (push (list bpfman--tok-assign pos (1+ pos)) tokens)
            (setq pos (1+ pos)))

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
              (skip-chars-forward "^ \t\n#'\"$" eol)
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
          (state 'start)  ; start -> saw-ident -> command -> args
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
          ;; A varref in start position means this is a command line,
          ;; not an assignment.
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

         ;; Assignment operator.
         ((= kind bpfman--tok-assign)
          (if (eq state 'let-eq)
              (progn
                (put-text-property beg end 'face 'font-lock-keyword-face)
                (setq state 'command))
            ;; Equals outside let/set context; treat as plain.
            (setq state 'args)))

         ;; Plain word: face depends on position.
         ((= kind bpfman--tok-word)
          (let ((text (buffer-substring-no-properties beg end)))
            (pcase state
              ('start
               ;; First word.  "let" starts a let-assignment;
               ;; "set" starts a set-binding; everything else is
               ;; a command.
               (cond
                ((string= text "let")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'let-name))
                ((string= text "set")
                 (put-text-property beg end 'face 'font-lock-keyword-face)
                 (setq state 'let-name))
                (t
                 (when (gethash text bpfman--commands)
                   (put-text-property beg end 'face 'font-lock-keyword-face))
                 (setq state 'subcommand))))

              ('let-name
               ;; Word after "let"/"set": variable name.
               (when (bpfman--ident-p text)
                 (put-text-property beg end 'face 'font-lock-variable-name-face))
               (setq state 'let-eq))

              ('let-eq
               ;; Expected = after variable name; got a word instead.
               ;; Fall through to args.
               (setq state 'args))

              ('command
               ;; First word after `=': command position.
               (when (gethash text bpfman--commands)
                 (put-text-property beg end 'face 'font-lock-keyword-face))
               (setq state 'subcommand))

              ('subcommand
               ;; Second word after command: subcommand position.
               (when (gethash text bpfman--subcommands)
                 (put-text-property beg end 'face 'font-lock-builtin-face))
               (setq state 'args))

              ('args
               ;; In argument position: no special highlighting for
               ;; plain words.
               nil)))))))))

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
    st)
  "Syntax table for `bpfman-mode'.")

;; ---- Mode definition ----

;;;###autoload
(define-derived-mode bpfman-mode prog-mode "Bpfman"
  "Major mode for editing bpfman REPL scripts.

The bpfman REPL language is line-oriented.  Each line is either a
let-assignment (let name = command ...), a set-binding
(set name = value), an assertion, or a plain command.  Comments
begin with # and extend to end of line.

Variable references use the $ sigil: $prog.id, ${prog.maps[0].name}.
Strings may be single- or double-quoted; $ is literal inside quotes.

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
