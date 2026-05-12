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
}
