package main

import (
	"fmt"
	"strings"

	"github.com/alecthomas/kong"
)

// ExampleFlag is a flag type that prints usage examples for the
// current command and exits. It uses the BeforeReset hook to fire
// before required-field validation, so commands like
// "bpfman link attach xdp --example" work without providing mandatory
// operands such as <program-id> or <iface>.
type ExampleFlag bool

// BeforeReset prints the example text for the current command path
// and exits with status 0.
func (ExampleFlag) BeforeReset(ctx *kong.Context, app *kong.Kong) error {
	// Build the command path from traced commands, ignoring
	// arguments and positionals that may not be populated yet.
	var parts []string
	for _, trace := range ctx.Path {
		if trace.Command != nil {
			parts = append(parts, trace.Command.Name)
		}
	}
	path := strings.Join(parts, " ")

	text, ok := commandExamples[path]
	if !ok {
		return fmt.Errorf("no examples available for %q", path)
	}

	fmt.Fprint(app.Stdout, text)
	app.Exit(0)
	return nil
}

// commandExamples maps command paths to their example text. Each
// snippet is a complete, copy-pasteable shell workflow.
var commandExamples = map[string]string{
	"program load image": `BPFMAN_PROG_ID=$(bpfman program load image \
  quay.io/bpfman-bytecode/xdp_pass:latest \
  --programs xdp:pass \
  -o 'jsonpath={.programs[0].record.program_id}')
`,

	"program load file": `BPFMAN_PROG_ID=$(bpfman program load file \
  ./program.o \
  --programs tracepoint:my_prog \
  -o 'jsonpath={.programs[0].record.program_id}')
`,

	"link attach xdp": `BPFMAN_PROG_ID=$(bpfman program load image \
  quay.io/bpfman-bytecode/xdp_pass:latest \
  --programs xdp:pass \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach xdp "$BPFMAN_PROG_ID" lo --priority 50
`,

	"link attach tc": `BPFMAN_PROG_ID=$(bpfman program load image \
  quay.io/bpfman-bytecode/go-tc-counter:latest \
  --programs tc:stats \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach tc "$BPFMAN_PROG_ID" lo ingress --priority 50
`,

	"link attach tcx": `BPFMAN_PROG_ID=$(bpfman program load image \
  quay.io/bpfman-bytecode/go-tc-counter:latest \
  --programs tcx:stats \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach tcx "$BPFMAN_PROG_ID" lo ingress --priority 50
`,

	"link attach tracepoint": `BPFMAN_PROG_ID=$(bpfman program load image \
  quay.io/bpfman-bytecode/go-tracepoint-counter:latest \
  --programs tracepoint:tracepoint_kill_recorder \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach tracepoint "$BPFMAN_PROG_ID" syscalls/sys_enter_kill
`,

	"link attach kprobe": `BPFMAN_PROG_ID=$(bpfman program load image \
  quay.io/bpfman-bytecode/go-kprobe-counter:latest \
  --programs kprobe:kprobe_counter \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach kprobe "$BPFMAN_PROG_ID" try_to_wake_up
`,

	"link attach uprobe": `BPFMAN_PROG_ID=$(bpfman program load image \
  quay.io/bpfman-bytecode/go-uprobe-counter:latest \
  --programs uprobe:uprobe_counter \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach uprobe "$BPFMAN_PROG_ID" /lib64/libc.so.6 --fn-name malloc
`,

	"link attach fentry": `BPFMAN_PROG_ID=$(bpfman program load file \
  ./fentry.o \
  --programs fentry:test_fentry:do_unlinkat \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach fentry "$BPFMAN_PROG_ID"
`,

	"link attach fexit": `BPFMAN_PROG_ID=$(bpfman program load file \
  ./fexit.o \
  --programs fexit:test_fexit:do_unlinkat \
  -o 'jsonpath={.programs[0].record.program_id}')

bpfman link attach fexit "$BPFMAN_PROG_ID"
`,
}
