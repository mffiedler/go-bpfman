package shell

import (
	"fmt"
	"sort"
)

// Session holds variable bindings and aliases for the REPL. It is
// the runtime state that persists across commands within a session.
type Session struct {
	vars           map[string]Value
	aliases        map[string]string
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
	return &Session{
		vars:    make(map[string]Value),
		aliases: make(map[string]string),
	}
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

// SetAlias binds a first-token alias. The caller is responsible for
// validating that name does not collide with shell commands.
func (s *Session) SetAlias(name, expansion string) {
	s.aliases[name] = expansion
}

// GetAlias retrieves an alias expansion. The second return value
// indicates whether the alias exists.
func (s *Session) GetAlias(name string) (string, bool) {
	v, ok := s.aliases[name]
	return v, ok
}

// DeleteAlias removes an alias binding.
func (s *Session) DeleteAlias(name string) {
	delete(s.aliases, name)
}

// AliasNames returns the sorted list of defined alias names.
func (s *Session) AliasNames() []string {
	names := make([]string, 0, len(s.aliases))
	for k := range s.aliases {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Expand resolves all variable references in the token slice against
// this session's bindings and returns typed arguments. Scalar
// references are resolved eagerly to ScalarValueArg. Bare structured
// references become StructuredValueArg, preserving the Value for
// typed consumption by command parsers. Non-variable tokens become
// WordArg or QuotedArg.
func (s *Session) Expand(tokens []Token) ([]Arg, error) {
	result := make([]Arg, 0, len(tokens))
	for _, tok := range tokens {
		if tok.Kind == TokenAdapterRef {
			v, ok := s.vars[tok.VarName]
			if !ok {
				return nil, fmt.Errorf("undefined variable: %s", tok.VarName)
			}
			resolved := v
			if tok.VarPath != "" {
				var err error
				resolved, err = v.LookupValue(tok.VarName, tok.VarPath)
				if err != nil {
					return nil, err
				}
			}
			if resolved.IsNil() {
				return nil, fmt.Errorf("adapter %s: variable %s is null", tok.Adapter, tok.VarName)
			}
			result = append(result, AdapterArg{
				Adapter: tok.Adapter,
				Name:    tok.VarName,
				Path:    tok.VarPath,
				Value:   resolved,
			})
			continue
		}

		if tok.Kind != TokenVarRef {
			switch tok.Kind {
			case TokenQuoted:
				result = append(result, QuotedArg{Text: tok.Text})
			default:
				result = append(result, WordArg{Text: tok.Text})
			}
			continue
		}

		v, ok := s.vars[tok.VarName]
		if !ok {
			return nil, fmt.Errorf("undefined variable: %s", tok.VarName)
		}

		if tok.VarPath == "" {
			// Bare reference to variable.
			if v.IsStructured() {
				result = append(result, StructuredValueArg{
					Name:  tok.VarName,
					Value: v,
				})
				continue
			}
			if v.IsNil() {
				return nil, fmt.Errorf("variable %s is null", tok.VarName)
			}
			str, err := v.Scalar()
			if err != nil {
				return nil, fmt.Errorf("variable %s: %w", tok.VarName, err)
			}
			result = append(result, ScalarValueArg{Text: str})
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
		result = append(result, ScalarValueArg{Text: str})
	}
	return result, nil
}
