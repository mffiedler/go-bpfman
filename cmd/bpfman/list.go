package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman"
	"k8s.io/apimachinery/pkg/labels"
)

// ListCmd lists managed programs or links.
type ListCmd struct {
	Programs ListProgramsCmd `cmd:"" default:"withargs" help:"List managed programs."`
	Links    ListLinksCmd    `cmd:"" help:"List managed links."`
}

// ListProgramsCmd lists managed BPF programs.
type ListProgramsCmd struct {
	OutputFlags
	Quiet      bool     `short:"q" help:"Output only program IDs, one per line."`
	Attached   bool     `name:"attached" help:"Show only programs with active links."`
	Unattached bool     `name:"unattached" help:"Show only programs without active links."`
	Type       []string `name:"type" sep:"," help:"Filter by program type (case-insensitive, e.g., --type=xdp,kprobe)."`
	Selector   string   `name:"selector" short:"l" help:"Label selector (e.g., app=myapp,version!=v1)."`
}

// Validate checks that the command flags are consistent.
func (c *ListProgramsCmd) Validate() error {
	if c.Attached && c.Unattached {
		return fmt.Errorf("--attached and --unattached are mutually exclusive")
	}
	return nil
}

func (c *ListProgramsCmd) buildFilter() (*bpfman.ProgramFilter, error) {
	filter := &bpfman.ProgramFilter{
		LabelSelector: labels.Everything(),
	}

	// Attachment state
	if c.Attached {
		filter.AttachmentState = bpfman.AttachmentStateAttached
	} else if c.Unattached {
		filter.AttachmentState = bpfman.AttachmentStateUnattached
	}

	// Type filter
	if len(c.Type) > 0 {
		types, err := ParseProgramTypes(c.Type)
		if err != nil {
			return nil, err
		}
		filter.Types = types
	}

	// Label selector
	if s := strings.TrimSpace(c.Selector); s != "" {
		sel, err := labels.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}
		filter.LabelSelector = sel
	}

	return filter, nil
}

// Run executes the list programs command.
func (c *ListProgramsCmd) Run(cli *CLI, ctx context.Context) error {
	if err := c.Validate(); err != nil {
		return err
	}

	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	filter, err := c.buildFilter()
	if err != nil {
		return err
	}

	result, err := mgr.ListProgramsWithFilter(ctx, filter)
	if err != nil {
		return err
	}

	if len(result.Programs) == 0 {
		return nil
	}

	if c.Quiet {
		var b strings.Builder
		for _, p := range result.Programs {
			fmt.Fprintf(&b, "program/%d\n", p.Spec.KernelID)
		}
		return cli.PrintOut(b.String())
	}

	output, err := FormatProgramsComposite(result, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// ListLinksCmd lists managed links.
type ListLinksCmd struct {
	OutputFlags
	Quiet     bool       `short:"q" help:"Output only link IDs, one per line."`
	ProgramID *ProgramID `name:"program-id" help:"Filter by program ID (supports hex with 0x prefix)."`
}

// Run executes the list links command.
func (c *ListLinksCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	var links []bpfman.LinkSpec
	if c.ProgramID != nil {
		links, err = mgr.ListLinksByProgram(ctx, c.ProgramID.Value)
	} else {
		links, err = mgr.ListLinks(ctx)
	}
	if err != nil {
		return err
	}

	if len(links) == 0 {
		return nil
	}

	if c.Quiet {
		var b strings.Builder
		for _, l := range links {
			fmt.Fprintf(&b, "link/%d\n", l.ID)
		}
		return cli.PrintOut(b.String())
	}

	output, err := FormatLinkList(links, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}
