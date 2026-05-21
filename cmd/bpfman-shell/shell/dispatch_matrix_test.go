package shell

import (
	"errors"
	"fmt"
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
	// expectExecBind is true when an unknown head at this
	// site reaches the ExecBind pipeline at runtime; false
	// when it reaches ExecCommand. The negative test
	// asserts the head shows up on the right pipeline so
	// preflight-passes-runtime-still-dispatches is held
	// together.
	expectExecBind bool
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
			name:           "bind",
			wrap:           func(h string) string { return "let p <- " + h + "\n" },
			expectExecBind: true,
		},
		{
			name:           "command",
			wrap:           func(h string) string { return h + "\n" },
			expectExecBind: false,
		},
		{
			name:           "defer",
			wrap:           func(h string) string { return "defer " + h + "\nprint \"main\"\n" },
			expectExecBind: true,
		},
		{
			name: "producer",
			wrap: func(h string) string {
				return "let xs <- foreach n in [1] { " + h + " }\n"
			},
			expectExecBind: true,
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
// def, no near-miss), passes preflight AND reaches the
// external dispatch path at runtime. Preflight-only checking
// would miss the case where the head still falls through to
// the wrong runtime pipeline.
func TestDispatch_Matrix_UnknownNameAllSitesPassPreflight(t *testing.T) {
	t.Parallel()

	for _, site := range dispatchSites() {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			head := "totally_unknown_command"
			src := site.wrap(head)
			// Preflight must not trip the conditional hint --
			// the name is not in source anywhere.
			issues := checkSource(t, src)
			for _, i := range issues {
				if strings.Contains(i.Msg, "conditional") {
					t.Fatalf("site=%s: unknown name must not trip the conditional hint (got %q)",
						site.name, i.Msg)
				}
			}
			// Runtime: the unknown head must reach the
			// external dispatch pipeline for the site's
			// shape. Bind-style sites land on ExecBind;
			// command-style sites land on ExecCommand.
			// site.expectExecBind says which pipeline the
			// head should appear on.
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
			if site.expectExecBind {
				found := false
				for _, c := range r.calls {
					if len(c.args) > 0 {
						if w, ok := c.args[0].(WordArg); ok && w.Text == head {
							found = true
							break
						}
					}
				}
				assert.True(t, found, "site=%s: unknown head %q must reach ExecBind", site.name, head)
			} else {
				assert.Contains(t, commandCalls, head,
					"site=%s: unknown head must reach ExecCommand", site.name)
			}
		})
	}
}

// Outcome x bind-form matrix for the eventually bind path.
// Captures the class of bug the recent external probe found
// inside evalEventuallyBind: the guard form was ignored on
// failure, the failure envelope's Code was 0 instead of 1.
// Each cell pins one combination of (eventually outcome) x
// (bind target shape). A regression in any of {let, guard,
// let-tuple} x {success, failure} fails a single cell rather
// than waiting for an external probe to reproduce.
//
// The matrix is separate from the dispatch matrix above
// because the structural question is different: the dispatch
// matrix tests "does the right runtime pipeline see this
// head", this matrix tests "does evalEventuallyBind handle
// every bind-target shape correctly across outcomes".
func TestEventuallyBind_Matrix_BindForm_x_Outcome(t *testing.T) {
	t.Parallel()

	// body produces an eventually outcome. "ok" body
	// succeeds first attempt; "fail" body times out.
	type outcomeCase struct {
		name string
		body string
	}
	outcomes := []outcomeCase{
		{name: "success", body: "  print \"ok\"\n"},
		{name: "failure", body: "  assert 1 == 2\n"},
	}
	// formCase carries the bind keyword, the bind-target
	// spelling, and per-outcome assertion functions. The
	// success / failure checks are split because the three
	// bind shapes publish different things on failure:
	//
	//   - let_single binds the eventually result value (a
	//     structured map with ok / timed_out / attempts /
	//     elapsed_ms / error / last_command). The result has
	//     NO top-level .code field, so failure asserts on
	//     $r.ok and $r.timed_out, not on a synthesised
	//     rc.code.
	//   - guard publishes nothing on failure -- it halts via
	//     GuardFailure whose Envelope carries OK=false and
	//     Code=1 (the synthesised rc).
	//   - let_tuple publishes both the rc envelope mirror
	//     (which DOES have .ok and .code) and the result
	//     value as separate slots.
	//
	// Earlier shape ran every form through a single rc.code
	// check; the single-name forms passed vacuously because
	// fmt.Sprint(nil) != "0" was always true. Per-form
	// assertions remove the vacuity.
	type formCase struct {
		name         string
		keyword      string
		target       string
		checkSuccess func(t *testing.T, s *Session)
		checkFailure func(t *testing.T, s *Session, err error)
	}
	forms := []formCase{
		{
			name:    "let_single",
			keyword: "let",
			target:  "r",
			checkSuccess: func(t *testing.T, s *Session) {
				v, ok := s.Get("r")
				require.True(t, ok, "let single: $r must be bound on success")
				raw, _ := v.Raw().(map[string]any)
				assert.Equal(t, true, raw["ok"], "let single: $r.ok must be true on success")
			},
			checkFailure: func(t *testing.T, s *Session, err error) {
				require.NoError(t, err, "let single: must not halt on failure")
				v, ok := s.Get("r")
				require.True(t, ok, "let single: $r must be bound on failure")
				raw, _ := v.Raw().(map[string]any)
				assert.Equal(t, false, raw["ok"], "let single: $r.ok must be false on failure")
				assert.Equal(t, true, raw["timed_out"], "let single: $r.timed_out must be true on failure (the result has no top-level .code; the timeout flag is the failure signal)")
			},
		},
		{
			name:    "guard",
			keyword: "guard",
			target:  "r",
			checkSuccess: func(t *testing.T, s *Session) {
				v, ok := s.Get("r")
				require.True(t, ok, "guard: $r must be bound on success")
				raw, _ := v.Raw().(map[string]any)
				assert.Equal(t, true, raw["ok"], "guard: $r.ok must be true on success")
			},
			checkFailure: func(t *testing.T, s *Session, err error) {
				require.Error(t, err, "guard: must halt on failure")
				var gf *GuardFailure
				require.True(t, errors.As(err, &gf), "guard: expected GuardFailure, got %T", err)
				assert.False(t, gf.Envelope.OK, "guard: envelope OK=false on failure")
				assert.NotZero(t, gf.Envelope.Code, "guard: envelope Code must be non-zero on failure")
				_, bound := s.Get("r")
				assert.False(t, bound, "guard: $r must NOT be bound on failure (the halt fires before assignment)")
			},
		},
		{
			name:    "let_tuple",
			keyword: "let",
			target:  "(rc p)",
			checkSuccess: func(t *testing.T, s *Session) {
				rcVal, ok := s.Get("rc")
				require.True(t, ok, "let tuple: $rc must be bound on success")
				raw, _ := rcVal.Raw().(map[string]any)
				assert.Equal(t, true, raw["ok"], "let tuple: $rc.ok must be true on success")
				_, primary := s.Get("p")
				assert.True(t, primary, "let tuple: $p must be bound on success")
			},
			checkFailure: func(t *testing.T, s *Session, err error) {
				require.NoError(t, err, "let tuple: must not halt on failure")
				rcVal, ok := s.Get("rc")
				require.True(t, ok, "let tuple: $rc must be bound on failure")
				raw, _ := rcVal.Raw().(map[string]any)
				assert.Equal(t, false, raw["ok"], "let tuple: $rc.ok must be false on failure")
				assert.Equal(t, "1", fmt.Sprint(raw["code"]), "let tuple: $rc.code must be 1 on failure")
				pVal, primary := s.Get("p")
				require.True(t, primary, "let tuple: $p must be bound on failure")
				pRaw, _ := pVal.Raw().(map[string]any)
				assert.Equal(t, false, pRaw["ok"], "let tuple: $p.ok (the result value) must be false on failure")
			},
		},
	}
	for _, outcome := range outcomes {
		for _, form := range forms {
			label := form.name + "_" + outcome.name
			t.Run(label, func(t *testing.T) {
				t.Parallel()
				src := fmt.Sprintf(
					"%s %s <- eventually timeout 30ms interval 5ms {\n%s}\n",
					form.keyword, form.target, outcome.body,
				)
				s := NewSession()
				env := eventuallyEnv(s,
					func([]Arg, Span) (Value, error) { return Value{}, nil },
					func([]Arg, Span) (BindResult, error) { return BindResult{Rc: OkEnvelope()}, nil },
				)
				err := runEventuallySrc(t, src, env)
				if outcome.name == "success" {
					require.NoError(t, err, "%s: success outcome must not error", label)
					form.checkSuccess(t, s)
				} else {
					form.checkFailure(t, s, err)
				}
			})
		}
	}
}
