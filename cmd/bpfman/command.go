package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/replang"
)

// Command is the sealed interface for typed command nodes produced by
// parsing expanded REPL arguments. Each concrete variant carries
// fully resolved, validated fields ready for execution.
type Command interface {
	isCommand()
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
func parseShowProgram(args []replang.Arg) (*ShowProgramCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("show program: requires a program ID")
	}

	// Resolve the program ID from the first argument.
	idStr, err := extractProgramID(args[0])
	if err != nil {
		return nil, err
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, err
	}

	cmd := &ShowProgramCommand{
		ID:   parsed.Value,
		View: "summary",
		Output: OutputFlags{
			Output: OutputValue{Value: "table"},
		},
	}

	// Walk the remaining arguments: optional view positional, optional -o flag.
	rest := args[1:]
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
		// Treat as view name.
		if !validShowViews[text] {
			return nil, fmt.Errorf("show program: unknown view %q (valid: summary, links, maps, paths)", text)
		}
		cmd.View = text
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
func parseLoadFile(args []replang.Arg) (*LoadFileCommand, error) {
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
func execLoadFile(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *LoadFileCommand) (replang.Value, error) {
	objPath, err := ParseObjectPath(cmd.Path)
	if err != nil {
		return replang.Value{}, err
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
		return replang.Value{}, err
	}

	output, err := FormatLoadedPrograms(result.Programs, &cmd.Output)
	if err != nil {
		return replang.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return replang.Value{}, err
	}

	if len(result.Programs) == 0 {
		return replang.Value{}, nil
	}

	val, err := replang.ValueFromStruct(result.Programs[0])
	if err != nil {
		return replang.Value{}, nil
	}
	return val, nil
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
func parseLinkAttach(args []replang.Arg) (*LinkAttachCommand, error) {
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
//	[-n <netns>] [-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachXDP(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		priority  int
		proceedOn []string
		netns     string
		progArg   replang.Arg
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
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach xdp: %s requires a value", text)
			}
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

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach xdp: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
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

	spec, err := bpfman.NewXDPAttachSpec(parsed.Value, iface, ifObj.Index)
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
//	[-n <netns>] [-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachTC(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		direction string
		priority  int
		proceedOn []string
		netns     string
		progArg   replang.Arg
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
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tc: %s requires a value", text)
			}
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

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tc: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
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

	spec, err := bpfman.NewTCAttachSpec(parsed.Value, iface, ifObj.Index, dir)
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
//	[-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachTCX(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		iface     string
		direction string
		priority  int
		netns     string
		progArg   replang.Arg
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
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tcx: %s requires a value", text)
			}
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

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: %w", err)
	}

	ifObj, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("link attach tcx: failed to find interface %q: %w", iface, err)
	}

	spec, err := bpfman.NewTCXAttachSpec(parsed.Value, iface, ifObj.Index, dir)
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
//	-t <group/name> [-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachTracepoint(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		tracepoint string
		progArg    replang.Arg
		output     = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-t", "--tracepoint":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tracepoint: %s requires a value", text)
			}
			tracepoint = argText(args[i])
		case "-m", "--metadata":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach tracepoint: %s requires a value", text)
			}
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
			if progArg != nil {
				return nil, fmt.Errorf("link attach tracepoint: unexpected argument %q", text)
			}
			progArg = args[i]
		}
	}

	if tracepoint == "" {
		return nil, fmt.Errorf("link attach tracepoint: --tracepoint is required")
	}
	if progArg == nil {
		return nil, fmt.Errorf("link attach tracepoint: requires a program ID")
	}

	parts := strings.SplitN(tracepoint, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("link attach tracepoint: tracepoint must be in 'group/name' format, got %q", tracepoint)
	}

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach tracepoint: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, fmt.Errorf("link attach tracepoint: %w", err)
	}

	spec, err := bpfman.NewTracepointAttachSpec(parsed.Value, parts[0], parts[1])
	if err != nil {
		return nil, fmt.Errorf("link attach tracepoint: %w", err)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachKprobe parses "link attach kprobe" arguments.
//
//	-f <fn-name> [--offset <n>] [-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachKprobe(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		fnName  string
		offset  uint64
		progArg replang.Arg
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
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach kprobe: %s requires a value", text)
			}
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

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach kprobe: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, fmt.Errorf("link attach kprobe: %w", err)
	}

	spec, err := bpfman.NewKprobeAttachSpec(parsed.Value, fnName)
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
//	[-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachUprobe(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		target       string
		fnName       string
		offset       uint64
		containerPid int32
		progArg      replang.Arg
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
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach uprobe: %s requires a value", text)
			}
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

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach uprobe: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, fmt.Errorf("link attach uprobe: %w", err)
	}

	spec, err := bpfman.NewUprobeAttachSpec(parsed.Value, target)
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
//	[-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachFentry(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		progArg replang.Arg
		output  = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-m", "--metadata":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach fentry: %s requires a value", text)
			}
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

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach fentry: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, fmt.Errorf("link attach fentry: %w", err)
	}

	spec, err := bpfman.NewFentryAttachSpec(parsed.Value)
	if err != nil {
		return nil, fmt.Errorf("link attach fentry: %w", err)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// parseLinkAttachFexit parses "link attach fexit" arguments.
//
//	[-m <key=val>]... [-o <format>] <program-id>
func parseLinkAttachFexit(args []replang.Arg) (*LinkAttachCommand, error) {
	var (
		progArg replang.Arg
		output  = OutputFlags{Output: OutputValue{Value: "table"}}
	)

	for i := 0; i < len(args); i++ {
		text := argText(args[i])
		switch text {
		case "-m", "--metadata":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("link attach fexit: %s requires a value", text)
			}
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

	idStr, err := extractProgramID(progArg)
	if err != nil {
		return nil, fmt.Errorf("link attach fexit: %w", err)
	}
	parsed, err := ParseProgramID(idStr)
	if err != nil {
		return nil, fmt.Errorf("link attach fexit: %w", err)
	}

	spec, err := bpfman.NewFexitAttachSpec(parsed.Value)
	if err != nil {
		return nil, fmt.Errorf("link attach fexit: %w", err)
	}

	return &LinkAttachCommand{Spec: spec, Output: output}, nil
}

// execLinkAttach executes a parsed LinkAttachCommand, attaching the
// BPF program under lock, printing output, and returning a structured
// Value for optional variable assignment.
func execLinkAttach(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *LinkAttachCommand) (replang.Value, error) {
	link, err := RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (bpfman.Link, error) {
		return mgr.Attach(ctx, writeLock, cmd.Spec)
	})
	if err != nil {
		return replang.Value{}, err
	}

	output, err := FormatLinkResult(link, &cmd.Output)
	if err != nil {
		return replang.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return replang.Value{}, err
	}

	val, err := replang.ValueFromStruct(link)
	if err != nil {
		return replang.Value{}, nil
	}
	return val, nil
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
func parseLinkDetach(args []replang.Arg) (*LinkDetachCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("link detach: requires at least one link ID")
	}

	var ids []kernel.LinkID
	for _, a := range args {
		idStr, err := extractLinkID(a)
		if err != nil {
			return nil, fmt.Errorf("link detach: %w", err)
		}
		parsed, err := ParseLinkID(idStr)
		if err != nil {
			return nil, fmt.Errorf("link detach: %w", err)
		}
		ids = append(ids, parsed.Value)
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
func parseLoadImage(args []replang.Arg) (*LoadImageCommand, error) {
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
func execLoadImage(ctx context.Context, cli *CLI, mgr *manager.Manager, cmd *LoadImageCommand) (replang.Value, error) {
	pullPolicy, err := bpfman.ParseImagePullPolicy(cmd.PullPolicy)
	if err != nil {
		return replang.Value{}, fmt.Errorf("load image: invalid pull policy %q: %w", cmd.PullPolicy, err)
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
		return replang.Value{}, err
	}

	output, err := FormatLoadedPrograms(result.Programs, &cmd.Output)
	if err != nil {
		return replang.Value{}, err
	}
	if err := cli.PrintOut(output); err != nil {
		return replang.Value{}, err
	}

	if len(result.Programs) == 0 {
		return replang.Value{}, nil
	}

	val, err := replang.ValueFromStruct(result.Programs[0])
	if err != nil {
		return replang.Value{}, nil
	}
	return val, nil
}
