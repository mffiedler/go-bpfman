package main

import (
	"context"
	"encoding/base64"
	"fmt"
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
