package bpfmanbuiltin

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	parent "github.com/frobware/go-bpfman/cmd/bpfman-shell/internal/builtins"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
)

var topLevelNouns = map[string]bool{
	"program":    true,
	"show":       true,
	"image":      true,
	"link":       true,
	"dispatcher": true,
	"audit":      true,
}

const HelpDetail = `Subcommands:

  Program management:
    bpfman program list [flags]                     List managed BPF programs
    bpfman program get <id>                         Get program details (assignable)
    bpfman program load file [flags]                Load from a local object file (assignable)
    bpfman program load image [flags]               Load from an OCI image (assignable)
    bpfman program unload <ids>                     Unload programs
    bpfman program delete (<ids> | --all) [-r]      Delete with cascading cleanup
    bpfman show program <id> [view] [-o]            Inspect (views: links, maps, paths)

  Image management:
    bpfman image build <image> <bytecode> [flags]   Build and publish a bytecode image
    bpfman image inspect <image>                    Inspect bytecode image metadata

  Link management:
    bpfman link attach <type> [flags] <id>          Attach a program (assignable)
    bpfman link detach <link-ids>                   Detach links
    bpfman link get <link-id>                       Get link details (assignable)
    bpfman link list [flags]                        List managed links
    bpfman link delete <link-ids> [-r]              Delete with cascading cleanup

  Dispatcher management:
    bpfman dispatcher list [--type <type>]           List dispatchers
    bpfman dispatcher get <type> <nsid> <ifindex>    Get dispatcher details
    bpfman dispatcher delete <type> <nsid> <ifindex> Delete a dispatcher

  Diagnostics:
    bpfman audit [rules]                            Audit coherency (read-only)
    bpfman audit explain [rule]                     Explain a coherency rule`

func init() {
	parent.Register(driver.Builtin{
		Name:     "bpfman",
		Handler:  Handle,
		Category: driver.CategoryIO,
		Usage:    "bpfman <subcommand> ...",
		Summary:  "Run bpfman program, link, dispatcher, and audit subcommands.",
		Detail:   HelpDetail,
	})
}

func IsTopLevelNoun(name string) bool {
	return topLevelNouns[name]
}

func Handle(c driver.Ctx) (runtime.Value, error) {
	val, err := dispatch(c.Ctx, c.CLI, c.Mgr, c.Args)
	if err != nil {
		return runtime.Value{}, &driver.RuntimeError{Msg: err.Error(), Span: c.Span}
	}
	return val, nil
}

func dispatch(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []runtime.Arg) (runtime.Value, error) {
	var err error
	args, err = maybeBrokerLoadFileArgs(ctx, args)
	if err != nil {
		return runtime.Value{}, err
	}
	args, err = resolveE2EImageRefsInArgs(args)
	if err != nil {
		return runtime.Value{}, err
	}
	if bpfmanDispatchMode == dispatchExternal {
		return dispatchCommandExternal(ctx, args)
	}
	return dispatchCommandLibrary(ctx, cli, mgr, args)
}

func dispatchCommandLibrary(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []runtime.Arg) (runtime.Value, error) {
	cmd, err := parseCommand(args)
	if err != nil {
		return runtime.Value{}, err
	}
	if cmd == nil {
		return runtime.Value{}, fmt.Errorf("missing command after \"bpfman\"; try \"bpfman program list\"")
	}
	return execCommand(ctx, cli, mgr, cmd)
}
