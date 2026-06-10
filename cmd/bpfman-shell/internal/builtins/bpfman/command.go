package bpfmanbuiltin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/image/oci"
)

// Command is the sealed interface for typed command nodes produced by
// parsing expanded shell arguments. Each concrete variant carries
// fully resolved, validated fields ready for execution.
type Command interface {
	isCommand()
}

// parseCommand routes expanded domain command arguments to the
// appropriate per-command parser, returning a typed Command node. The
// routing logic matches command and subcommand keywords, then
// delegates the remaining arguments to the specific parser. Returns
// nil with no error when args is empty.
func parseCommand(args []runtime.Arg) (Command, error) {
	if len(args) == 0 {
		return nil, nil
	}

	cmd := driver.ArgText(args[0])
	arg := func(n int) string {
		if n < len(args) {
			return driver.ArgText(args[n])
		}
		return ""
	}

	switch {
	// program commands
	case len(args) >= 2 && cmd == "program" && arg(1) == "list":
		return parseListPrograms(args[2:])
	case len(args) >= 3 && cmd == "program" && arg(1) == "load" && arg(2) == "file":
		return parseLoadFile(args[3:])
	case len(args) >= 3 && cmd == "program" && arg(1) == "load" && arg(2) == "image":
		return parseLoadImage(args[3:])
	case len(args) >= 2 && cmd == "program" && arg(1) == "get":
		return parseGetProgram(args[2:])
	case len(args) >= 2 && cmd == "program" && arg(1) == "unload":
		return parseUnloadProgram(args[2:])
	case len(args) >= 2 && cmd == "program" && arg(1) == "delete":
		return parseDeleteProgram(args[2:])
	case len(args) >= 3 && cmd == "show" && arg(1) == "program":
		return parseShowProgram(args[2:])
	case len(args) >= 2 && cmd == "image" && arg(1) == "inspect":
		return parseImageInspect(args[2:])
	case len(args) >= 2 && cmd == "image" && arg(1) == "build":
		return parseImageBuild(args[2:])

	// link commands
	case len(args) >= 2 && cmd == "link" && arg(1) == "attach":
		return parseLinkAttach(args[2:])
	case len(args) >= 2 && cmd == "link" && arg(1) == "detach":
		return parseLinkDetach(args[2:])
	case len(args) >= 2 && cmd == "link" && arg(1) == "get":
		return parseGetLink(args[2:])
	case len(args) >= 2 && cmd == "link" && arg(1) == "list":
		return parseListLinks(args[2:])
	case len(args) >= 2 && cmd == "link" && arg(1) == "delete":
		return parseDeleteLink(args[2:])

	// dispatcher commands
	case len(args) >= 2 && cmd == "dispatcher" && arg(1) == "list":
		return parseDispatcherList(args[2:])
	case len(args) >= 2 && cmd == "dispatcher" && arg(1) == "get":
		return parseDispatcherGet(args[2:])
	case len(args) >= 2 && cmd == "dispatcher" && arg(1) == "delete":
		return parseDispatcherDelete(args[2:])

	// known nouns reached without a usable subcommand: emit a
	// targeted message instead of the generic "unknown command".
	case cmd == "program" && arg(1) == "load":
		return nil, fmt.Errorf("program load: requires 'file' or 'image'")
	case cmd == "program":
		return nil, fmt.Errorf("program: subcommand required (list, load, get, unload, delete)")
	case cmd == "link":
		return nil, fmt.Errorf("link: subcommand required (attach, detach, list, get, delete)")
	case cmd == "dispatcher":
		return nil, fmt.Errorf("dispatcher: subcommand required (list, get, delete)")
	case cmd == "image":
		return nil, fmt.Errorf("image: subcommand required (build, inspect)")
	case cmd == "show":
		return nil, fmt.Errorf("show: subcommand required (program)")

	default:
		return nil, fmt.Errorf("unknown command %q. Try \"bpfman --help\" for available commands.", strings.Join(driver.ArgTexts(args), " "))
	}
}

// execCommand executes a typed Command node, returning an optional
// result value for variable binding. Commands that do not produce
// assignable results return an empty value.
func execCommand(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd Command) (runtime.Value, error) {
	switch c := cmd.(type) {
	case *ShowProgramCommand:
		return runtime.Value{}, execShowProgram(ctx, cli, mgr, c)
	case *LoadFileCommand:
		return execLoadFile(ctx, cli, mgr, c)
	case *LoadImageCommand:
		return execLoadImage(ctx, cli, mgr, c)
	case *GetProgramCommand:
		return execGetProgram(ctx, cli, mgr, c)
	case *GetLinkCommand:
		return execGetLink(ctx, cli, mgr, c)
	case *UnloadProgramCommand:
		return runtime.Value{}, execUnloadProgram(ctx, cli, mgr, c)
	case *DeleteProgramCommand:
		return runtime.Value{}, execDeleteProgram(ctx, cli, mgr, c)
	case *ListProgramsCommand:
		return execListPrograms(ctx, cli, mgr, c)
	case *LinkAttachCommand:
		return execLinkAttach(ctx, cli, mgr, c)
	case *LinkDetachCommand:
		return runtime.Value{}, execLinkDetach(ctx, cli, mgr, c)
	case *ListLinksCommand:
		return execListLinks(ctx, cli, mgr, c)
	case *DeleteLinkCommand:
		return runtime.Value{}, execDeleteLink(ctx, cli, mgr, c)
	case *DispatcherListCommand:
		return execDispatcherList(ctx, cli, mgr, c)
	case *DispatcherGetCommand:
		return execDispatcherGet(ctx, cli, mgr, c)
	case *DispatcherDeleteCommand:
		return runtime.Value{}, execDispatcherDelete(ctx, cli, mgr, c)
	case *ImageInspectCommand:
		return execImageInspect(ctx, cli, c)
	case *ImageBuildCommand:
		return runtime.Value{}, execImageBuild(ctx, cli, c)
	default:
		return runtime.Value{}, fmt.Errorf("unhandled command type %T", cmd)
	}
}

// ImageBuildCommand represents a parsed "image build" command.
type ImageBuildCommand struct {
	Args []string
}

func (*ImageBuildCommand) isCommand() {}

func parseImageBuild(args []runtime.Arg) (*ImageBuildCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("image build: requires an image reference")
	}
	argv := make([]string, 0, len(args))
	for i, arg := range args {
		text, err := argToCLIText(arg)
		if err != nil {
			return nil, fmt.Errorf("image build arg %d: %w", i+1, err)
		}
		argv = append(argv, text)
	}
	return &ImageBuildCommand{Args: argv}, nil
}

func execImageBuild(ctx context.Context, cli *bpfmancli.CLI, cmd *ImageBuildCommand) error {
	argv := append([]string{"image", "build"}, cmd.Args...)
	child, cancellationErr := newBPFManCommand(ctx, argv...)
	output, err := child.CombinedOutput()
	if len(output) > 0 {
		if err := cli.PrintOut(string(output)); err != nil {
			return err
		}
	}
	if cancelErr := cancellationErr(); cancelErr != nil {
		return cancelErr
	}
	if err != nil {
		return fmt.Errorf("image build: %w", err)
	}
	return nil
}

// ImageInspectCommand represents a parsed "image inspect" command.
type ImageInspectCommand struct {
	ImageURL string
}

func (*ImageInspectCommand) isCommand() {}

func parseImageInspect(args []runtime.Arg) (*ImageInspectCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("image inspect: requires an image reference")
	}
	if len(args) > 1 {
		return nil, syntax.SpanErrorf(runtime.ArgSpan(args[1]), "image inspect: unexpected argument %q", driver.ArgText(args[1]))
	}
	return &ImageInspectCommand{ImageURL: driver.ArgText(args[0])}, nil
}

func execImageInspect(ctx context.Context, cli *bpfmancli.CLI, cmd *ImageInspectCommand) (runtime.Value, error) {
	inspection, err := oci.InspectBytecodeImage(ctx, cmd.ImageURL)
	if err != nil {
		return runtime.Value{}, err
	}
	output, err := json.MarshalIndent(inspection, "", "  ")
	if err != nil {
		return runtime.Value{}, err
	}
	if err := cli.PrintOut(string(output) + "\n"); err != nil {
		return runtime.Value{}, err
	}
	val, err := runtime.ValueFromStruct(inspection)
	if err != nil {
		return runtime.Value{}, err
	}
	return val, nil
}

// parseProgramIDArg resolves a single runtime.Arg directly to a
// kernel.ProgramID, combining argument extraction and ID parsing
// into one step. For text-bearing args the text is parsed as a
// program ID. A StructuredValueArg must carry a program origin
// satisfying HasKernelProgramID, and the ID is read from that
// capability. There is no path-lookup fallback: an origin-less
// structured value (such as a jq-constructed object, whose origin
// is stripped) carries no such capability and is rejected, so a
// program handle cannot be laundered through jq and reused as a
// command argument.
func parseProgramIDArg(a runtime.Arg) (kernel.ProgramID, error) {
	switch v := a.(type) {
	case runtime.WordArg:
		return parseProgramIDText(v.Text)
	case runtime.QuotedArg:
		return parseProgramIDText(v.Text)
	case runtime.ScalarValueArg:
		return parseProgramIDText(v.Text)
	case runtime.StructuredValueArg:
		display := displayName(v.Name)
		if err := runtime.ExpectOrigin(v.Value, display, semantics.OriginProgram); err != nil {
			return 0, err
		}
		origin := v.Value.Origin()
		if origin == nil {
			return 0, fmt.Errorf("%s is structured but carries no kernel ID capability", display)
		}
		x, ok := origin.(bpfman.HasKernelProgramID)
		if !ok {
			return 0, fmt.Errorf("%s is structured but its origin (%T) does not satisfy HasKernelProgramID", display, origin)
		}
		return x.KernelProgramID(), nil
	default:
		return 0, fmt.Errorf("unexpected argument type %T", a)
	}
}

// displayName produces a user-facing reference for a structured
// Value that appears in an argument position. Named variables
// display as "$name"; anonymous values (e.g. the result of a
// nested command substitution) display as "<command result>" so
// that error messages remain intelligible.
func displayName(name string) string {
	if name == "" {
		return "<command result>"
	}
	return "$" + name
}

// parseProgramIDText parses a program ID from text into a
// kernel.ProgramID.
func parseProgramIDText(s string) (kernel.ProgramID, error) {
	parsed, err := bpfmancli.ParseProgramID(s)
	if err != nil {
		return 0, err
	}
	return parsed.Value, nil
}

// parseLinkIDArg resolves a single runtime.Arg directly to a
// bpfman.LinkID, combining argument extraction and ID parsing into
// one step. For text-bearing args the text is parsed as a link ID.
// A StructuredValueArg must carry a link origin satisfying
// HasLinkID, and the ID is read from that capability. There
// is no path-lookup fallback: an origin-less structured value
// (such as a jq-constructed object, whose origin is stripped)
// carries no such capability and is rejected.
func parseLinkIDArg(a runtime.Arg) (bpfman.LinkID, error) {
	switch v := a.(type) {
	case runtime.WordArg:
		return parseLinkIDText(v.Text)
	case runtime.QuotedArg:
		return parseLinkIDText(v.Text)
	case runtime.ScalarValueArg:
		return parseLinkIDText(v.Text)
	case runtime.StructuredValueArg:
		display := displayName(v.Name)
		if err := runtime.ExpectOrigin(v.Value, display, semantics.OriginLink); err != nil {
			return 0, err
		}
		origin := v.Value.Origin()
		if origin == nil {
			return 0, fmt.Errorf("%s is structured but carries no link ID capability", display)
		}
		x, ok := origin.(bpfman.HasLinkID)
		if !ok {
			return 0, fmt.Errorf("%s is structured but its origin (%T) does not satisfy HasLinkID", display, origin)
		}
		return x.LinkID(), nil
	default:
		return 0, fmt.Errorf("unexpected argument type %T", a)
	}
}

// parseLinkIDText parses a link ID from text into a bpfman.LinkID.
func parseLinkIDText(s string) (bpfman.LinkID, error) {
	parsed, err := bpfmancli.ParseLinkID(s)
	if err != nil {
		return 0, err
	}
	return parsed.Value, nil
}

// ShowProgramCommand represents a fully parsed "show program" command
// with resolved program ID, view name, and output format.
type ShowProgramCommand struct {
	ID     kernel.ProgramID
	View   string
	Output cliformat.OutputFlags
}

func (*ShowProgramCommand) isCommand() {}

// validShowViews lists the accepted sub-view names for "show program".
var validShowViews = map[string]bool{
	"summary": true,
	"links":   true,
	"maps":    true,
	"paths":   true,
}

// parseShowProgram resolves expanded shell arguments into a
// ShowProgramCommand. The grammar is:
//
//	<program-id> [view] [-o format]
//
// One required positional (program ID), one optional positional (view
// name defaulting to "summary"), and one optional flag (-o).
func parseShowProgram(args []runtime.Arg) (*ShowProgramCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("show program: requires a program ID")
	}

	// Resolve the program ID from the first argument.
	id, err := parseProgramIDArg(args[0])
	if err != nil {
		return nil, err
	}

	cmd := &ShowProgramCommand{
		ID:   id,
		View: "summary",
		Output: cliformat.OutputFlags{
			Output: cliformat.OutputValue{Value: "table"},
		},
	}

	// Walk the remaining arguments: optional view positional, optional -o flag.
	rest := args[1:]
	viewSet := false
	for i := 0; i < len(rest); i++ {
		text := driver.ArgText(rest[i])
		if text == "-o" {
			flagSpan := runtime.ArgSpan(rest[i])
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(flagSpan, "show program: duplicate -o flag")
			}
			i++
			if i >= len(rest) {
				return nil, syntax.SpanErrorf(flagSpan, "show program: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{
				Value: driver.ArgText(rest[i]),
				IsSet: true,
			}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, syntax.SpanErrorf(runtime.ArgSpan(rest[i]), "show program: unknown flag %q", text)
		}
		if viewSet {
			return nil, syntax.SpanErrorf(runtime.ArgSpan(rest[i]), "show program: only one view may be specified")
		}
		// Treat as view name.
		if !validShowViews[text] {
			return nil, syntax.SpanErrorf(runtime.ArgSpan(rest[i]), "show program: unknown view %q (valid: summary, links, maps, paths)", text)
		}
		cmd.View = text
		viewSet = true
	}

	return cmd, nil
}

// execShowProgram executes a parsed ShowProgramCommand, fetching the
// program from the store and rendering output according to the
// requested view and format.
func execShowProgram(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *ShowProgramCommand) error {
	prog, err := mgr.Get(ctx, cmd.ID)
	if err != nil {
		return err
	}

	format, err := cmd.Output.Format()
	if err != nil {
		return err
	}

	// JSON output always emits the full Program regardless of
	// sub-view; consumers can select fields with jq.
	if format == cliformat.OutputFormatJSON {
		output, err := cliformat.FormatShowJSON(prog)
		if err != nil {
			return err
		}
		return cli.PrintOut(output)
	}

	var output string
	switch cmd.View {
	case "summary":
		var fmtErr error
		output, fmtErr = cliformat.FormatProgram(prog, &cmd.Output)
		if fmtErr != nil {
			return fmtErr
		}
	case "links":
		output = cliformat.FormatShowLinks(prog)
	case "maps":
		output = cliformat.FormatShowMaps(prog)
	case "paths":
		output = cliformat.FormatShowPaths(prog)
	default:
		return fmt.Errorf("unknown view %q (valid: summary, links, maps, paths)", cmd.View)
	}

	return cli.PrintOut(output)
}

// LoadFileCommand represents a fully parsed "load file" command.
type LoadFileCommand struct {
	Path        string
	Programs    []bpfmancli.ProgramSpec
	Metadata    []bpfmancli.KeyValue
	GlobalData  []bpfmancli.GlobalData
	Application string
	MapOwnerID  kernel.ProgramID
	Output      cliformat.OutputFlags
}

func (*LoadFileCommand) isCommand() {}

// parseLoadFile resolves expanded shell arguments into a
// LoadFileCommand. The grammar is:
//
//	-p <path> [--programs <spec>]... [-m <key=val>]... [-g <name=hex>]...
//	[-a <app>] [--map-owner-id <id>] [-o <format>]
func parseLoadFile(args []runtime.Arg) (*LoadFileCommand, error) {
	cmd := &LoadFileCommand{
		Output: cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-p", "--path":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load file: %s requires a value", text)
			}
			cmd.Path = driver.ArgText(args[i])
		case "--programs":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load file: --programs requires a value")
			}
			for _, part := range splitComma(driver.ArgText(args[i])) {
				spec, err := bpfmancli.ParseProgramSpec(part)
				if err != nil {
					return nil, fmt.Errorf("load file: %w", err)
				}
				cmd.Programs = append(cmd.Programs, spec)
			}
		case "-m", "--metadata":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load file: %s requires a value", text)
			}
			kv, err := bpfmancli.ParseKeyValue(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load file: %w", err)
			}
			cmd.Metadata = append(cmd.Metadata, kv)
		case "-g", "--global":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load file: %s requires a value", text)
			}
			gd, err := bpfmancli.ParseGlobalData(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load file: %w", err)
			}
			cmd.GlobalData = append(cmd.GlobalData, gd)
		case "-a", "--application":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load file: %s requires a value", text)
			}
			cmd.Application = driver.ArgText(args[i])
		case "--map-owner-id":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load file: --map-owner-id requires a value")
			}
			parsed, err := bpfmancli.ParseProgramID(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load file: %w", err)
			}
			cmd.MapOwnerID = parsed.Value
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "load file: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load file: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "load file: unknown flag %q", text)
			}
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "load file: unexpected argument %q", text)
		}
	}

	if cmd.Path == "" {
		return nil, fmt.Errorf("load file: --path is required")
	}

	return cmd, nil
}

// execLoadFile executes a parsed LoadFileCommand, loading the BPF
// program from a local object file, printing output, and returning a
// structured Value for optional variable assignment.
func execLoadFile(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *LoadFileCommand) (runtime.Value, error) {
	objPath, err := bpfmancli.ParseObjectPath(cmd.Path)
	if err != nil {
		return runtime.Value{}, err
	}

	// load is lockless by construction (docs/PLAN-load-lockless.md):
	// the kernel BPF_PROG_LOAD, bytecode publish, and single
	// sqlite commit transaction all run without acquiring the
	// writer flock.
	var globalData map[string][]byte
	if len(cmd.GlobalData) > 0 {
		globalData = bpfmancli.GlobalDataMap(cmd.GlobalData)
	}

	req := manager.NewLoadRequest(manager.LoadSource{FilePath: objPath.Path}, loadProgramSpecs(cmd.Programs), manager.LoadRequestOpts{
		UserMetadata: bpfmancli.MetadataMap(cmd.Metadata),
		GlobalData:   globalData,
		Application:  cmd.Application,
		MapOwnerID:   cmd.MapOwnerID,
	})
	loaded, err := mgr.LoadFromRequest(ctx, req)
	if err != nil {
		return runtime.Value{}, fmt.Errorf("failed to load programs: %w", err)
	}
	result := loadFileResult{Programs: loaded}

	output, err := cliformat.FormatLoadedPrograms(result.Programs, &cmd.Output)
	if err != nil {
		return runtime.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return runtime.Value{}, err
	}

	// The DSL value wraps the full load result so callers can address
	// every loaded program. Single-prog scripts read $loaded.programs[0];
	// multi-prog scripts iterate or filter via jq. Same shape as
	// `bpfman program list -o json` so users learn one path pattern.
	val, err := runtime.ValueFromStruct(bpfman.LoadResult{Programs: result.Programs})
	if err != nil {
		return runtime.Value{}, nil
	}
	return val, nil
}

// LinkAttachCommand represents a fully parsed "link attach" command.
// The AttachSpec is constructed at parse time from the type-specific
// flags; execution simply runs it under lock.
type LinkAttachCommand struct {
	Spec   bpfman.AttachSpec
	Output cliformat.OutputFlags
}

func (*LinkAttachCommand) isCommand() {}

// parseLinkAttach parses "link attach <type> <args...>" into a
// LinkAttachCommand. The first argument is the attach type; the
// remaining arguments are type-specific flags and one required
// program ID.
func parseLinkAttach(args []runtime.Arg) (*LinkAttachCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("link attach requires a type (xdp, tc, tcx, tracepoint, kprobe, uprobe, fentry, fexit)")
	}

	attachType := driver.ArgText(args[0])
	rest := args[1:]

	switch attachType {
	case "xdp":
		return parseLinkAttachXDP(rest)
	case "tc":
		return parseLinkAttachTC(rest)
	case "tcx":
		return parseLinkAttachTCX(rest)
	case "tracepoint":
		return parseLinkAttachTracepoint(rest)
	case "kprobe":
		return parseLinkAttachKprobe(rest)
	case "uprobe":
		return parseLinkAttachUprobe(rest)
	case "fentry":
		return parseLinkAttachFentry(rest)
	case "fexit":
		return parseLinkAttachFexit(rest)
	default:
		return nil, fmt.Errorf("unknown attach type %q (valid: xdp, tc, tcx, tracepoint, kprobe, uprobe, fentry, fexit)", attachType)
	}
}

// parseLinkAttachXDP parses "link attach xdp" arguments.
//
//	-i <iface> [-p <priority>] [--proceed-on <actions>]...
//	[-n <netns>] [-o <format>] <program-id>
func parseLinkAttachXDP(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		priority  int
		proceedOn []string
		netns     string
		progArg   runtime.Arg
		output    = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	defaults := true
	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-i", "--iface":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach xdp: %s requires a value", text)
			}
			iface = driver.ArgText(args[i])
		case "-p", "--priority":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach xdp: %s requires a value", text)
			}
			v, err := strconv.Atoi(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("link attach xdp: invalid priority %q: %w", driver.ArgText(args[i]), err)
			}
			priority = v
		case "--proceed-on":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach xdp: --proceed-on requires a value")
			}
			proceedOn = append(proceedOn, splitComma(driver.ArgText(args[i]))...)
			defaults = false
		case "-n", "--netns":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach xdp: %s requires a value", text)
			}
			netns = driver.ArgText(args[i])
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach xdp: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach xdp: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach xdp: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach xdp: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach xdp: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if iface == "" {
		return nil, fmt.Errorf("link attach xdp: --iface is required")
	}
	if progArg == nil {
		return nil, fmt.Errorf("link attach xdp: requires a program ID")
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach xdp: %w", err)
	}

	if defaults {
		proceedOn = []string{"pass", "dispatcher_return"}
	}
	actions, err := bpfman.ParseXDPActions(proceedOn)
	if err != nil {
		return nil, fmt.Errorf("link attach xdp: invalid proceed-on value: %w", err)
	}

	spec, err := bpfman.NewXDPAttachSpec(progID, iface)
	if err != nil {
		return nil, fmt.Errorf("link attach xdp: %w", err)
	}
	spec = spec.WithPriority(priority).WithProceedOnActions(actions)
	if netns != "" {
		spec = spec.WithNetns(netns)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachTC parses "link attach tc" arguments.
//
//	-i <iface> -d <direction> [-p <priority>] [--proceed-on <actions>]...
//	[-n <netns>] [-o <format>] <program-id>
func parseLinkAttachTC(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		direction string
		priority  int
		proceedOn []string
		netns     string
		progArg   runtime.Arg
		output    = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	defaults := true
	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-i", "--iface":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tc: %s requires a value", text)
			}
			iface = driver.ArgText(args[i])
		case "-d", "--direction":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tc: %s requires a value", text)
			}
			direction = driver.ArgText(args[i])
		case "-p", "--priority":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tc: %s requires a value", text)
			}
			v, err := strconv.Atoi(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("link attach tc: invalid priority %q: %w", driver.ArgText(args[i]), err)
			}
			priority = v
		case "--proceed-on":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tc: --proceed-on requires a value")
			}
			proceedOn = append(proceedOn, splitComma(driver.ArgText(args[i]))...)
			defaults = false
		case "-n", "--netns":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tc: %s requires a value", text)
			}
			netns = driver.ArgText(args[i])
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach tc: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tc: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tc: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tc: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tc: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if iface == "" {
		return nil, fmt.Errorf("link attach tc: --iface is required")
	}
	if direction == "" {
		return nil, fmt.Errorf("link attach tc: --direction is required")
	}
	if progArg == nil {
		return nil, fmt.Errorf("link attach tc: requires a program ID")
	}
	if priority < 0 {
		return nil, fmt.Errorf("link attach tc: --priority must be non-negative, got %d", priority)
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: %w", err)
	}

	if defaults {
		proceedOn = []string{"pipe", "dispatcher_return"}
	}
	actions, err := bpfman.ParseTCActions(proceedOn)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: invalid proceed-on value: %w", err)
	}

	spec, err := bpfman.NewTCAttachSpecFromString(progID, iface, direction)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: %w", err)
	}
	spec = spec.WithPriority(priority).WithProceedOnActions(actions)
	if netns != "" {
		spec = spec.WithNetns(netns)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachTCX parses "link attach tcx" arguments.
//
//	-i <iface> -d <direction> [-p <priority>] [-n <netns>]
//	[-o <format>] <program-id>
func parseLinkAttachTCX(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		direction string
		priority  int
		netns     string
		progArg   runtime.Arg
		output    = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-i", "--iface":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tcx: %s requires a value", text)
			}
			iface = driver.ArgText(args[i])
		case "-d", "--direction":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tcx: %s requires a value", text)
			}
			direction = driver.ArgText(args[i])
		case "-p", "--priority":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tcx: %s requires a value", text)
			}
			v, err := strconv.Atoi(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("link attach tcx: invalid priority %q: %w", driver.ArgText(args[i]), err)
			}
			priority = v
		case "-n", "--netns":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tcx: %s requires a value", text)
			}
			netns = driver.ArgText(args[i])
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach tcx: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tcx: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tcx: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tcx: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tcx: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if iface == "" {
		return nil, fmt.Errorf("link attach tcx: --iface is required")
	}
	if direction == "" {
		return nil, fmt.Errorf("link attach tcx: --direction is required")
	}
	if progArg == nil {
		return nil, fmt.Errorf("link attach tcx: requires a program ID")
	}
	if priority < 0 {
		return nil, fmt.Errorf("link attach tcx: --priority must be non-negative, got %d", priority)
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: %w", err)
	}

	spec, err := bpfman.NewTCXAttachSpecFromString(progID, iface, direction)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: %w", err)
	}
	spec = spec.WithPriority(priority)
	if netns != "" {
		spec = spec.WithNetns(netns)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachTracepoint parses "link attach tracepoint" arguments.
//
//	[-o <format>] <program-id> <group/name>
func parseLinkAttachTracepoint(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		progArg    runtime.Arg
		tracepoint string
		output     = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach tracepoint: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tracepoint: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach tracepoint: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tracepoint: unknown flag %q", text)
			}
			switch {
			case progArg == nil:
				progArg = args[i]
			case tracepoint == "":
				tracepoint = text
			default:
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach tracepoint: unexpected argument %q", text)
			}
		}
	}

	if progArg == nil {
		return nil, fmt.Errorf("link attach tracepoint: requires a program ID")
	}
	if tracepoint == "" {
		return nil, fmt.Errorf("link attach tracepoint: requires a tracepoint in group/name form")
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tracepoint: %w", err)
	}

	spec, err := bpfman.NewTracepointAttachSpecFromString(progID, tracepoint)
	if err != nil {
		return nil, fmt.Errorf("link attach tracepoint: %w", err)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachKprobe parses "link attach kprobe" arguments.
//
//	-f <fn-name> [--offset <n>] [-o <format>] <program-id>
func parseLinkAttachKprobe(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		fnName  string
		offset  uint64
		progArg runtime.Arg
		output  = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-f", "--fn-name":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach kprobe: %s requires a value", text)
			}
			fnName = driver.ArgText(args[i])
		case "--offset":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach kprobe: --offset requires a value")
			}
			v, err := strconv.ParseUint(driver.ArgText(args[i]), 0, 64)
			if err != nil {
				return nil, fmt.Errorf("link attach kprobe: invalid offset %q: %w", driver.ArgText(args[i]), err)
			}
			offset = v
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach kprobe: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach kprobe: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach kprobe: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach kprobe: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach kprobe: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if fnName == "" {
		return nil, fmt.Errorf("link attach kprobe: --fn-name is required")
	}
	if progArg == nil {
		return nil, fmt.Errorf("link attach kprobe: requires a program ID")
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach kprobe: %w", err)
	}

	spec, err := bpfman.NewKprobeAttachSpec(progID, fnName)
	if err != nil {
		return nil, fmt.Errorf("link attach kprobe: %w", err)
	}
	if offset != 0 {
		spec = spec.WithOffset(offset)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachUprobe parses "link attach uprobe" arguments.
//
//	--target <path> [-f <fn-name>] [--offset <n>] [--container-pid <pid>]
//	[-o <format>] <program-id>
func parseLinkAttachUprobe(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		target       string
		fnName       string
		offset       uint64
		containerPid int32
		progArg      runtime.Arg
		output       = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "--target":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach uprobe: --target requires a value")
			}
			target = driver.ArgText(args[i])
		case "-f", "--fn-name":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach uprobe: %s requires a value", text)
			}
			fnName = driver.ArgText(args[i])
		case "--offset":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach uprobe: --offset requires a value")
			}
			v, err := strconv.ParseUint(driver.ArgText(args[i]), 0, 64)
			if err != nil {
				return nil, fmt.Errorf("link attach uprobe: invalid offset %q: %w", driver.ArgText(args[i]), err)
			}
			offset = v
		case "--container-pid":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach uprobe: --container-pid requires a value")
			}
			v, err := strconv.ParseInt(driver.ArgText(args[i]), 10, 32)
			if err != nil {
				return nil, fmt.Errorf("link attach uprobe: invalid container-pid %q: %w", driver.ArgText(args[i]), err)
			}
			containerPid = int32(v)
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach uprobe: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach uprobe: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach uprobe: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach uprobe: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach uprobe: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if target == "" {
		return nil, fmt.Errorf("link attach uprobe: --target is required")
	}
	if progArg == nil {
		return nil, fmt.Errorf("link attach uprobe: requires a program ID")
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach uprobe: %w", err)
	}

	spec, err := bpfman.NewUprobeAttachSpec(progID, target)
	if err != nil {
		return nil, fmt.Errorf("link attach uprobe: %w", err)
	}
	if fnName != "" {
		spec = spec.WithFnName(fnName)
	}
	if offset != 0 {
		spec = spec.WithOffset(offset)
	}
	if containerPid > 0 {
		spec = spec.WithContainerPid(containerPid)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachFentry parses "link attach fentry" arguments.
//
//	[-o <format>] <program-id>
func parseLinkAttachFentry(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		progArg runtime.Arg
		output  = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach fentry: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach fentry: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach fentry: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach fentry: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach fentry: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if progArg == nil {
		return nil, fmt.Errorf("link attach fentry: requires a program ID")
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach fentry: %w", err)
	}

	spec, err := bpfman.NewFentryAttachSpec(progID)
	if err != nil {
		return nil, fmt.Errorf("link attach fentry: %w", err)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachFexit parses "link attach fexit" arguments.
//
//	[-o <format>] <program-id>
func parseLinkAttachFexit(args []runtime.Arg) (*LinkAttachCommand, error) {
	var (
		progArg runtime.Arg
		output  = cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach fexit: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach fexit: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link attach fexit: -o requires a value")
			}
			output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach fexit: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link attach fexit: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if progArg == nil {
		return nil, fmt.Errorf("link attach fexit: requires a program ID")
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach fexit: %w", err)
	}

	spec, err := bpfman.NewFexitAttachSpec(progID)
	if err != nil {
		return nil, fmt.Errorf("link attach fexit: %w", err)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// execLinkAttach executes a parsed LinkAttachCommand, attaching the
// BPF program under lock, printing output, and returning a structured
// Value for optional variable assignment.
func execLinkAttach(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *LinkAttachCommand) (runtime.Value, error) {
	link, err := bpfmancli.RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (bpfman.Link, error) {
		return mgr.Attach(ctx, writeLock, cmd.Spec)
	})
	if err != nil {
		return runtime.Value{}, err
	}

	output, err := cliformat.FormatLinkResult(link, &cmd.Output)
	if err != nil {
		return runtime.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return runtime.Value{}, err
	}

	val, err := runtime.ValueFromStruct(link)
	if err != nil {
		return runtime.Value{}, nil
	}
	return val.WithKind(semantics.OriginLink), nil
}

// LinkDetachCommand represents a fully parsed "link detach" command
// with resolved link IDs ready for execution.
type LinkDetachCommand struct {
	LinkIDs []bpfman.LinkID
}

func (*LinkDetachCommand) isCommand() {}

// parseLinkDetach resolves expanded shell arguments into a
// LinkDetachCommand. Each positional argument is a link ID, possibly
// from a structured variable reference.
func parseLinkDetach(args []runtime.Arg) (*LinkDetachCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("link detach: requires at least one link ID")
	}

	var ids []bpfman.LinkID
	for _, a := range args {
		id, err := parseLinkIDArg(a)
		if err != nil {
			return nil, fmt.Errorf("link detach: %w", err)
		}
		ids = append(ids, id)
	}

	return &LinkDetachCommand{LinkIDs: ids}, nil
}

// execLinkDetach executes a parsed LinkDetachCommand, detaching each
// link under lock.
func execLinkDetach(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *LinkDetachCommand) error {
	return bpfmancli.RunBatchMutation(ctx, cli, cmd.LinkIDs, "link", "detach",
		func(ctx context.Context, writeLock lock.WriterScope, id bpfman.LinkID) error {
			return mgr.Detach(ctx, writeLock, id)
		})
}

// splitComma splits a string on commas and trims whitespace from each element.
func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// LoadImageCommand represents a fully parsed "load image" command.
type LoadImageCommand struct {
	ImageURL     string
	Programs     []bpfmancli.ProgramSpec
	PullPolicy   string
	RegistryAuth string
	Application  string
	Metadata     []bpfmancli.KeyValue
	GlobalData   []bpfmancli.GlobalData
	MapOwnerID   kernel.ProgramID
	Output       cliformat.OutputFlags
}

func (*LoadImageCommand) isCommand() {}

// parseLoadImage resolves expanded shell arguments into a
// LoadImageCommand. The grammar is:
//
//	-i <url> [--programs <spec>]... [-p <policy>] [--registry-auth <auth>]
//	[-a <app>] [--map-owner-id <id>] [-m <key=val>]... [-g <name=hex>]...
//	[-o <format>]
func parseLoadImage(args []runtime.Arg) (*LoadImageCommand, error) {
	cmd := &LoadImageCommand{
		PullPolicy: "IfNotPresent",
		Output:     cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-i", "--image-url":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: %s requires a value", text)
			}
			cmd.ImageURL = driver.ArgText(args[i])
		case "--programs":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: --programs requires a value")
			}
			for _, part := range splitComma(driver.ArgText(args[i])) {
				spec, err := bpfmancli.ParseProgramSpec(part)
				if err != nil {
					return nil, fmt.Errorf("load image: %w", err)
				}
				cmd.Programs = append(cmd.Programs, spec)
			}
		case "-p", "--pull-policy":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: %s requires a value", text)
			}
			cmd.PullPolicy = driver.ArgText(args[i])
		case "--registry-auth":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: --registry-auth requires a value")
			}
			cmd.RegistryAuth = driver.ArgText(args[i])
		case "-a", "--application":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: %s requires a value", text)
			}
			cmd.Application = driver.ArgText(args[i])
		case "--map-owner-id":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: --map-owner-id requires a value")
			}
			parsed, err := bpfmancli.ParseProgramID(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			cmd.MapOwnerID = parsed.Value
		case "-m", "--metadata":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: %s requires a value", text)
			}
			kv, err := bpfmancli.ParseKeyValue(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			cmd.Metadata = append(cmd.Metadata, kv)
		case "-g", "--global":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: %s requires a value", text)
			}
			gd, err := bpfmancli.ParseGlobalData(driver.ArgText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			cmd.GlobalData = append(cmd.GlobalData, gd)
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "load image: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "load image: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "load image: unknown flag %q", text)
			}
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "load image: unexpected argument %q", text)
		}
	}

	if cmd.ImageURL == "" {
		return nil, fmt.Errorf("load image: --image-url is required")
	}

	return cmd, nil
}

// execLoadImage executes a parsed LoadImageCommand, loading BPF
// programs from an OCI image, printing output, and returning a
// structured Value for optional variable assignment.
func execLoadImage(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *LoadImageCommand) (runtime.Value, error) {
	pullPolicy, err := bpfman.ParseImagePullPolicy(cmd.PullPolicy)
	if err != nil {
		return runtime.Value{}, fmt.Errorf("load image: invalid pull policy %q: %w", cmd.PullPolicy, err)
	}

	type loadImageResult struct {
		Programs []bpfman.Program
	}

	// load is lockless by construction (docs/PLAN-load-lockless.md):
	// the OCI pull, kernel BPF_PROG_LOAD, bytecode publish, and
	// single sqlite commit transaction all run without acquiring
	// the writer flock.
	var globalData map[string][]byte
	if len(cmd.GlobalData) > 0 {
		globalData = bpfmancli.GlobalDataMap(cmd.GlobalData)
	}

	ref := platform.ImageRef{
		URL:        cmd.ImageURL,
		PullPolicy: pullPolicy,
	}

	if cmd.RegistryAuth != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(cmd.RegistryAuth)
		if decErr != nil {
			return runtime.Value{}, fmt.Errorf("invalid registry-auth: invalid base64 encoding: %w", decErr)
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return runtime.Value{}, fmt.Errorf("invalid registry-auth: expected 'username:password' format")
		}
		if parts[0] == "" || parts[1] == "" {
			return runtime.Value{}, fmt.Errorf("invalid registry-auth: username and password must both be non-empty")
		}
		ref.Auth = &platform.ImageAuth{
			Username: parts[0],
			Password: parts[1],
		}
	}

	req := manager.NewLoadRequest(manager.LoadSource{Image: &ref}, loadProgramSpecs(cmd.Programs), manager.LoadRequestOpts{
		UserMetadata: bpfmancli.MetadataMap(cmd.Metadata),
		GlobalData:   globalData,
		Application:  cmd.Application,
		MapOwnerID:   cmd.MapOwnerID,
	})
	loaded, err := mgr.LoadFromRequest(ctx, req)
	if err != nil {
		return runtime.Value{}, fmt.Errorf("failed to load from image: %w", err)
	}
	result := loadImageResult{Programs: loaded}

	output, err := cliformat.FormatLoadedPrograms(result.Programs, &cmd.Output)
	if err != nil {
		return runtime.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return runtime.Value{}, err
	}

	// See parseLoadFile for the rationale: wrap as {programs: [...]}
	// so the DSL surface for load matches list and never silently
	// drops loaded programs.
	val, err := runtime.ValueFromStruct(bpfman.LoadResult{Programs: result.Programs})
	if err != nil {
		return runtime.Value{}, nil
	}
	return val, nil
}

// GetProgramCommand represents a fully parsed "program get" command
// with a resolved program ID and output format.
type GetProgramCommand struct {
	ID     kernel.ProgramID
	Output cliformat.OutputFlags
}

func (*GetProgramCommand) isCommand() {}

// parseGetProgram resolves expanded shell arguments into a
// GetProgramCommand. The grammar is:
//
//	<program-id> [-o format]
func parseGetProgram(args []runtime.Arg) (*GetProgramCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("program get: requires a program ID")
	}

	id, err := parseProgramIDArg(args[0])
	if err != nil {
		return nil, fmt.Errorf("program get: %w", err)
	}

	cmd := &GetProgramCommand{
		ID: id,
		Output: cliformat.OutputFlags{
			Output: cliformat.OutputValue{Value: "table"},
		},
	}

	for i := 1; i < len(args); i++ {
		text := driver.ArgText(args[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "program get: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "program get: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{
				Value: driver.ArgText(args[i]),
				IsSet: true,
			}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "program get: unknown flag %q", text)
		}
		return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "program get: unexpected argument %q", text)
	}

	return cmd, nil
}

// execGetProgram executes a parsed GetProgramCommand, fetching the
// program from the store, rendering output, and returning a
// structured Value for variable assignment.
func execGetProgram(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *GetProgramCommand) (runtime.Value, error) {
	prog, err := mgr.Get(ctx, cmd.ID)
	if err != nil {
		return runtime.Value{}, err
	}

	output, err := cliformat.FormatProgram(prog, &cmd.Output)
	if err != nil {
		return runtime.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return runtime.Value{}, err
	}

	val, err := runtime.ValueFromStruct(prog)
	if err != nil {
		return runtime.Value{}, nil
	}
	return val.WithKind(semantics.OriginProgram), nil
}

// GetLinkCommand represents a fully parsed "link get" command with a
// resolved link ID and output format.
type GetLinkCommand struct {
	ID     bpfman.LinkID
	Output cliformat.OutputFlags
}

func (*GetLinkCommand) isCommand() {}

// parseGetLink resolves expanded shell arguments into a
// GetLinkCommand. The grammar is:
//
//	<link-id> [-o format]
func parseGetLink(args []runtime.Arg) (*GetLinkCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("link get: requires a link ID")
	}

	id, err := parseLinkIDArg(args[0])
	if err != nil {
		return nil, fmt.Errorf("link get: %w", err)
	}

	cmd := &GetLinkCommand{
		ID: id,
		Output: cliformat.OutputFlags{
			Output: cliformat.OutputValue{Value: "table"},
		},
	}

	for i := 1; i < len(args); i++ {
		text := driver.ArgText(args[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link get: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link get: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{
				Value: driver.ArgText(args[i]),
				IsSet: true,
			}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link get: unknown flag %q", text)
		}
		return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link get: unexpected argument %q", text)
	}

	return cmd, nil
}

// execGetLink executes a parsed GetLinkCommand, fetching the link
// from the store, rendering output, and returning a structured Value
// for variable assignment.
func execGetLink(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *GetLinkCommand) (runtime.Value, error) {
	info, err := mgr.GetLinkInfo(ctx, cmd.ID)
	if err != nil {
		return runtime.Value{}, err
	}

	link := bpfman.Link{
		Record: info.Record,
		Status: bpfman.LinkStatus{
			Kernel:     info.Kernel,
			KernelSeen: info.Presence.InKernel,
			PinPresent: info.Presence.InFS,
		},
	}

	output, fmtErr := cliformat.FormatLinkResult(link, &cmd.Output)
	if fmtErr != nil {
		return runtime.Value{}, fmtErr
	}
	if err := cli.PrintOut(output); err != nil {
		return runtime.Value{}, err
	}

	val, err := runtime.ValueFromStruct(link)
	if err != nil {
		return runtime.Value{}, nil
	}
	return val.WithKind(semantics.OriginLink), nil
}

// UnloadProgramCommand represents a fully parsed "program unload"
// command with resolved program IDs.
type UnloadProgramCommand struct {
	ProgramIDs []kernel.ProgramID
}

func (*UnloadProgramCommand) isCommand() {}

// parseUnloadProgram resolves expanded shell arguments into an
// UnloadProgramCommand. Each positional argument is a program ID.
func parseUnloadProgram(args []runtime.Arg) (*UnloadProgramCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("program unload: requires at least one program ID")
	}

	var ids []kernel.ProgramID
	for _, a := range args {
		id, err := parseProgramIDArg(a)
		if err != nil {
			return nil, fmt.Errorf("program unload: %w", err)
		}
		ids = append(ids, id)
	}

	return &UnloadProgramCommand{ProgramIDs: ids}, nil
}

// execUnloadProgram executes a parsed UnloadProgramCommand, unloading
// each program under lock.
func execUnloadProgram(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *UnloadProgramCommand) error {
	return bpfmancli.RunBatchMutation(ctx, cli, cmd.ProgramIDs, "program", "unload",
		func(ctx context.Context, writeLock lock.WriterScope, id kernel.ProgramID) error {
			return mgr.Unload(ctx, writeLock, id)
		})
}

// DeleteProgramCommand represents a fully parsed "program delete"
// command with resolved program IDs and flags.
type DeleteProgramCommand struct {
	ProgramIDs []kernel.ProgramID
	All        bool
	Recursive  bool
}

func (*DeleteProgramCommand) isCommand() {}

// parseDeleteProgram resolves expanded shell arguments into a
// DeleteProgramCommand. The grammar is:
//
//	(<program-id>... | --all) [-r]
func parseDeleteProgram(args []runtime.Arg) (*DeleteProgramCommand, error) {
	cmd := &DeleteProgramCommand{}

	var positionals []runtime.Arg
	for i := range args {
		text := driver.ArgText(args[i])
		switch text {
		case "--all":
			cmd.All = true
		case "-r", "--recursive":
			cmd.Recursive = true
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "program delete: unknown flag %q", text)
			}
			positionals = append(positionals, args[i])
		}
	}

	if cmd.All && len(positionals) > 0 {
		return nil, fmt.Errorf("program delete: --all and explicit program IDs are mutually exclusive")
	}
	if !cmd.All && len(positionals) == 0 {
		return nil, fmt.Errorf("program delete: provide at least one program ID or use --all")
	}

	for _, a := range positionals {
		id, err := parseProgramIDArg(a)
		if err != nil {
			return nil, fmt.Errorf("program delete: %w", err)
		}
		cmd.ProgramIDs = append(cmd.ProgramIDs, id)
	}

	return cmd, nil
}

// execDeleteProgram executes a parsed DeleteProgramCommand. When All
// is set, every managed program is collected first. Each program is
// deleted with cascading cleanup under lock.
func execDeleteProgram(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *DeleteProgramCommand) error {
	ids, err := collectDeleteIDs(ctx, mgr, cmd.All, cmd.ProgramIDs)
	if err != nil {
		return err
	}
	return executeDeletePrograms(ctx, cli, mgr, ids, cmd.Recursive, cmd.All)
}

// DeleteLinkCommand represents a fully parsed "link delete" command
// with resolved link IDs and flags.
type DeleteLinkCommand struct {
	LinkIDs   []bpfman.LinkID
	Recursive bool
}

func (*DeleteLinkCommand) isCommand() {}

// parseDeleteLink resolves expanded shell arguments into a
// DeleteLinkCommand. The grammar is:
//
//	<link-id>... [-r]
func parseDeleteLink(args []runtime.Arg) (*DeleteLinkCommand, error) {
	cmd := &DeleteLinkCommand{}

	var positionals []runtime.Arg
	for i := range args {
		text := driver.ArgText(args[i])
		switch text {
		case "-r", "--recursive":
			cmd.Recursive = true
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link delete: unknown flag %q", text)
			}
			positionals = append(positionals, args[i])
		}
	}

	if len(positionals) == 0 {
		return nil, fmt.Errorf("link delete: requires at least one link ID")
	}

	for _, a := range positionals {
		id, err := parseLinkIDArg(a)
		if err != nil {
			return nil, fmt.Errorf("link delete: %w", err)
		}
		cmd.LinkIDs = append(cmd.LinkIDs, id)
	}

	return cmd, nil
}

// execDeleteLink executes a parsed DeleteLinkCommand, deleting each
// link with cascading cleanup under lock.
func execDeleteLink(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *DeleteLinkCommand) error {
	type result struct {
		id  bpfman.LinkID
		err error
	}
	results := make([]result, 0, len(cmd.LinkIDs))

	lockErr := bpfmancli.RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		deleteResults := mgr.DeleteLinks(ctx, writeLock, cmd.LinkIDs, manager.DeleteLinksOpts{Recursive: cmd.Recursive})
		for _, r := range deleteResults {
			results = append(results, result{id: r.LinkID, err: r.Err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = cli.PrintErrf("link %d: %v\n", r.id, r.err)
			failCount++
		}
	}
	if failCount > 0 {
		return fmt.Errorf("%d of %d link(s) failed to delete", failCount, len(results))
	}
	return nil
}

// ListProgramsCommand represents a fully parsed "program list"
// command with filter flags and output format.
type ListProgramsCommand struct {
	Quiet      bool
	Attached   bool
	Unattached bool
	Types      []bpfman.ProgramType
	Selector   string
	Output     cliformat.OutputFlags
}

func (*ListProgramsCommand) isCommand() {}

// parseListPrograms resolves expanded shell arguments into a
// ListProgramsCommand. The grammar is:
//
//	[-q] [--attached|--unattached] [--type <types>]...
//	[-l <selector>] [-o <format>]
func parseListPrograms(args []runtime.Arg) (*ListProgramsCommand, error) {
	cmd := &ListProgramsCommand{
		Output: cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-q", "--quiet":
			cmd.Quiet = true
		case "--attached":
			cmd.Attached = true
		case "--unattached":
			cmd.Unattached = true
		case "--type":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "program list: --type requires a value")
			}
			for _, part := range splitComma(driver.ArgText(args[i])) {
				progType, err := bpfman.ParseProgramType(strings.ToLower(strings.TrimSpace(part)))
				if err != nil {
					return nil, fmt.Errorf("program list: %w", err)
				}
				cmd.Types = append(cmd.Types, progType)
			}
		case "-l", "--selector":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "program list: %s requires a value", text)
			}
			cmd.Selector = driver.ArgText(args[i])
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "program list: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "program list: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "program list: unknown flag %q", text)
			}
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "program list: unexpected argument %q", text)
		}
	}

	if cmd.Attached && cmd.Unattached {
		return nil, fmt.Errorf("program list: --attached and --unattached are mutually exclusive")
	}

	return cmd, nil
}

// execListPrograms executes a parsed ListProgramsCommand, listing
// programs from the store, rendering output, and returning the
// result as a bindable value so shell scripts can inspect the list
// structurally.
func execListPrograms(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *ListProgramsCommand) (runtime.Value, error) {
	var opts []bpfman.ListOption

	if cmd.Attached {
		opts = append(opts, bpfman.WithAttached())
	} else if cmd.Unattached {
		opts = append(opts, bpfman.WithUnattached())
	}

	if len(cmd.Types) > 0 {
		opts = append(opts, bpfman.WithTypes(cmd.Types...))
	}

	if s := strings.TrimSpace(cmd.Selector); s != "" {
		sel, err := labels.Parse(s)
		if err != nil {
			return runtime.Value{}, fmt.Errorf("invalid label selector: %w", err)
		}
		opts = append(opts, bpfman.MatchingSelector(sel))
	}

	result, err := mgr.ListPrograms(ctx, opts...)
	if err != nil {
		return runtime.Value{}, err
	}

	if len(result.Programs) > 0 || cmd.Output.IsStructured() {
		if cmd.Quiet {
			var b strings.Builder
			for _, p := range result.Programs {
				fmt.Fprintf(&b, "program/%d\n", p.Record.ProgramID)
			}
			if err := cli.PrintOut(b.String()); err != nil {
				return runtime.Value{}, err
			}
		} else {
			output, err := cliformat.FormatProgramsComposite(result, &cmd.Output)
			if err != nil {
				return runtime.Value{}, err
			}
			if err := cli.PrintOut(output); err != nil {
				return runtime.Value{}, err
			}
		}
	}

	val, err := runtime.ValueFromStruct(result)
	if err != nil {
		return runtime.Value{}, nil
	}
	return val, nil
}

// ListLinksCommand represents a fully parsed "link list" command with
// filter flags and output format.
type ListLinksCommand struct {
	Quiet     bool
	ProgramID *kernel.ProgramID
	Kinds     []bpfman.LinkKind
	Output    cliformat.OutputFlags
}

func (*ListLinksCommand) isCommand() {}

// parseListLinks resolves expanded shell arguments into a
// ListLinksCommand. The grammar is:
//
//	[-q] [--program-id <id>] [--kind <kinds>]... [-o <format>]
func parseListLinks(args []runtime.Arg) (*ListLinksCommand, error) {
	cmd := &ListLinksCommand{
		Output: cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "-q", "--quiet":
			cmd.Quiet = true
		case "--program-id":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link list: --program-id requires a value")
			}
			id, err := parseProgramIDArg(args[i])
			if err != nil {
				return nil, fmt.Errorf("link list: %w", err)
			}
			cmd.ProgramID = &id
		case "--kind":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link list: --kind requires a value")
			}
			for _, part := range splitComma(driver.ArgText(args[i])) {
				kind, err := bpfman.ParseLinkKind(strings.ToLower(strings.TrimSpace(part)))
				if err != nil {
					return nil, fmt.Errorf("link list: %w", err)
				}
				cmd.Kinds = append(cmd.Kinds, kind)
			}
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link list: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "link list: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link list: unknown flag %q", text)
			}
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "link list: unexpected argument %q", text)
		}
	}

	return cmd, nil
}

// execListLinks executes a parsed ListLinksCommand, listing links
// from the store and rendering output.
func execListLinks(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *ListLinksCommand) (runtime.Value, error) {
	var opts []bpfman.LinkListOption

	if cmd.ProgramID != nil {
		opts = append(opts, bpfman.WithProgramID(*cmd.ProgramID))
	}

	if len(cmd.Kinds) > 0 {
		opts = append(opts, bpfman.WithKinds(cmd.Kinds...))
	}

	links, err := mgr.ListLinks(ctx, opts...)
	if err != nil {
		return runtime.Value{}, err
	}

	if len(links) > 0 || cmd.Output.IsStructured() {
		if cmd.Quiet {
			var b strings.Builder
			for _, l := range links {
				fmt.Fprintf(&b, "link/%d\n", l.ID)
			}
			if err := cli.PrintOut(b.String()); err != nil {
				return runtime.Value{}, err
			}
		} else {
			output, err := cliformat.FormatLinkList(links, &cmd.Output)
			if err != nil {
				return runtime.Value{}, err
			}
			if err := cli.PrintOut(output); err != nil {
				return runtime.Value{}, err
			}
		}
	}

	val, err := runtime.ValueFromStruct(bpfman.LinkListResult{Links: links})
	if err != nil {
		return runtime.Value{}, nil
	}
	return val, nil
}

// DispatcherListCommand represents a fully parsed "dispatcher list"
// command with optional key filters and output format. Zero values
// mean unfiltered: nsid 0 and ifindex 0 never identify a real
// dispatcher, matching the zero DispatcherType sentinel.
type DispatcherListCommand struct {
	Type    dispatcher.DispatcherType
	Nsid    uint64
	Ifindex uint32
	Output  cliformat.OutputFlags
}

func (*DispatcherListCommand) isCommand() {}

// parseDispatcherList resolves expanded shell arguments into a
// DispatcherListCommand. The grammar is:
//
//	[--type <type>] [--nsid <nsid>] [--ifindex <ifindex>] [-o <format>]
func parseDispatcherList(args []runtime.Arg) (*DispatcherListCommand, error) {
	cmd := &DispatcherListCommand{
		Output: cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := driver.ArgText(args[i])
		switch text {
		case "--type":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "dispatcher list: --type requires a value")
			}
			typ, err := dispatcher.ParseDispatcherType(strings.ToLower(strings.TrimSpace(driver.ArgText(args[i]))))
			if err != nil {
				return nil, fmt.Errorf("dispatcher list: %w", err)
			}
			cmd.Type = typ
		case "--nsid":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "dispatcher list: --nsid requires a value")
			}
			nsid, err := strconv.ParseUint(driver.ArgText(args[i]), 10, 64)
			if err != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher list: invalid nsid %q: %v", driver.ArgText(args[i]), err)
			}
			cmd.Nsid = nsid
		case "--ifindex":
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "dispatcher list: --ifindex requires a value")
			}
			ifindex, err := strconv.ParseUint(driver.ArgText(args[i]), 10, 32)
			if err != nil {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher list: invalid ifindex %q: %v", driver.ArgText(args[i]), err)
			}
			cmd.Ifindex = uint32(ifindex)
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher list: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "dispatcher list: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher list: unknown flag %q", text)
			}
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher list: unexpected argument %q", text)
		}
	}

	return cmd, nil
}

// execDispatcherList executes a parsed DispatcherListCommand, listing
// dispatchers from the store, rendering output, and returning the
// filtered summaries as a bindable value so scripts can assert on
// the result the way they do with link list.
func execDispatcherList(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *DispatcherListCommand) (runtime.Value, error) {
	summaries, err := mgr.ListDispatcherSummaries(ctx)
	if err != nil {
		return runtime.Value{}, err
	}

	filter := dispatcher.KeyFilter{Type: cmd.Type, Nsid: cmd.Nsid, Ifindex: cmd.Ifindex}
	filtered := summaries[:0]
	for _, s := range summaries {
		if filter.Matches(s.Key) {
			filtered = append(filtered, s)
		}
	}
	summaries = filtered

	if len(summaries) > 0 || cmd.Output.IsStructured() {
		output, err := cliformat.FormatDispatcherList(summaries, &cmd.Output)
		if err != nil {
			return runtime.Value{}, err
		}
		if err := cli.PrintOut(output); err != nil {
			return runtime.Value{}, err
		}
	}

	if summaries == nil {
		summaries = []platform.DispatcherSummary{}
	}
	return runtime.ValueFromStruct(platform.DispatcherListResult{Dispatchers: summaries})
}

// DispatcherGetCommand represents a fully parsed "dispatcher get"
// command with resolved dispatcher key and output format.
type DispatcherGetCommand struct {
	Key    dispatcher.Key
	Output cliformat.OutputFlags
}

func (*DispatcherGetCommand) isCommand() {}

// parseDispatcherGet resolves expanded shell arguments into a
// DispatcherGetCommand. The grammar is:
//
//	<type> <nsid> <ifindex> [-o <format>]
func parseDispatcherGet(args []runtime.Arg) (*DispatcherGetCommand, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("dispatcher get: requires <type> <nsid> <ifindex>")
	}

	dispType, err := dispatcher.ParseDispatcherType(strings.ToLower(strings.TrimSpace(driver.ArgText(args[0]))))
	if err != nil {
		return nil, fmt.Errorf("dispatcher get: %w", err)
	}

	nsid, err := strconv.ParseUint(driver.ArgText(args[1]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("dispatcher get: invalid nsid %q: %w", driver.ArgText(args[1]), err)
	}

	ifindex, err := strconv.ParseUint(driver.ArgText(args[2]), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("dispatcher get: invalid ifindex %q: %w", driver.ArgText(args[2]), err)
	}

	cmd := &DispatcherGetCommand{
		Key:    dispatcher.NewKey(dispType, nsid, uint32(ifindex)),
		Output: cliformat.OutputFlags{Output: cliformat.OutputValue{Value: "table"}},
	}

	for i := 3; i < len(args); i++ {
		text := driver.ArgText(args[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher get: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i-1]), "dispatcher get: -o requires a value")
			}
			cmd.Output.Output = cliformat.OutputValue{Value: driver.ArgText(args[i]), IsSet: true}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher get: unknown flag %q", text)
		}
		return nil, syntax.SpanErrorf(runtime.ArgSpan(args[i]), "dispatcher get: unexpected argument %q", text)
	}

	return cmd, nil
}

// execDispatcherGet executes a parsed DispatcherGetCommand, fetching
// the dispatcher snapshot, printing the formatted view to cli.Out
// (suppressed when run as a bind RHS), and returning the snapshot
// as a typed Value so callers using `guard snap <- bpfman dispatcher
// get ...` get a structured handle they can walk.
func execDispatcherGet(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *DispatcherGetCommand) (runtime.Value, error) {
	snap, err := mgr.GetDispatcherSnapshot(ctx, cmd.Key)
	if err != nil {
		return runtime.Value{}, err
	}

	output, err := cliformat.FormatDispatcherSnapshot(snap, &cmd.Output)
	if err != nil {
		return runtime.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return runtime.Value{}, err
	}
	val, err := runtime.ValueFromStruct(snap)
	if err != nil {
		return runtime.Value{}, err
	}
	return val, nil
}

// DispatcherDeleteCommand represents a fully parsed "dispatcher
// delete" command with a resolved dispatcher key.
type DispatcherDeleteCommand struct {
	Key dispatcher.Key
}

func (*DispatcherDeleteCommand) isCommand() {}

// parseDispatcherDelete resolves expanded shell arguments into a
// DispatcherDeleteCommand. The grammar is:
//
//	<type> <nsid> <ifindex>
func parseDispatcherDelete(args []runtime.Arg) (*DispatcherDeleteCommand, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("dispatcher delete: requires <type> <nsid> <ifindex>")
	}

	dispType, err := dispatcher.ParseDispatcherType(strings.ToLower(strings.TrimSpace(driver.ArgText(args[0]))))
	if err != nil {
		return nil, fmt.Errorf("dispatcher delete: %w", err)
	}

	nsid, err := strconv.ParseUint(driver.ArgText(args[1]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("dispatcher delete: invalid nsid %q: %w", driver.ArgText(args[1]), err)
	}

	ifindex, err := strconv.ParseUint(driver.ArgText(args[2]), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("dispatcher delete: invalid ifindex %q: %w", driver.ArgText(args[2]), err)
	}

	if len(args) > 3 {
		return nil, fmt.Errorf("dispatcher delete: unexpected argument %q", driver.ArgText(args[3]))
	}

	return &DispatcherDeleteCommand{
		Key: dispatcher.NewKey(dispType, nsid, uint32(ifindex)),
	}, nil
}

// execDispatcherDelete executes a parsed DispatcherDeleteCommand,
// deleting the dispatcher under lock.
func execDispatcherDelete(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, cmd *DispatcherDeleteCommand) error {
	return bpfmancli.RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		return mgr.DeleteDispatcherSnapshot(ctx, writeLock, cmd.Key)
	})
}
