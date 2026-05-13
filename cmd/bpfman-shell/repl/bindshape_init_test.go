package repl

import "github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"

// Test-only bpfman bind-shape registration. The repl-package test
// binary exercises CheckInput end-to-end on script source that
// references `bpfman program list / load`, `bpfman link attach`,
// etc. The cmd/bpfman-shell init that registers the real
// bind-shape policy is not compiled into the repl test binary,
// so without this stub the static checker would treat every
// bpfman bind as the default external-result envelope and report
// false-positive field errors on every $prog.record.X path.
//
// The stub mirrors the production noun / verb routing in
// cmd/bpfman-shell/bindshapes.go but skips the kind-aware
// specialisation for `bpfman link attach <kind>` because the
// repl tests do not assert against record.details fields. The
// cmd-side test binary covers that path with the real
// reflection-derived shapes.
func init() {
	shell.RegisterBindShape("bpfman", stubBpfmanBindShape)
}

func stubBpfmanBindShape(args []shell.Expr) shell.Shape {
	if len(args) < 2 {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	noun, ok := args[0].(*shell.LiteralExpr)
	if !ok || noun.Quoted {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	verb, ok := args[1].(*shell.LiteralExpr)
	if !ok || verb.Quoted {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	switch noun.Text {
	case "program":
		switch verb.Text {
		case "load":
			elem := shell.KindShape(shell.OriginProgram)
			return shell.Shape{
				Sealed: true,
				Kind:   shell.OriginUnknown,
				Fields: map[string]shell.Shape{
					"programs": {Sealed: false, Kind: shell.OriginUnknown, Elem: &elem},
				},
			}
		case "get":
			return shell.KindShape(shell.OriginProgram)
		case "list":
			elem := shell.KindShape(shell.OriginProgram)
			return shell.Shape{Sealed: false, Kind: shell.OriginUnknown, Elem: &elem}
		}
	case "link":
		switch verb.Text {
		case "attach", "get":
			return shell.KindShape(shell.OriginLink)
		case "list":
			elem := shell.KindShape(shell.OriginLink)
			return shell.Shape{Sealed: false, Kind: shell.OriginUnknown, Elem: &elem}
		}
	}
	return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
}
