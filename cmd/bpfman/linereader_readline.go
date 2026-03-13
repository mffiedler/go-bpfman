package main

import "github.com/ergochat/readline"

// readlineCompleter adapts a CompleteFunc to readline's AutoCompleter
// interface.
type readlineCompleter struct {
	fn CompleteFunc
}

func (c *readlineCompleter) Do(line []rune, pos int) ([][]rune, int) {
	replace, candidates := c.fn(string(line), pos)
	suffixes := make([][]rune, len(candidates))
	for i, cand := range candidates {
		// Each candidate is a full replacement string; strip
		// the first `replace` runes to produce the suffix that
		// readline appends after removing `replace` runes
		// before the cursor.
		r := []rune(cand)
		if replace <= len(r) {
			suffixes[i] = r[replace:]
		} else {
			suffixes[i] = r
		}
	}
	return suffixes, replace
}

// readlineReader wraps a readline.Instance to implement LineReader.
type readlineReader struct {
	inst *readline.Instance
}

// NewLineReader creates a LineReader backed by ergochat/readline.
func NewLineReader(prompt, historyPath string, complete CompleteFunc) (LineReader, error) {
	cfg := &readline.Config{
		Prompt:       prompt,
		HistoryFile:  historyPath,
		AutoComplete: &readlineCompleter{fn: complete},
	}
	inst, err := readline.NewEx(cfg)
	if err != nil {
		return nil, err
	}
	return &readlineReader{inst: inst}, nil
}

func (r *readlineReader) Readline() (string, error) {
	line, err := r.inst.Readline()
	if err == readline.ErrInterrupt {
		return "", ErrInterrupt
	}
	return line, err
}

func (r *readlineReader) Close() error {
	return r.inst.Close()
}
