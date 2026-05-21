package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Dispatch sites x head kinds, exhaustively. Each cell pins
// what the dispatch rule for that combination is supposed to
// do. The matrix exists because the recent bug class has been
// "rule applied at site A but not B" -- the alias-to-def
// resolution, the conditional-def hint, the def-lookup
// precedence each leaked across one site but not another.
// Enumerating the cross product makes a future asymmetry fail
// a test at PR time instead of waiting for an external probe.
//
// Rows are dispatch sites:
//
//   - bind: `let p <- HEAD`
//   - command: `HEAD` at statement position
//   - defer: `defer HEAD`
//   - producer: `let xs <- foreach n in [1] { HEAD }` (bind-
//     collect producer)
//
// Columns are head kinds:
//
//   - topLevelDef: `def HEAD() { ... }` at script top level
//   - conditionalDef: `def HEAD() { ... }` declared inside a
//     never-taken if-branch (the runtime never registers it
//     but the body parses cleanly)
//
// Adding a third name-kind (alias, unknown, etc.) extends the
// matrix in one place rather than per-site. Each cell asserts
// one of two outcomes: the def's body runs (the sentinel
// command surfaces through ExecCommand) OR the checker
// rejects with the conditional-branch diagnostic.

type dispatchSite struct {
	name string
	// script wraps the head invocation in the dispatch
	// shape the site uses. The %q slot receives the head
	// name verbatim. The "sentinel" command at the body
	// fires through ExecCommand when the def actually
	// executes.
	wrap func(headName string) string
}

type headKind struct {
	name string
	// decl is the script-prefix that brings the head into
	// (or NOT into) scope. The head name itself is fixed
	// because the wrap functions interpolate the same name.
	decl string
	// wantBodyRuns is true if the cell should reach the
	// def's body at runtime (top-level decl); false if the
	// checker should reject before runtime (conditional
	// decl).
	wantBodyRuns bool
}

func dispatchSites() []dispatchSite {
	return []dispatchSite{
		{
			name: "bind",
			wrap: func(h string) string { return "let p <- " + h + "\n" },
		},
		{
			name: "command",
			wrap: func(h string) string { return h + "\n" },
		},
		{
			name: "defer",
			wrap: func(h string) string { return "defer " + h + "\nprint \"main\"\n" },
		},
		{
			name: "producer",
			wrap: func(h string) string {
				return "let xs <- foreach n in [1] { " + h + " }\n"
			},
		},
	}
}

func headKinds() []headKind {
	return []headKind{
		{
			name:         "topLevelDef",
			decl:         "def headfn() {\n  sentinel\n  return \"ok\"\n}\n",
			wantBodyRuns: true,
		},
		{
			name:         "conditionalDef",
			decl:         "if false {\n  def headfn() {\n    sentinel\n    return \"ok\"\n  }\n}\n",
			wantBodyRuns: false,
		},
	}
}

func TestDispatch_Matrix_Sites_x_HeadKinds(t *testing.T) {
	t.Parallel()

	for _, site := range dispatchSites() {
		for _, head := range headKinds() {
			t.Run(site.name+"_"+head.name, func(t *testing.T) {
				t.Parallel()

				src := head.decl + site.wrap("headfn")

				if !head.wantBodyRuns {
					// Conditional-def cells: preflight
					// must reject with a diagnostic that
					// names the conditional-branch
					// status and the head name. Every
					// dispatch site sees the same hint
					// because all four route through
					// the consolidated dispatch helpers.
					issues := checkSource(t, src)
					require.NotEmpty(t, issues, "site=%s head=%s: preflight must reject", site.name, head.name)
					combined := issues[0].Msg
					for _, i := range issues[1:] {
						combined += "\n" + i.Msg
					}
					assert.Contains(t, combined, "headfn", "diagnostic must name the head")
					assert.Contains(t, combined, "conditional", "diagnostic must name the conditional-branch status")
					return
				}

				// Top-level-def cells: the def's body
				// runs at runtime. The sentinel command
				// fires through ExecCommand; record and
				// assert it shows up.
				r := &recorder{}
				var commandCalls []string
				env := &Env{
					Session:  NewSession(),
					ExecBind: r.execBind,
					ExecCommand: func(args []Arg, _ Span) (Value, error) {
						if len(args) > 0 {
							if w, ok := args[0].(WordArg); ok {
								commandCalls = append(commandCalls, w.Text)
							}
						}
						return Value{}, nil
					},
					RenderDeferFailure: func(Pos, []Arg, Envelope) {},
				}
				require.NoError(t, runProgramWithEnv(t, src, env))
				assert.Contains(t, commandCalls, "sentinel",
					"site=%s head=%s: def body must run; recorded=%v",
					site.name, head.name, commandCalls)
				// The head name must not have reached
				// ExecBind / ExecCommand as a top-level
				// dispatch -- if it did, the def-lookup
				// precedence drifted at this site.
				for _, c := range commandCalls {
					if c == "headfn" {
						t.Fatalf("site=%s head=%s: head name %q reached ExecCommand as a top-level dispatch", site.name, head.name, c)
					}
				}
				for _, c := range r.calls {
					if len(c.args) == 0 {
						continue
					}
					if w, ok := c.args[0].(WordArg); ok && w.Text == "headfn" {
						t.Fatalf("site=%s head=%s: head name %q reached ExecBind as a top-level dispatch", site.name, head.name, w.Text)
					}
				}
			})
		}
	}
}

// A focused negative test: each dispatch site, when given a
// head name that does NOT exist anywhere in the program (no
// def, no alias, no near-miss), passes preflight and reaches
// the external dispatch path. Confirms the helpers do not
// over-reject; only the conditional-branch case trips
// preflight, not every unknown name.
func TestDispatch_Matrix_UnknownNameAllSitesPassPreflight(t *testing.T) {
	t.Parallel()

	for _, site := range dispatchSites() {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			src := site.wrap("totally_unknown_command")
			issues := checkSource(t, src)
			for _, i := range issues {
				if strings.Contains(i.Msg, "conditional") {
					t.Fatalf("site=%s: unknown name must not trip the conditional hint (got %q)",
						site.name, i.Msg)
				}
			}
		})
	}
}
