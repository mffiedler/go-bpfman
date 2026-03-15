package replang

import (
	"fmt"
	"sort"
)

// Session holds variable bindings for the REPL. It is the runtime
// state that persists across commands within a session.
type Session struct {
	vars           map[string]Value
	assertFailures int
}

// RecordAssertFailure increments the assertion failure counter.
func (s *Session) RecordAssertFailure() {
	s.assertFailures++
}

// AssertFailures returns the number of recorded assertion failures.
func (s *Session) AssertFailures() int {
	return s.assertFailures
}

// NewSession returns an empty session.
func NewSession() *Session {
	return &Session{vars: make(map[string]Value)}
}

// Set binds a value to a variable name, replacing any existing binding.
func (s *Session) Set(name string, v Value) {
	s.vars[name] = v
}

// Get retrieves a variable's value. The second return value indicates
// whether the variable exists.
func (s *Session) Get(name string) (Value, bool) {
	v, ok := s.vars[name]
	return v, ok
}

// Delete removes a variable binding.
func (s *Session) Delete(name string) {
	delete(s.vars, name)
}

// Names returns the sorted list of bound variable names.
func (s *Session) Names() []string {
	names := make([]string, 0, len(s.vars))
	for k := range s.vars {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Expand resolves all VarRef tokens in the slice against this
// session's variable bindings. Each VarRef is replaced with a
// TokenWord containing the scalar string value. Non-VarRef tokens
// pass through unchanged.
func (s *Session) Expand(tokens []Token) ([]Token, error) {
	result := make([]Token, 0, len(tokens))
	for _, tok := range tokens {
		if tok.Kind != TokenVarRef {
			result = append(result, tok)
			continue
		}

		v, ok := s.vars[tok.VarName]
		if !ok {
			return nil, fmt.Errorf("undefined variable: %s", tok.VarName)
		}

		if tok.VarPath == "" {
			// Bare reference to variable.
			if v.IsStructured() {
				// Pass through the original token text
				// so per-command handlers can resolve it
				// (e.g. auto-extract .record.program_id).
				result = append(result, Token{Kind: TokenWord, Text: tok.Text})
				continue
			}
			if v.IsNil() {
				return nil, fmt.Errorf("variable %s is null", tok.VarName)
			}
			str, err := v.Scalar()
			if err != nil {
				return nil, fmt.Errorf("variable %s: %w", tok.VarName, err)
			}
			result = append(result, Token{Kind: TokenWord, Text: str})
			continue
		}

		resolved, err := v.Lookup(tok.VarName, tok.VarPath)
		if err != nil {
			return nil, err
		}
		str, err := resolved.Scalar()
		if err != nil {
			return nil, fmt.Errorf("variable %s.%s: %w", tok.VarName, tok.VarPath, err)
		}
		result = append(result, Token{Kind: TokenWord, Text: str})
	}
	return result, nil
}
