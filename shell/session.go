package shell

import "sort"

// Session holds variable bindings, aliases, and user-defined commands
// (defs) for the REPL. It is the runtime state that persists across
// commands within a session.
type Session struct {
	vars           map[string]Value
	aliases        map[string]string
	defs           map[string]*DefValue
	assertFailures int
}

// DefValue is a user-defined command registered via the `def NAME(P1,
// P2, ...) { BODY }` form. It holds the parameter list, the parsed
// body, and the source location of the declaration for diagnostics.
type DefValue struct {
	Name   string
	Params []string
	Body   []Stmt
	Loc    Loc
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
		defs:    make(map[string]*DefValue),
	}
}

// SetDef registers (or replaces) a user-defined command. The caller
// is responsible for validating the name and parameter list.
func (s *Session) SetDef(d *DefValue) {
	s.defs[d.Name] = d
}

// GetDef retrieves a user-defined command. The second return value
// indicates whether a def with that name exists.
func (s *Session) GetDef(name string) (*DefValue, bool) {
	d, ok := s.defs[name]
	return d, ok
}

// DeleteDef removes a user-defined command. Returns true if the def
// existed.
func (s *Session) DeleteDef(name string) bool {
	if _, ok := s.defs[name]; !ok {
		return false
	}
	delete(s.defs, name)
	return true
}

// DefNames returns the sorted list of registered def names.
func (s *Session) DefNames() []string {
	names := make([]string, 0, len(s.defs))
	for k := range s.defs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
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
