package shell

// Test-only registration of bind shapes for builtins whose runtime
// registration lives in cmd/bpfman-shell. The shell package's static
// checker tests (check_test.go) assert on the inferred Shape of
// expressions like `let p <- start sleep 60` and `let r <- which
// bpftool`, which need the registry populated before the checker
// runs. cmd/bpfman-shell's init does this in production builds; for
// the shell-package test binary we mirror the same registrations
// here so the tests stay package-local and do not require linking
// the cmd-side init.
//
// Keep this file in lock-step with cmd/bpfman-shell/builtins.go's
// BindShape entries. The registrations themselves are idempotent so
// duplicating them in both places is safe; the test list is just
// the subset the shell-package tests actually exercise.
func init() {
	RegisterBindShape("start", StaticBindShape(KindShape(OriginJob)))
	RegisterBindShape("fire", StaticBindShape(KindShape(OriginJob)))
	RegisterBindShape("kill", StaticBindShape(KindShape(OriginEnvelope)))
	RegisterBindShape("wait", StaticBindShape(KindShape(OriginEnvelope)))
	RegisterBindShape("exec", StaticBindShape(KindShape(OriginEnvelope)))
	RegisterBindShape("file", StaticBindShape(Shape{Sealed: false, Kind: OriginUnknown}))
	RegisterBindShape("bpfman", stubBpfmanBindShape)
}

// stubBpfmanBindShape mirrors the production registration in
// cmd/bpfman-shell/bindshapes.go closely enough to keep shell-
// package tests honest. The kind-aware specialisation for
// `bpfman link attach <kind>` is deliberately omitted: shell-
// package tests do not probe record.details, and the cmd-side
// test binary covers that path with the real reflection-derived
// shapes.
func stubBpfmanBindShape(args []Expr) Shape {
	if len(args) < 2 {
		return Shape{Sealed: false, Kind: OriginUnknown}
	}
	noun, ok := args[0].(*LiteralExpr)
	if !ok || noun.Quoted {
		return Shape{Sealed: false, Kind: OriginUnknown}
	}
	verb, ok := args[1].(*LiteralExpr)
	if !ok || verb.Quoted {
		return Shape{Sealed: false, Kind: OriginUnknown}
	}
	switch noun.Text {
	case "program":
		switch verb.Text {
		case "load":
			elem := KindShape(OriginProgram)
			return Shape{
				Sealed: true,
				Kind:   OriginUnknown,
				Fields: map[string]Shape{
					"programs": {Sealed: false, Kind: OriginUnknown, Elem: &elem},
				},
			}
		case "get":
			return KindShape(OriginProgram)
		case "list":
			elem := KindShape(OriginProgram)
			return Shape{Sealed: false, Kind: OriginUnknown, Elem: &elem}
		}
	case "link":
		switch verb.Text {
		case "attach", "get":
			return KindShape(OriginLink)
		case "list":
			elem := KindShape(OriginLink)
			return Shape{Sealed: false, Kind: OriginUnknown, Elem: &elem}
		}
	}
	return Shape{Sealed: false, Kind: OriginUnknown}
}
