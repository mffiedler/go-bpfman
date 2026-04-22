package shell

import "sort"

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
