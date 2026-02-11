package main

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/frobware/go-bpfman"
)

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

func (c *ListProgramsCmd) buildListOptions() ([]bpfman.ListOption, error) {
	var opts []bpfman.ListOption

	// Attachment state
	if c.Attached {
		opts = append(opts, bpfman.WithAttached())
	} else if c.Unattached {
		opts = append(opts, bpfman.WithUnattached())
	}

	// Type filter
	if len(c.Type) > 0 {
		types, err := ParseProgramTypesSlice(c.Type)
		if err != nil {
			return nil, err
		}
		opts = append(opts, bpfman.WithTypes(types...))
	}

	// Label selector
	if s := strings.TrimSpace(c.Selector); s != "" {
		sel, err := labels.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}
		opts = append(opts, bpfman.MatchingSelector(sel))
	}

	return opts, nil
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

	opts, err := c.buildListOptions()
	if err != nil {
		return err
	}

	result, err := mgr.ListPrograms(ctx, opts...)
	if err != nil {
		return err
	}

	if len(result.Programs) == 0 && !c.OutputFlags.IsStructured() {
		return nil
	}

	if c.Quiet {
		var b strings.Builder
		for _, p := range result.Programs {
			fmt.Fprintf(&b, "program/%d\n", p.Record.ProgramID)
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
	Kind      []string   `name:"kind" sep:"," help:"Filter by link kind (e.g., --kind=xdp,kprobe)."`
}

func (c *ListLinksCmd) buildLinkListOptions() ([]bpfman.LinkListOption, error) {
	var opts []bpfman.LinkListOption

	if c.ProgramID != nil {
		opts = append(opts, bpfman.WithProgramID(c.ProgramID.Value))
	}

	if len(c.Kind) > 0 {
		kinds, err := ParseLinkKindsSlice(c.Kind)
		if err != nil {
			return nil, err
		}
		opts = append(opts, bpfman.WithKinds(kinds...))
	}

	return opts, nil
}

// Run executes the list links command.
func (c *ListLinksCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	opts, err := c.buildLinkListOptions()
	if err != nil {
		return err
	}

	links, err := mgr.ListLinks(ctx, opts...)
	if err != nil {
		return err
	}

	if len(links) == 0 && !c.OutputFlags.IsStructured() {
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
