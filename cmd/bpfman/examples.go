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
// flags such as --iface.
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
  --programs xdp:pass \
  --image-url quay.io/bpfman-bytecode/xdp_pass:latest \
  -o 'jsonpath={[0].record.program_id}')
`,

	"program load file": `BPFMAN_PROG_ID=$(bpfman program load file \
  --path ./program.o \
  --programs tracepoint:my_prog \
  -o 'jsonpath={[0].record.program_id}')
`,

	"link attach xdp": `BPFMAN_PROG_ID=$(bpfman program load image \
  --programs xdp:pass \
  --image-url quay.io/bpfman-bytecode/xdp_pass:latest \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach xdp --iface lo "$BPFMAN_PROG_ID"
`,

	"link attach tc": `BPFMAN_PROG_ID=$(bpfman program load image \
  --programs tc:stats \
  --image-url quay.io/bpfman-bytecode/go-tc-counter:latest \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach tc --iface lo --direction ingress "$BPFMAN_PROG_ID"
`,

	"link attach tcx": `BPFMAN_PROG_ID=$(bpfman program load image \
  --programs tcx:stats \
  --image-url quay.io/bpfman-bytecode/go-tc-counter:latest \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach tcx --iface lo --direction ingress "$BPFMAN_PROG_ID"
`,

	"link attach tracepoint": `BPFMAN_PROG_ID=$(bpfman program load image \
  --programs tracepoint:tracepoint_kill_recorder \
  --image-url quay.io/bpfman-bytecode/go-tracepoint-counter:latest \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach tracepoint "$BPFMAN_PROG_ID" syscalls/sys_enter_kill
`,

	"link attach kprobe": `BPFMAN_PROG_ID=$(bpfman program load image \
  --programs kprobe:kprobe_counter \
  --image-url quay.io/bpfman-bytecode/go-kprobe-counter:latest \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach kprobe --fn-name try_to_wake_up "$BPFMAN_PROG_ID"
`,

	"link attach uprobe": `BPFMAN_PROG_ID=$(bpfman program load image \
  --programs uprobe:uprobe_counter \
  --image-url quay.io/bpfman-bytecode/go-uprobe-counter:latest \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach uprobe --target /lib64/libc.so.6 --fn-name malloc "$BPFMAN_PROG_ID"
`,

	"link attach fentry": `BPFMAN_PROG_ID=$(bpfman program load file \
  --path ./fentry.o \
  --programs fentry:test_fentry:do_unlinkat \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach fentry "$BPFMAN_PROG_ID"
`,

	"link attach fexit": `BPFMAN_PROG_ID=$(bpfman program load file \
  --path ./fexit.o \
  --programs fexit:test_fexit:do_unlinkat \
  -o 'jsonpath={[0].record.program_id}')

bpfman link attach fexit "$BPFMAN_PROG_ID"
`,
}
