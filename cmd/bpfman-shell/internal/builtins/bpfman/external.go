// External `bpfman` dispatch: invoke the bpfman binary as a
// subprocess, capture stdout (always JSON; the dispatcher
// appends -o json if the caller did not), and decode into the
// same typed Value shape the in-process library backend
// produces for the matching (noun, verb).
//
// The mode toggle is BPFMAN_DISPATCH=external (also accepts
// "exec" / "cli"). Default is library. The two backends share
// scripts byte-for-byte: a script that runs cleanly under
// library mode runs cleanly under external mode iff the JSON
// contract holds for every command it invokes. Tests that
// exercise both modes are the parity check.

package bpfmanbuiltin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	bpfman "github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
	"github.com/frobware/go-bpfman/platform"
)

// dispatchMode selects between in-process library calls and
// fork+exec of the bpfman binary.
type dispatchMode int

const (
	dispatchLibrary dispatchMode = iota
	dispatchExternal
)

// bpfmanDispatchMode is set once at process start from the
// BPFMAN_DISPATCH environment variable. "external" / "exec" /
// "cli" enable the external backend; anything else (including
// unset) keeps the in-process library backend.
var bpfmanDispatchMode = func() dispatchMode {
	switch strings.ToLower(os.Getenv("BPFMAN_DISPATCH")) {
	case "external", "exec", "cli":
		return dispatchExternal
	default:
		return dispatchLibrary
	}
}()

// dispatchCommandExternal forks the bpfman binary with the
// command's textual args, appends -o json if absent, captures
// stdout, and decodes via decodeBpfmanResult.
func dispatchCommandExternal(ctx context.Context, args []runtime.Arg) (runtime.Value, error) {
	if len(args) == 0 {
		return runtime.Value{}, fmt.Errorf("missing command after \"bpfman\"; try \"bpfman program list\"")
	}

	argv := make([]string, 0, len(args))
	for i, a := range args {
		text, err := argToCLIText(a)
		if err != nil {
			return runtime.Value{}, fmt.Errorf("bpfman arg %d: %w", i+1, err)
		}
		argv = append(argv, text)
	}
	if !hasOutputFlag(argv) {
		argv = append(argv, "-o", "json")
	}

	bin := os.Getenv("BPFMAN_BIN")
	if bin == "" {
		bin = "bpfman"
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return runtime.Value{}, errors.New(msg)
	}
	return decodeBpfmanResult(args, stdout.Bytes())
}

// argToCLIText renders a single runtime.Arg into the textual form
// the bpfman CLI expects on argv. WordArg / QuotedArg /
// ScalarValueArg pass straight through; StructuredValueArg
// dispatches on the value's origin capability:
//
//   - HasKernelLinkID  -> the link ID as decimal text
//   - HasKernelProgramID -> the program ID as decimal text
//
// This mirrors the library backend's parseLinkIDArg /
// parseProgramIDArg, which use the same capability interfaces to
// preserve the `$link` / `$prog` ergonomic that scripts already
// depend on. A StructuredValueArg whose origin satisfies neither
// capability errors out: the CLI takes only IDs (and other
// textual flags) on argv, so other structured kinds (Job, Envelope,
// NetPair) cannot legitimately appear.
func argToCLIText(a runtime.Arg) (string, error) {
	switch v := a.(type) {
	case runtime.WordArg:
		return v.Text, nil
	case runtime.QuotedArg:
		return v.Text, nil
	case runtime.ScalarValueArg:
		return v.Text, nil
	case runtime.AdapterArg:
		s, err := v.Value.Scalar()
		if err != nil {
			return "", fmt.Errorf("adapter %s: %w", v.Adapter, err)
		}
		return s, nil
	case runtime.StructuredValueArg:
		origin := v.Value.Origin()
		if origin != nil {
			if x, ok := origin.(bpfman.HasKernelLinkID); ok {
				return strconv.FormatUint(uint64(x.KernelLinkID()), 10), nil
			}
			if x, ok := origin.(bpfman.HasKernelProgramID); ok {
				return strconv.FormatUint(uint64(x.KernelProgramID()), 10), nil
			}
		}
		display := displayName(v.Name)
		return "", fmt.Errorf("%s is structured but carries no kernel ID capability", display)
	}
	return "", fmt.Errorf("unexpected argument type %T", a)
}

// hasOutputFlag reports whether argv already specifies an output
// format via -o or --output. Used to avoid clobbering an
// explicit user choice when the script invokes a CLI form that
// already passes -o json (or jsonpath, etc.).
func hasOutputFlag(argv []string) bool {
	for _, a := range argv {
		switch {
		case a == "-o" || a == "--output":
			return true
		case strings.HasPrefix(a, "-o="):
			return true
		case strings.HasPrefix(a, "--output="):
			return true
		}
	}
	return false
}

// decodeBpfmanResult unmarshals stdout into the Go domain type
// that matches (noun, verb) and tags the resulting Value with
// the same OriginKind the library backend uses. Commands whose
// primary slot is void (unload, detach, delete) return an empty
// Value; the caller's rc slot still carries the envelope so
// guard / assert observe the right outcome.
//
// The dispatch table mirrors shell/semantics' bpfman bind-shape
// logic: any (noun, verb) that yields a typed Shape there must
// yield a Value with that OriginKind here. Tests that run the
// same script under both modes lock the two paths together.
func decodeBpfmanResult(args []runtime.Arg, stdout []byte) (runtime.Value, error) {
	if len(args) < 2 {
		return runtime.Value{}, nil
	}
	noun := driver.ArgText(args[0])
	verb := driver.ArgText(args[1])
	switch noun {
	case "program":
		switch verb {
		case "get":
			var p bpfman.Program
			if err := json.Unmarshal(stdout, &p); err != nil {
				return runtime.Value{}, fmt.Errorf("decode Program: %w", err)
			}
			v, err := runtime.ValueFromStruct(p)
			if err != nil {
				return runtime.Value{}, err
			}
			return v.WithKind(semantics.OriginProgram), nil
		case "list":
			var result bpfman.ProgramListResult
			if err := json.Unmarshal(stdout, &result); err != nil {
				return runtime.Value{}, fmt.Errorf("decode ProgramListResult: %w", err)
			}
			return runtime.ValueFromStruct(result)
		case "load":
			var result bpfman.LoadResult
			if err := json.Unmarshal(stdout, &result); err != nil {
				return runtime.Value{}, fmt.Errorf("decode LoadResult: %w", err)
			}
			return runtime.ValueFromStruct(result)
		case "unload", "delete":
			return runtime.Value{}, nil
		}
	case "link":
		switch verb {
		case "attach", "get":
			var l bpfman.Link
			if err := json.Unmarshal(stdout, &l); err != nil {
				return runtime.Value{}, fmt.Errorf("decode Link: %w", err)
			}
			v, err := runtime.ValueFromStruct(l)
			if err != nil {
				return runtime.Value{}, err
			}
			return v.WithKind(semantics.OriginLink), nil
		case "list":
			var result bpfman.LinkListResult
			if err := json.Unmarshal(stdout, &result); err != nil {
				return runtime.Value{}, fmt.Errorf("decode LinkListResult: %w", err)
			}
			return runtime.ValueFromStruct(result)
		case "detach", "delete":
			return runtime.Value{}, nil
		}
	case "dispatcher":
		switch verb {
		case "get":
			var snap platform.DispatcherSnapshot
			if err := json.Unmarshal(stdout, &snap); err != nil {
				return runtime.Value{}, fmt.Errorf("decode DispatcherSnapshot: %w", err)
			}
			return runtime.ValueFromStruct(snap)
		case "list":
			var summaries []platform.DispatcherSummary
			if err := json.Unmarshal(stdout, &summaries); err != nil {
				return runtime.Value{}, fmt.Errorf("decode DispatcherSummary list: %w", err)
			}
			return runtime.ValueFromStruct(summaries)
		case "delete":
			return runtime.Value{}, nil
		}
	}
	// Anything else (audit, show, ...) has no typed primary slot
	// today. The envelope from the caller's bind already carries
	// stdout/stderr/code.
	return runtime.Value{}, nil
}
