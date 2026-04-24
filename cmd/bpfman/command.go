package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/coherency"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/shell"
)

// Command is the sealed interface for typed command nodes produced by
// parsing expanded REPL arguments. Each concrete variant carries
// fully resolved, validated fields ready for execution.
type Command interface {
	isCommand()
}

// parseCommand routes expanded domain command arguments to the
// appropriate per-command parser, returning a typed Command node. The
// routing logic matches command and subcommand keywords, then
// delegates the remaining arguments to the specific parser. Returns
// nil with no error when args is empty.
func parseCommand(args []shell.Arg) (Command, error) {
	if len(args) == 0 {
		return nil, nil
	}

	cmd := argText(args[0])
	arg := func(n int) string {
		if n < len(args) {
			return argText(args[n])
		}
		return ""
	}

	switch {
	// program commands
	case len(args) >= 2 && (cmd == "program" || cmd == "programs") && arg(1) == "list":
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

	// diagnostics
	case cmd == "gc":
		return parseGC(args[1:])
	case cmd == "doctor":
		return parseDoctor(args[1:])

	default:
		return nil, fmt.Errorf("unknown command %q. Type \"help\" for available commands.", strings.Join(argTexts(args), " "))
	}
}

// execCommand executes a typed Command node, returning an optional
// result value for variable binding. Commands that do not produce
// assignable results return an empty value.
func execCommand(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd Command) (shell.Value, error) {
	switch c := cmd.(type) {
	case *ShowProgramCommand:
		return shell.Value{}, execShowProgram(ctx, cli, mgr, c)
	case *LoadFileCommand:
		return execLoadFile(ctx, cli, mgr, c)
	case *LoadImageCommand:
		return execLoadImage(ctx, cli, mgr, c)
	case *GetProgramCommand:
		return execGetProgram(ctx, cli, mgr, c)
	case *GetLinkCommand:
		return execGetLink(ctx, cli, mgr, c)
	case *UnloadProgramCommand:
		return shell.Value{}, execUnloadProgram(ctx, cli, mgr, c)
	case *DeleteProgramCommand:
		return shell.Value{}, execDeleteProgram(ctx, cli, mgr, c)
	case *ListProgramsCommand:
		return execListPrograms(ctx, cli, mgr, c)
	case *LinkAttachCommand:
		return execLinkAttach(ctx, cli, mgr, c)
	case *LinkDetachCommand:
		return shell.Value{}, execLinkDetach(ctx, cli, mgr, c)
	case *ListLinksCommand:
		return execListLinks(ctx, cli, mgr, c)
	case *DeleteLinkCommand:
		return shell.Value{}, execDeleteLink(ctx, cli, mgr, c)
	case *DispatcherListCommand:
		return shell.Value{}, execDispatcherList(ctx, cli, mgr, c)
	case *DispatcherGetCommand:
		return shell.Value{}, execDispatcherGet(ctx, cli, mgr, c)
	case *DispatcherDeleteCommand:
		return shell.Value{}, execDispatcherDelete(ctx, cli, mgr, c)
	case *GCCommand:
		return shell.Value{}, execGC(ctx, cli, mgr, c)
	case *DoctorCommand:
		return shell.Value{}, execDoctor(ctx, cli, mgr, c)
	default:
		return shell.Value{}, fmt.Errorf("unhandled command type %T", cmd)
	}
}

// parseProgramIDArg resolves a single shell.Arg directly to a
// kernel.ProgramID, combining argument extraction and ID parsing
// into one step. For text-bearing args the text is parsed as a
// program ID. For StructuredValueArg with an origin, the
// HasProgramID capability interface is used to extract the ID
// directly. For origin-less structured values, path lookup
// (.record.program_id) is the fallback.
func parseProgramIDArg(a shell.Arg) (kernel.ProgramID, error) {
	switch v := a.(type) {
	case shell.WordArg:
		return parseProgramIDText(v.Text)
	case shell.QuotedArg:
		return parseProgramIDText(v.Text)
	case shell.ScalarValueArg:
		return parseProgramIDText(v.Text)
	case shell.StructuredValueArg:
		display := displayName(v.Name)
		if err := shell.ExpectOrigin(v.Value, display, shell.OriginProgram); err != nil {
			return 0, err
		}
		if origin := v.Value.Origin(); origin != nil {
			if x, ok := origin.(bpfman.HasKernelProgramID); ok {
				return x.KernelProgramID(), nil
			}
		}
		// Fallback for origin-less structured values (OriginUnknown).
		resolved, err := v.Value.LookupValue(v.Name, "record.program_id")
		if err != nil {
			return 0, fmt.Errorf("%s is structured but has no .record.program_id field", display)
		}
		s, err := resolved.Scalar()
		if err != nil {
			return 0, err
		}
		return parseProgramIDText(s)
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
	parsed, err := ParseProgramID(s)
	if err != nil {
		return 0, err
	}
	return parsed.Value, nil
}

// parseLinkIDArg resolves a single shell.Arg directly to a
// kernel.LinkID, combining argument extraction and ID parsing into
// one step. For text-bearing args the text is parsed as a link ID.
// For StructuredValueArg with an origin, the HasLinkID capability
// interface is used to extract the ID directly. For origin-less
// structured values, path lookup (.record.id) is the fallback.
func parseLinkIDArg(a shell.Arg) (kernel.LinkID, error) {
	switch v := a.(type) {
	case shell.WordArg:
		return parseLinkIDText(v.Text)
	case shell.QuotedArg:
		return parseLinkIDText(v.Text)
	case shell.ScalarValueArg:
		return parseLinkIDText(v.Text)
	case shell.StructuredValueArg:
		display := displayName(v.Name)
		if err := shell.ExpectOrigin(v.Value, display, shell.OriginLink); err != nil {
			return 0, err
		}
		if origin := v.Value.Origin(); origin != nil {
			if x, ok := origin.(bpfman.HasKernelLinkID); ok {
				return x.KernelLinkID(), nil
			}
		}
		// Fallback for origin-less structured values (OriginUnknown).
		resolved, err := v.Value.LookupValue(v.Name, "record.id")
		if err != nil {
			return 0, fmt.Errorf("%s is structured but has no .record.id field", display)
		}
		s, err := resolved.Scalar()
		if err != nil {
			return 0, err
		}
		return parseLinkIDText(s)
	default:
		return 0, fmt.Errorf("unexpected argument type %T", a)
	}
}

// parseLinkIDText parses a link ID from text into a kernel.LinkID.
func parseLinkIDText(s string) (kernel.LinkID, error) {
	parsed, err := ParseLinkID(s)
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
	Output OutputFlags
}

func (*ShowProgramCommand) isCommand() {}

// validShowViews lists the accepted sub-view names for "show program".
var validShowViews = map[string]bool{
	"summary": true,
	"links":   true,
	"maps":    true,
	"paths":   true,
}

// parseShowProgram resolves expanded REPL arguments into a
// ShowProgramCommand. The grammar is:
//
//	<program-id> [view] [-o format]
//
// One required positional (program ID), one optional positional (view
// name defaulting to "summary"), and one optional flag (-o).
func parseShowProgram(args []shell.Arg) (*ShowProgramCommand, error) {
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
		Output: OutputFlags{
			Output: OutputValue{Value: "table"},
		},
	}

	// Walk the remaining arguments: optional view positional, optional -o flag.
	rest := args[1:]
	viewSet := false
	for i := 0; i < len(rest); i++ {
		text := argText(rest[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("show program: duplicate -o flag")
			}
			i++
			if i >= len(rest) {
				return nil, fmt.Errorf("show program: -o requires a value")
			}
			cmd.Output.Output = OutputValue{
				Value: argText(rest[i]),
				IsSet: true,
			}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, fmt.Errorf("show program: unknown flag %q", text)
		}
		if viewSet {
			return nil, fmt.Errorf("show program: only one view may be specified")
		}
		// Treat as view name.
		if !validShowViews[text] {
			return nil, fmt.Errorf("show program: unknown view %q (valid: summary, links, maps, paths)", text)
		}
		cmd.View = text
		viewSet = true
	}

	return cmd, nil
}

// execShowProgram executes a parsed ShowProgramCommand, fetching the
// program from the store and rendering output according to the
// requested view and format.
func execShowProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *ShowProgramCommand) error {
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
	if format == OutputFormatJSON {
		output, err := formatShowJSON(prog)
		if err != nil {
			return err
		}
		return cli.PrintOut(output)
	}

	var output string
	switch cmd.View {
	case "summary":
		var fmtErr error
		output, fmtErr = FormatProgram(prog, &cmd.Output)
		if fmtErr != nil {
			return fmtErr
		}
	case "links":
		output = formatShowLinks(prog)
	case "maps":
		output = formatShowMaps(prog)
	case "paths":
		output = formatShowPaths(prog)
	default:
		return fmt.Errorf("unknown view %q (valid: summary, links, maps, paths)", cmd.View)
	}

	return cli.PrintOut(output)
}

// LoadFileCommand represents a fully parsed "load file" command.
type LoadFileCommand struct {
	Path        string
	Programs    []ProgramSpec
	Metadata    []KeyValue
	GlobalData  []GlobalData
	Application string
	MapOwnerID  kernel.ProgramID
	Output      OutputFlags
}

func (*LoadFileCommand) isCommand() {}

// parseLoadFile resolves expanded REPL arguments into a
// LoadFileCommand. The grammar is:
//
//	-p <path> [--programs <spec>]... [-m <key=val>]... [-g <name=hex>]...
//	[-a <app>] [--map-owner-id <id>] [-o <format>]
func parseLoadFile(args []shell.Arg) (*LoadFileCommand, error) {
	cmd := &LoadFileCommand{
		Output: OutputFlags{Output: OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-p", "--path":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load file: %s requires a value", text)
			}
			cmd.Path = argText(args[i])
		case "--programs":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load file: --programs requires a value")
			}
			spec, err := ParseProgramSpec(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load file: %w", err)
			}
			cmd.Programs = append(cmd.Programs, spec)
		case "-m", "--metadata":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load file: %s requires a value", text)
			}
			kv, err := ParseKeyValue(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load file: %w", err)
			}
			cmd.Metadata = append(cmd.Metadata, kv)
		case "-g", "--global":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load file: %s requires a value", text)
			}
			gd, err := ParseGlobalData(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load file: %w", err)
			}
			cmd.GlobalData = append(cmd.GlobalData, gd)
		case "-a", "--application":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load file: %s requires a value", text)
			}
			cmd.Application = argText(args[i])
		case "--map-owner-id":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load file: --map-owner-id requires a value")
			}
			parsed, err := ParseProgramID(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load file: %w", err)
			}
			cmd.MapOwnerID = parsed.Value
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("load file: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load file: -o requires a value")
			}
			cmd.Output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("load file: unknown flag %q", text)
			}
			return nil, fmt.Errorf("load file: unexpected argument %q", text)
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
func execLoadFile(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *LoadFileCommand) (shell.Value, error) {
	objPath, err := ParseObjectPath(cmd.Path)
	if err != nil {
		return shell.Value{}, err
	}

	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (loadFileResult, error) {
		var globalData map[string][]byte
		if len(cmd.GlobalData) > 0 {
			globalData = GlobalDataMap(cmd.GlobalData)
		}

		metadata := MetadataMap(cmd.Metadata)
		if cmd.Application != "" {
			if metadata == nil {
				metadata = make(map[string]string)
			}
			metadata["bpfman.io/application"] = cmd.Application
		}

		var programs []manager.ProgramSpec
		for _, prog := range cmd.Programs {
			programs = append(programs, manager.ProgramSpec{
				Name:       prog.Name,
				Type:       prog.Type,
				AttachFunc: prog.AttachFunc,
				MapOwnerID: cmd.MapOwnerID,
			})
		}

		loaded, loadErr := mgr.Load(ctx, writeLock, manager.LoadSource{
			FilePath: objPath.Path,
		}, programs, manager.LoadOpts{
			UserMetadata: metadata,
			GlobalData:   globalData,
		})
		if loadErr != nil {
			return loadFileResult{}, fmt.Errorf("failed to load programs: %w", loadErr)
		}
		return loadFileResult{Programs: loaded}, nil
	})
	if err != nil {
		return shell.Value{}, err
	}

	output, err := FormatLoadedPrograms(result.Programs, &cmd.Output)
	if err != nil {
		return shell.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return shell.Value{}, err
	}

	if len(result.Programs) == 0 {
		return shell.Value{}, nil
	}

	val, err := shell.ValueFromStruct(result.Programs[0])
	if err != nil {
		return shell.Value{}, nil
	}
	return val.WithKind(shell.OriginProgram), nil
}

// LinkAttachCommand represents a fully parsed "link attach" command.
// The AttachSpec is constructed at parse time from the type-specific
// flags; execution simply runs it under lock.
type LinkAttachCommand struct {
	Spec   bpfman.AttachSpec
	Output OutputFlags
}

func (*LinkAttachCommand) isCommand() {}

// parseLinkAttach parses "link attach <type> <args...>" into a
// LinkAttachCommand. The first argument is the attach type; the
// remaining arguments are type-specific flags and one required
// program ID.
func parseLinkAttach(args []shell.Arg) (*LinkAttachCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("link attach requires a type (xdp, tc, tcx, tracepoint, kprobe, uprobe, fentry, fexit)")
	}

	attachType := argText(args[0])
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
func parseLinkAttachXDP(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		priority  int
		proceedOn []string
		netns     string
		progArg   shell.Arg
		output    = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	defaults := true
	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-i", "--iface":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach xdp: %s requires a value", text)
			}
			iface = argText(args[i])
		case "-p", "--priority":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach xdp: %s requires a value", text)
			}
			v, err := strconv.Atoi(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("link attach xdp: invalid priority %q: %w", argText(args[i]), err)
			}
			priority = v
		case "--proceed-on":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach xdp: --proceed-on requires a value")
			}
			proceedOn = append(proceedOn, splitComma(argText(args[i]))...)
			defaults = false
		case "-n", "--netns":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach xdp: %s requires a value", text)
			}
			netns = argText(args[i])
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach xdp: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach xdp: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach xdp: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach xdp: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, fmt.Errorf("link attach xdp: unexpected argument %q", text)
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

	ifObj, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("link attach xdp: failed to find interface %q: %w", iface, err)
	}

	if defaults {
		proceedOn = []string{"pass", "dispatcher_return"}
	}
	actions, err := bpfman.ParseXDPActions(proceedOn)
	if err != nil {
		return nil, fmt.Errorf("link attach xdp: invalid proceed-on value: %w", err)
	}

	spec, err := bpfman.NewXDPAttachSpec(progID, iface, ifObj.Index)
	if err != nil {
		return nil, fmt.Errorf("link attach xdp: %w", err)
	}
	spec = spec.WithPriority(priority).WithProceedOn(actions)
	if netns != "" {
		spec = spec.WithNetns(netns)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachTC parses "link attach tc" arguments.
//
//	-i <iface> -d <direction> [-p <priority>] [--proceed-on <actions>]...
//	[-n <netns>] [-o <format>] <program-id>
func parseLinkAttachTC(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		direction string
		priority  int
		proceedOn []string
		netns     string
		progArg   shell.Arg
		output    = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	defaults := true
	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-i", "--iface":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tc: %s requires a value", text)
			}
			iface = argText(args[i])
		case "-d", "--direction":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tc: %s requires a value", text)
			}
			direction = argText(args[i])
		case "-p", "--priority":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tc: %s requires a value", text)
			}
			v, err := strconv.Atoi(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("link attach tc: invalid priority %q: %w", argText(args[i]), err)
			}
			priority = v
		case "--proceed-on":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tc: --proceed-on requires a value")
			}
			proceedOn = append(proceedOn, splitComma(argText(args[i]))...)
			defaults = false
		case "-n", "--netns":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tc: %s requires a value", text)
			}
			netns = argText(args[i])
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach tc: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach tc: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tc: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach tc: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, fmt.Errorf("link attach tc: unexpected argument %q", text)
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
	if priority < 0 || priority > 1000 {
		return nil, fmt.Errorf("link attach tc: --priority must be 0-1000, got %d", priority)
	}

	dir, err := bpfman.ParseTCDirection(direction)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: %w", err)
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: %w", err)
	}

	ifObj, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: failed to find interface %q: %w", iface, err)
	}

	if defaults {
		proceedOn = []string{"pipe", "dispatcher_return"}
	}
	actions, err := bpfman.ParseTCActions(proceedOn)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: invalid proceed-on value: %w", err)
	}

	spec, err := bpfman.NewTCAttachSpec(progID, iface, ifObj.Index, dir)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: %w", err)
	}
	spec = spec.WithPriority(priority).WithProceedOn(actions)
	if netns != "" {
		spec = spec.WithNetns(netns)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachTCX parses "link attach tcx" arguments.
//
//	-i <iface> -d <direction> [-p <priority>] [-n <netns>]
//	[-o <format>] <program-id>
func parseLinkAttachTCX(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		direction string
		priority  int
		netns     string
		progArg   shell.Arg
		output    = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-i", "--iface":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tcx: %s requires a value", text)
			}
			iface = argText(args[i])
		case "-d", "--direction":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tcx: %s requires a value", text)
			}
			direction = argText(args[i])
		case "-p", "--priority":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tcx: %s requires a value", text)
			}
			v, err := strconv.Atoi(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("link attach tcx: invalid priority %q: %w", argText(args[i]), err)
			}
			priority = v
		case "-n", "--netns":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tcx: %s requires a value", text)
			}
			netns = argText(args[i])
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach tcx: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach tcx: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tcx: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach tcx: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, fmt.Errorf("link attach tcx: unexpected argument %q", text)
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
	if priority < 0 || priority > 1000 {
		return nil, fmt.Errorf("link attach tcx: --priority must be 0-1000, got %d", priority)
	}

	dir, err := bpfman.ParseTCDirection(direction)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: %w", err)
	}

	progID, err := parseProgramIDArg(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: %w", err)
	}

	ifObj, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: failed to find interface %q: %w", iface, err)
	}

	spec, err := bpfman.NewTCXAttachSpec(progID, iface, ifObj.Index, dir)
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
func parseLinkAttachTracepoint(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		progArg    shell.Arg
		tracepoint string
		output     = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach tracepoint: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach tracepoint: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tracepoint: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach tracepoint: unknown flag %q", text)
			}
			switch {
			case progArg == nil:
				progArg = args[i]
			case tracepoint == "":
				tracepoint = text
			default:
				return nil, fmt.Errorf("link attach tracepoint: unexpected argument %q", text)
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

	tp, err := ParseTracepointName(tracepoint)
	if err != nil {
		return nil, fmt.Errorf("link attach tracepoint: %w", err)
	}

	spec, err := bpfman.NewTracepointAttachSpec(progID, tp.Group, tp.Name)
	if err != nil {
		return nil, fmt.Errorf("link attach tracepoint: %w", err)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachKprobe parses "link attach kprobe" arguments.
//
//	-f <fn-name> [--offset <n>] [-o <format>] <program-id>
func parseLinkAttachKprobe(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		fnName  string
		offset  uint64
		progArg shell.Arg
		output  = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-f", "--fn-name":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach kprobe: %s requires a value", text)
			}
			fnName = argText(args[i])
		case "--offset":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach kprobe: --offset requires a value")
			}
			v, err := strconv.ParseUint(argText(args[i]), 0, 64)
			if err != nil {
				return nil, fmt.Errorf("link attach kprobe: invalid offset %q: %w", argText(args[i]), err)
			}
			offset = v
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach kprobe: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach kprobe: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach kprobe: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach kprobe: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, fmt.Errorf("link attach kprobe: unexpected argument %q", text)
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
func parseLinkAttachUprobe(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		target       string
		fnName       string
		offset       uint64
		containerPid int32
		progArg      shell.Arg
		output       = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "--target":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach uprobe: --target requires a value")
			}
			target = argText(args[i])
		case "-f", "--fn-name":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach uprobe: %s requires a value", text)
			}
			fnName = argText(args[i])
		case "--offset":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach uprobe: --offset requires a value")
			}
			v, err := strconv.ParseUint(argText(args[i]), 0, 64)
			if err != nil {
				return nil, fmt.Errorf("link attach uprobe: invalid offset %q: %w", argText(args[i]), err)
			}
			offset = v
		case "--container-pid":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach uprobe: --container-pid requires a value")
			}
			v, err := strconv.ParseInt(argText(args[i]), 10, 32)
			if err != nil {
				return nil, fmt.Errorf("link attach uprobe: invalid container-pid %q: %w", argText(args[i]), err)
			}
			containerPid = int32(v)
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach uprobe: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach uprobe: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach uprobe: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach uprobe: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, fmt.Errorf("link attach uprobe: unexpected argument %q", text)
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
func parseLinkAttachFentry(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		progArg shell.Arg
		output  = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach fentry: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach fentry: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach fentry: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach fentry: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, fmt.Errorf("link attach fentry: unexpected argument %q", text)
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
func parseLinkAttachFexit(args []shell.Arg) (*LinkAttachCommand, error) {
	var (
		progArg shell.Arg
		output  = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-m", "--metadata":
			return nil, fmt.Errorf("link attach fexit: metadata is not supported for attach commands")
		case "-o":
			if output.Output.IsSet {
				return nil, fmt.Errorf("link attach fexit: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach fexit: -o requires a value")
			}
			output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link attach fexit: unknown flag %q", text)
			}
			if progArg != nil {
				return nil, fmt.Errorf("link attach fexit: unexpected argument %q", text)
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
func execLinkAttach(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *LinkAttachCommand) (shell.Value, error) {
	link, err := RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (bpfman.Link, error) {
		return mgr.Attach(ctx, writeLock, cmd.Spec)
	})
	if err != nil {
		return shell.Value{}, err
	}

	output, err := FormatLinkResult(link, &cmd.Output)
	if err != nil {
		return shell.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return shell.Value{}, err
	}

	val, err := shell.ValueFromStruct(link)
	if err != nil {
		return shell.Value{}, nil
	}
	return val.WithKind(shell.OriginLink), nil
}

// LinkDetachCommand represents a fully parsed "link detach" command
// with resolved link IDs ready for execution.
type LinkDetachCommand struct {
	LinkIDs []kernel.LinkID
}

func (*LinkDetachCommand) isCommand() {}

// parseLinkDetach resolves expanded REPL arguments into a
// LinkDetachCommand. Each positional argument is a link ID, possibly
// from a structured variable reference.
func parseLinkDetach(args []shell.Arg) (*LinkDetachCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("link detach: requires at least one link ID")
	}

	var ids []kernel.LinkID
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
func execLinkDetach(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *LinkDetachCommand) error {
	return runBatchMutation(ctx, cli, cmd.LinkIDs, "link", "detach",
		func(ctx context.Context, writeLock lock.WriterScope, id kernel.LinkID) error {
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
	Programs     []ProgramSpec
	PullPolicy   string
	RegistryAuth string
	Application  string
	Metadata     []KeyValue
	GlobalData   []GlobalData
	MapOwnerID   kernel.ProgramID
	Output       OutputFlags
}

func (*LoadImageCommand) isCommand() {}

// parseLoadImage resolves expanded REPL arguments into a
// LoadImageCommand. The grammar is:
//
//	-i <url> [--programs <spec>]... [-p <policy>] [--registry-auth <auth>]
//	[-a <app>] [--map-owner-id <id>] [-m <key=val>]... [-g <name=hex>]...
//	[-o <format>]
func parseLoadImage(args []shell.Arg) (*LoadImageCommand, error) {
	cmd := &LoadImageCommand{
		PullPolicy: "IfNotPresent",
		Output:     OutputFlags{Output: OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-i", "--image-url":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: %s requires a value", text)
			}
			cmd.ImageURL = argText(args[i])
		case "--programs":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: --programs requires a value")
			}
			spec, err := ParseProgramSpec(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			cmd.Programs = append(cmd.Programs, spec)
		case "-p", "--pull-policy":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: %s requires a value", text)
			}
			cmd.PullPolicy = argText(args[i])
		case "--registry-auth":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: --registry-auth requires a value")
			}
			cmd.RegistryAuth = argText(args[i])
		case "-a", "--application":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: %s requires a value", text)
			}
			cmd.Application = argText(args[i])
		case "--map-owner-id":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: --map-owner-id requires a value")
			}
			parsed, err := ParseProgramID(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			cmd.MapOwnerID = parsed.Value
		case "-m", "--metadata":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: %s requires a value", text)
			}
			kv, err := ParseKeyValue(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			cmd.Metadata = append(cmd.Metadata, kv)
		case "-g", "--global":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: %s requires a value", text)
			}
			gd, err := ParseGlobalData(argText(args[i]))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			cmd.GlobalData = append(cmd.GlobalData, gd)
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("load image: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("load image: -o requires a value")
			}
			cmd.Output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("load image: unknown flag %q", text)
			}
			return nil, fmt.Errorf("load image: unexpected argument %q", text)
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
func execLoadImage(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *LoadImageCommand) (shell.Value, error) {
	pullPolicy, err := bpfman.ParseImagePullPolicy(cmd.PullPolicy)
	if err != nil {
		return shell.Value{}, fmt.Errorf("load image: invalid pull policy %q: %w", cmd.PullPolicy, err)
	}

	type loadImageResult struct {
		Programs []bpfman.Program
	}

	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (loadImageResult, error) {
		var globalData map[string][]byte
		if len(cmd.GlobalData) > 0 {
			globalData = GlobalDataMap(cmd.GlobalData)
		}

		metadata := MetadataMap(cmd.Metadata)
		if cmd.Application != "" {
			if metadata == nil {
				metadata = make(map[string]string)
			}
			metadata["bpfman.io/application"] = cmd.Application
		}

		ref := platform.ImageRef{
			URL:        cmd.ImageURL,
			PullPolicy: pullPolicy,
		}

		if cmd.RegistryAuth != "" {
			decoded, decErr := base64.StdEncoding.DecodeString(cmd.RegistryAuth)
			if decErr != nil {
				return loadImageResult{}, fmt.Errorf("invalid registry-auth: invalid base64 encoding: %w", decErr)
			}
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) != 2 {
				return loadImageResult{}, fmt.Errorf("invalid registry-auth: expected 'username:password' format")
			}
			ref.Auth = &platform.ImageAuth{
				Username: parts[0],
				Password: parts[1],
			}
		}

		var programs []manager.ProgramSpec
		for _, prog := range cmd.Programs {
			programs = append(programs, manager.ProgramSpec{
				Name:       prog.Name,
				Type:       prog.Type,
				AttachFunc: prog.AttachFunc,
				MapOwnerID: cmd.MapOwnerID,
			})
		}

		loaded, loadErr := mgr.Load(ctx, writeLock, manager.LoadSource{
			Image: &ref,
		}, programs, manager.LoadOpts{
			UserMetadata: metadata,
			GlobalData:   globalData,
		})
		if loadErr != nil {
			return loadImageResult{}, fmt.Errorf("failed to load from image: %w", loadErr)
		}
		return loadImageResult{Programs: loaded}, nil
	})
	if err != nil {
		return shell.Value{}, err
	}

	output, err := FormatLoadedPrograms(result.Programs, &cmd.Output)
	if err != nil {
		return shell.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return shell.Value{}, err
	}

	if len(result.Programs) == 0 {
		return shell.Value{}, nil
	}

	val, err := shell.ValueFromStruct(result.Programs[0])
	if err != nil {
		return shell.Value{}, nil
	}
	return val.WithKind(shell.OriginProgram), nil
}

// GetProgramCommand represents a fully parsed "program get" command
// with a resolved program ID and output format.
type GetProgramCommand struct {
	ID     kernel.ProgramID
	Output OutputFlags
}

func (*GetProgramCommand) isCommand() {}

// parseGetProgram resolves expanded REPL arguments into a
// GetProgramCommand. The grammar is:
//
//	<program-id> [-o format]
func parseGetProgram(args []shell.Arg) (*GetProgramCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("program get: requires a program ID")
	}

	id, err := parseProgramIDArg(args[0])
	if err != nil {
		return nil, fmt.Errorf("program get: %w", err)
	}

	cmd := &GetProgramCommand{
		ID: id,
		Output: OutputFlags{
			Output: OutputValue{Value: "table"},
		},
	}

	for i := 1; i < len(args); i++ {
		text := argText(args[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("program get: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("program get: -o requires a value")
			}
			cmd.Output.Output = OutputValue{
				Value: argText(args[i]),
				IsSet: true,
			}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, fmt.Errorf("program get: unknown flag %q", text)
		}
		return nil, fmt.Errorf("program get: unexpected argument %q", text)
	}

	return cmd, nil
}

// execGetProgram executes a parsed GetProgramCommand, fetching the
// program from the store, rendering output, and returning a
// structured Value for variable assignment.
func execGetProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *GetProgramCommand) (shell.Value, error) {
	prog, err := mgr.Get(ctx, cmd.ID)
	if err != nil {
		return shell.Value{}, err
	}

	output, err := FormatProgram(prog, &cmd.Output)
	if err != nil {
		return shell.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return shell.Value{}, err
	}

	val, err := shell.ValueFromStruct(prog)
	if err != nil {
		return shell.Value{}, nil
	}
	return val.WithKind(shell.OriginProgram), nil
}

// GetLinkCommand represents a fully parsed "link get" command with a
// resolved link ID and output format.
type GetLinkCommand struct {
	ID     kernel.LinkID
	Output OutputFlags
}

func (*GetLinkCommand) isCommand() {}

// parseGetLink resolves expanded REPL arguments into a
// GetLinkCommand. The grammar is:
//
//	<link-id> [-o format]
func parseGetLink(args []shell.Arg) (*GetLinkCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("link get: requires a link ID")
	}

	id, err := parseLinkIDArg(args[0])
	if err != nil {
		return nil, fmt.Errorf("link get: %w", err)
	}

	cmd := &GetLinkCommand{
		ID: id,
		Output: OutputFlags{
			Output: OutputValue{Value: "table"},
		},
	}

	for i := 1; i < len(args); i++ {
		text := argText(args[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("link get: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link get: -o requires a value")
			}
			cmd.Output.Output = OutputValue{
				Value: argText(args[i]),
				IsSet: true,
			}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, fmt.Errorf("link get: unknown flag %q", text)
		}
		return nil, fmt.Errorf("link get: unexpected argument %q", text)
	}

	return cmd, nil
}

// execGetLink executes a parsed GetLinkCommand, fetching the link
// from the store, rendering output, and returning a structured Value
// for variable assignment.
func execGetLink(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *GetLinkCommand) (shell.Value, error) {
	info, err := mgr.GetLinkInfo(ctx, cmd.ID)
	if err != nil {
		return shell.Value{}, err
	}

	link := bpfman.Link{
		Record: info.Record,
		Status: bpfman.LinkStatus{
			Kernel:     info.Kernel,
			KernelSeen: info.Presence.InKernel,
			PinPresent: info.Presence.InFS,
		},
	}

	output, fmtErr := FormatLinkResult(link, &cmd.Output)
	if fmtErr != nil {
		return shell.Value{}, fmtErr
	}
	if err := cli.PrintOut(output); err != nil {
		return shell.Value{}, err
	}

	val, err := shell.ValueFromStruct(link)
	if err != nil {
		return shell.Value{}, nil
	}
	return val.WithKind(shell.OriginLink), nil
}

// UnloadProgramCommand represents a fully parsed "program unload"
// command with resolved program IDs.
type UnloadProgramCommand struct {
	ProgramIDs []kernel.ProgramID
}

func (*UnloadProgramCommand) isCommand() {}

// parseUnloadProgram resolves expanded REPL arguments into an
// UnloadProgramCommand. Each positional argument is a program ID.
func parseUnloadProgram(args []shell.Arg) (*UnloadProgramCommand, error) {
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
func execUnloadProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *UnloadProgramCommand) error {
	return runBatchMutation(ctx, cli, cmd.ProgramIDs, "program", "unload",
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

// parseDeleteProgram resolves expanded REPL arguments into a
// DeleteProgramCommand. The grammar is:
//
//	(<program-id>... | --all) [-r]
func parseDeleteProgram(args []shell.Arg) (*DeleteProgramCommand, error) {
	cmd := &DeleteProgramCommand{}

	var positionals []shell.Arg
	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "--all":
			cmd.All = true
		case "-r", "--recursive":
			cmd.Recursive = true
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("program delete: unknown flag %q", text)
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
func execDeleteProgram(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *DeleteProgramCommand) error {
	ids, err := collectDeleteIDs(ctx, mgr, cmd.All, programIDsToProgramIDs(cmd.ProgramIDs))
	if err != nil {
		return err
	}
	return executeDeletePrograms(ctx, cli, mgr, ids, cmd.Recursive)
}

// programIDsToProgramIDs converts a slice of kernel.ProgramID to the
// ProgramID wrapper type used by collectDeleteIDs.
func programIDsToProgramIDs(ids []kernel.ProgramID) []ProgramID {
	result := make([]ProgramID, len(ids))
	for i, id := range ids {
		result[i] = ProgramID{Value: id}
	}
	return result
}

// DeleteLinkCommand represents a fully parsed "link delete" command
// with resolved link IDs and flags.
type DeleteLinkCommand struct {
	LinkIDs   []kernel.LinkID
	Recursive bool
}

func (*DeleteLinkCommand) isCommand() {}

// parseDeleteLink resolves expanded REPL arguments into a
// DeleteLinkCommand. The grammar is:
//
//	<link-id>... [-r]
func parseDeleteLink(args []shell.Arg) (*DeleteLinkCommand, error) {
	cmd := &DeleteLinkCommand{}

	var positionals []shell.Arg
	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-r", "--recursive":
			cmd.Recursive = true
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link delete: unknown flag %q", text)
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
func execDeleteLink(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *DeleteLinkCommand) error {
	type result struct {
		id  kernel.LinkID
		err error
	}
	results := make([]result, 0, len(cmd.LinkIDs))

	lockErr := RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		for _, id := range cmd.LinkIDs {
			err := deleteLink(ctx, writeLock, mgr, id, cmd.Recursive)
			results = append(results, result{id: id, err: err})
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
	Types      []string
	Selector   string
	Output     OutputFlags
}

func (*ListProgramsCommand) isCommand() {}

// parseListPrograms resolves expanded REPL arguments into a
// ListProgramsCommand. The grammar is:
//
//	[-q] [--attached|--unattached] [--type <types>]...
//	[-l <selector>] [-o <format>]
func parseListPrograms(args []shell.Arg) (*ListProgramsCommand, error) {
	cmd := &ListProgramsCommand{
		Output: OutputFlags{Output: OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
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
				return nil, fmt.Errorf("program list: --type requires a value")
			}
			cmd.Types = append(cmd.Types, splitComma(argText(args[i]))...)
		case "-l", "--selector":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("program list: %s requires a value", text)
			}
			cmd.Selector = argText(args[i])
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("program list: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("program list: -o requires a value")
			}
			cmd.Output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("program list: unknown flag %q", text)
			}
			return nil, fmt.Errorf("program list: unexpected argument %q", text)
		}
	}

	if cmd.Attached && cmd.Unattached {
		return nil, fmt.Errorf("program list: --attached and --unattached are mutually exclusive")
	}

	return cmd, nil
}

// execListPrograms executes a parsed ListProgramsCommand, listing
// programs from the store, rendering output, and returning the
// result as a bindable value so REPL scripts can inspect the list
// structurally.
func execListPrograms(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *ListProgramsCommand) (shell.Value, error) {
	var opts []bpfman.ListOption

	if cmd.Attached {
		opts = append(opts, bpfman.WithAttached())
	} else if cmd.Unattached {
		opts = append(opts, bpfman.WithUnattached())
	}

	if len(cmd.Types) > 0 {
		types, err := ParseProgramTypesSlice(cmd.Types)
		if err != nil {
			return shell.Value{}, err
		}
		opts = append(opts, bpfman.WithTypes(types...))
	}

	if s := strings.TrimSpace(cmd.Selector); s != "" {
		sel, err := labels.Parse(s)
		if err != nil {
			return shell.Value{}, fmt.Errorf("invalid label selector: %w", err)
		}
		opts = append(opts, bpfman.MatchingSelector(sel))
	}

	result, err := mgr.ListPrograms(ctx, opts...)
	if err != nil {
		return shell.Value{}, err
	}

	if len(result.Programs) > 0 || cmd.Output.IsStructured() {
		if cmd.Quiet {
			var b strings.Builder
			for _, p := range result.Programs {
				fmt.Fprintf(&b, "program/%d\n", p.Record.ProgramID)
			}
			if err := cli.PrintOut(b.String()); err != nil {
				return shell.Value{}, err
			}
		} else {
			output, err := FormatProgramsComposite(result, &cmd.Output)
			if err != nil {
				return shell.Value{}, err
			}
			if err := cli.PrintOut(output); err != nil {
				return shell.Value{}, err
			}
		}
	}

	val, err := shell.ValueFromStruct(result)
	if err != nil {
		return shell.Value{}, nil
	}
	return val, nil
}

// ListLinksCommand represents a fully parsed "link list" command with
// filter flags and output format.
type ListLinksCommand struct {
	Quiet     bool
	ProgramID *kernel.ProgramID
	Kinds     []string
	Output    OutputFlags
}

func (*ListLinksCommand) isCommand() {}

// parseListLinks resolves expanded REPL arguments into a
// ListLinksCommand. The grammar is:
//
//	[-q] [--program-id <id>] [--kind <kinds>]... [-o <format>]
func parseListLinks(args []shell.Arg) (*ListLinksCommand, error) {
	cmd := &ListLinksCommand{
		Output: OutputFlags{Output: OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-q", "--quiet":
			cmd.Quiet = true
		case "--program-id":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link list: --program-id requires a value")
			}
			id, err := parseProgramIDArg(args[i])
			if err != nil {
				return nil, fmt.Errorf("link list: %w", err)
			}
			cmd.ProgramID = &id
		case "--kind":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link list: --kind requires a value")
			}
			cmd.Kinds = append(cmd.Kinds, splitComma(argText(args[i]))...)
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("link list: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link list: -o requires a value")
			}
			cmd.Output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("link list: unknown flag %q", text)
			}
			return nil, fmt.Errorf("link list: unexpected argument %q", text)
		}
	}

	return cmd, nil
}

// execListLinks executes a parsed ListLinksCommand, listing links
// from the store and rendering output.
func execListLinks(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *ListLinksCommand) (shell.Value, error) {
	var opts []bpfman.LinkListOption

	if cmd.ProgramID != nil {
		opts = append(opts, bpfman.WithProgramID(*cmd.ProgramID))
	}

	if len(cmd.Kinds) > 0 {
		kinds, err := ParseLinkKindsSlice(cmd.Kinds)
		if err != nil {
			return shell.Value{}, err
		}
		opts = append(opts, bpfman.WithKinds(kinds...))
	}

	links, err := mgr.ListLinks(ctx, opts...)
	if err != nil {
		return shell.Value{}, err
	}

	if len(links) > 0 || cmd.Output.IsStructured() {
		if cmd.Quiet {
			var b strings.Builder
			for _, l := range links {
				fmt.Fprintf(&b, "link/%d\n", l.ID)
			}
			if err := cli.PrintOut(b.String()); err != nil {
				return shell.Value{}, err
			}
		} else {
			output, err := FormatLinkList(links, &cmd.Output)
			if err != nil {
				return shell.Value{}, err
			}
			if err := cli.PrintOut(output); err != nil {
				return shell.Value{}, err
			}
		}
	}

	val, err := shell.ValueFromStruct(bpfman.LinkListResult{Links: links})
	if err != nil {
		return shell.Value{}, nil
	}
	return val, nil
}

// DispatcherListCommand represents a fully parsed "dispatcher list"
// command with optional type filter and output format.
type DispatcherListCommand struct {
	Type   string
	Output OutputFlags
}

func (*DispatcherListCommand) isCommand() {}

// parseDispatcherList resolves expanded REPL arguments into a
// DispatcherListCommand. The grammar is:
//
//	[--type <type>] [-o <format>]
func parseDispatcherList(args []shell.Arg) (*DispatcherListCommand, error) {
	cmd := &DispatcherListCommand{
		Output: OutputFlags{Output: OutputValue{Value: "table"}},
	}

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "--type":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("dispatcher list: --type requires a value")
			}
			cmd.Type = argText(args[i])
		case "-o":
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("dispatcher list: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("dispatcher list: -o requires a value")
			}
			cmd.Output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("dispatcher list: unknown flag %q", text)
			}
			return nil, fmt.Errorf("dispatcher list: unexpected argument %q", text)
		}
	}

	return cmd, nil
}

// execDispatcherList executes a parsed DispatcherListCommand, listing
// dispatchers from the store and rendering output.
func execDispatcherList(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *DispatcherListCommand) error {
	summaries, err := mgr.ListDispatcherSummaries(ctx)
	if err != nil {
		return err
	}

	if cmd.Type != "" {
		filterType, err := dispatcher.ParseDispatcherType(cmd.Type)
		if err != nil {
			return err
		}
		filtered := summaries[:0]
		for _, s := range summaries {
			if s.Key.Type == filterType {
				filtered = append(filtered, s)
			}
		}
		summaries = filtered
	}

	if len(summaries) == 0 && !cmd.Output.IsStructured() {
		return nil
	}

	output, err := FormatDispatcherList(summaries, &cmd.Output)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// DispatcherGetCommand represents a fully parsed "dispatcher get"
// command with resolved dispatcher key and output format.
type DispatcherGetCommand struct {
	Key    dispatcher.Key
	Output OutputFlags
}

func (*DispatcherGetCommand) isCommand() {}

// parseDispatcherGet resolves expanded REPL arguments into a
// DispatcherGetCommand. The grammar is:
//
//	<type> <nsid> <ifindex> [-o <format>]
func parseDispatcherGet(args []shell.Arg) (*DispatcherGetCommand, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("dispatcher get: requires <type> <nsid> <ifindex>")
	}

	dispType, err := dispatcher.ParseDispatcherType(argText(args[0]))
	if err != nil {
		return nil, fmt.Errorf("dispatcher get: %w", err)
	}

	nsid, err := strconv.ParseUint(argText(args[1]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("dispatcher get: invalid nsid %q: %w", argText(args[1]), err)
	}

	ifindex, err := strconv.ParseUint(argText(args[2]), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("dispatcher get: invalid ifindex %q: %w", argText(args[2]), err)
	}

	cmd := &DispatcherGetCommand{
		Key: dispatcher.Key{
			Type:    dispType,
			Nsid:    nsid,
			Ifindex: uint32(ifindex),
		},
		Output: OutputFlags{Output: OutputValue{Value: "table"}},
	}

	for i := 3; i < len(args); i++ {
		text := argText(args[i])
		if text == "-o" {
			if cmd.Output.Output.IsSet {
				return nil, fmt.Errorf("dispatcher get: duplicate -o flag")
			}
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("dispatcher get: -o requires a value")
			}
			cmd.Output.Output = OutputValue{Value: argText(args[i]), IsSet: true}
			continue
		}
		if strings.HasPrefix(text, "-") {
			return nil, fmt.Errorf("dispatcher get: unknown flag %q", text)
		}
		return nil, fmt.Errorf("dispatcher get: unexpected argument %q", text)
	}

	return cmd, nil
}

// execDispatcherGet executes a parsed DispatcherGetCommand, fetching
// the dispatcher snapshot and rendering output.
func execDispatcherGet(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *DispatcherGetCommand) error {
	snap, err := mgr.GetDispatcherSnapshot(ctx, cmd.Key)
	if err != nil {
		return err
	}

	output, err := FormatDispatcherSnapshot(snap, &cmd.Output)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// DispatcherDeleteCommand represents a fully parsed "dispatcher
// delete" command with a resolved dispatcher key.
type DispatcherDeleteCommand struct {
	Key dispatcher.Key
}

func (*DispatcherDeleteCommand) isCommand() {}

// parseDispatcherDelete resolves expanded REPL arguments into a
// DispatcherDeleteCommand. The grammar is:
//
//	<type> <nsid> <ifindex>
func parseDispatcherDelete(args []shell.Arg) (*DispatcherDeleteCommand, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("dispatcher delete: requires <type> <nsid> <ifindex>")
	}

	dispType, err := dispatcher.ParseDispatcherType(argText(args[0]))
	if err != nil {
		return nil, fmt.Errorf("dispatcher delete: %w", err)
	}

	nsid, err := strconv.ParseUint(argText(args[1]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("dispatcher delete: invalid nsid %q: %w", argText(args[1]), err)
	}

	ifindex, err := strconv.ParseUint(argText(args[2]), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("dispatcher delete: invalid ifindex %q: %w", argText(args[2]), err)
	}

	if len(args) > 3 {
		return nil, fmt.Errorf("dispatcher delete: unexpected argument %q", argText(args[3]))
	}

	return &DispatcherDeleteCommand{
		Key: dispatcher.Key{
			Type:    dispType,
			Nsid:    nsid,
			Ifindex: uint32(ifindex),
		},
	}, nil
}

// execDispatcherDelete executes a parsed DispatcherDeleteCommand,
// deleting the dispatcher under lock.
func execDispatcherDelete(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *DispatcherDeleteCommand) error {
	return RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		return mgr.DeleteDispatcherSnapshot(ctx, writeLock, cmd.Key)
	})
}

// GCCommand represents a fully parsed "gc" command with flags and
// optional rule names.
type GCCommand struct {
	DryRun bool
	Prune  bool
	Rules  []string
}

func (*GCCommand) isCommand() {}

// parseGC resolves expanded REPL arguments into a GCCommand. The
// grammar is:
//
//	[--dry-run] [--prune] [rule...]
func parseGC(args []shell.Arg) (*GCCommand, error) {
	cmd := &GCCommand{}

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "--dry-run":
			cmd.DryRun = true
		case "--prune":
			cmd.Prune = true
		default:
			if strings.HasPrefix(text, "-") {
				return nil, fmt.Errorf("gc: unknown flag %q", text)
			}
			cmd.Rules = append(cmd.Rules, text)
		}
	}

	if len(cmd.Rules) > 0 {
		gcRuleNames := make(map[string]bool)
		for _, r := range coherency.GCRules() {
			gcRuleNames[r.Name] = true
		}
		for _, name := range cmd.Rules {
			if !gcRuleNames[name] {
				return nil, fmt.Errorf("gc: unknown rule: %s\n\nAvailable GC rules:\n%s",
					name, formatGCRuleNames())
			}
		}
	}

	return cmd, nil
}

// execGC executes a parsed GCCommand, running garbage collection.
func execGC(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *GCCommand) error {
	gcCmd := &GCCmd{
		DryRun: cmd.DryRun,
		Prune:  cmd.Prune,
		Rules:  cmd.Rules,
	}

	gcOpts := manager.GCOptions{
		Rules: cmd.Rules,
		Prune: cmd.Prune,
	}

	if cmd.DryRun {
		return gcCmd.runDryRun(cli, ctx, mgr, gcOpts)
	}
	return gcCmd.runExecute(cli, ctx, mgr, gcOpts)
}

// DoctorCommand represents a fully parsed "doctor" command.
type DoctorCommand struct {
	Subcommand string // "checkup" or "explain"
	RuleName   string // for "explain" subcommand
}

func (*DoctorCommand) isCommand() {}

// parseDoctor resolves expanded REPL arguments into a
// DoctorCommand. The grammar is:
//
//	[checkup]
//	explain [rule]
func parseDoctor(args []shell.Arg) (*DoctorCommand, error) {
	if len(args) == 0 {
		return &DoctorCommand{Subcommand: "checkup"}, nil
	}

	sub := argText(args[0])
	switch sub {
	case "checkup":
		if len(args) > 1 {
			return nil, fmt.Errorf("doctor checkup: unexpected argument %q", argText(args[1]))
		}
		return &DoctorCommand{Subcommand: "checkup"}, nil
	case "explain":
		cmd := &DoctorCommand{Subcommand: "explain"}
		if len(args) > 1 {
			cmd.RuleName = argText(args[1])
		}
		if len(args) > 2 {
			return nil, fmt.Errorf("doctor explain: unexpected argument %q", argText(args[2]))
		}
		return cmd, nil
	default:
		return nil, fmt.Errorf("doctor: unknown subcommand %q (valid: checkup, explain)", sub)
	}
}

// execDoctor executes a parsed DoctorCommand.
func execDoctor(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *DoctorCommand) error {
	switch cmd.Subcommand {
	case "checkup":
		return replDoctorCheckup(ctx, cli, mgr)
	case "explain":
		if cmd.RuleName == "" {
			return replDoctorExplain(cli, nil)
		}
		return replDoctorExplain(cli, []string{cmd.RuleName})
	default:
		return fmt.Errorf("doctor: unknown subcommand %q", cmd.Subcommand)
	}
}
