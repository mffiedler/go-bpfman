package main

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/manager"
)

// ListProgramsCmd lists managed BPF programs.
type ListProgramsCmd struct {
	cliformat.OutputFlags
	Quiet            bool                 `short:"q" help:"Output only program IDs, one per line."`
	Attached         bool                 `name:"attached" help:"Show only programs with active links."`
	Unattached       bool                 `name:"unattached" help:"Show only programs without active links."`
	Type             []bpfman.ProgramType `name:"type" sep:"," help:"Filter by program type (case-insensitive, e.g., --type=xdp,kprobe)."`
	ProgramType      []bpfman.ProgramType `name:"program-type" short:"p" sep:"," help:"Filter by program type (Rust-compatible alias for --type)."`
	Application      string               `name:"application" help:"Filter by application metadata."`
	MetadataSelector []bpfmancli.KeyValue `name:"metadata-selector" short:"m" help:"Filter by KEY=VALUE metadata (can be repeated)."`
	All              bool                 `name:"all" short:"a" help:"Accepted for Rust CLI compatibility; Go lists all managed programs by default."`
	Selector         string               `name:"selector" short:"l" help:"Label selector (e.g., app=myapp,version!=v1)."`
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

	if len(c.Type) > 0 {
		opts = append(opts, bpfman.WithTypes(c.Type...))
	}
	if len(c.ProgramType) > 0 {
		opts = append(opts, bpfman.WithTypes(c.ProgramType...))
	}

	var selectors []labels.Selector
	metadata := bpfmancli.MetadataMap(c.MetadataSelector)
	if c.Application != "" {
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata[manager.ApplicationMetadataKey] = c.Application
	}
	if len(metadata) > 0 {
		selectors = append(selectors, labels.SelectorFromSet(labels.Set(metadata)))
	}
	if s := strings.TrimSpace(c.Selector); s != "" {
		sel, err := labels.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}
		selectors = append(selectors, sel)
	}
	if len(selectors) > 0 {
		opts = append(opts, bpfman.MatchingSelector(combineSelectors(selectors...)))
	}

	return opts, nil
}

func combineSelectors(selectors ...labels.Selector) labels.Selector {
	combined := labels.NewSelector()
	for _, sel := range selectors {
		requirements, selectable := sel.Requirements()
		if !selectable {
			return labels.Nothing()
		}
		combined = combined.Add(requirements...)
	}
	return combined
}

// Run executes the list programs command.
func (c *ListProgramsCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
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

	output, err := cliformat.FormatProgramsComposite(result, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// ListLinksCmd lists managed links.
type ListLinksCmd struct {
	cliformat.OutputFlags
	Quiet            bool                 `short:"q" help:"Output only link IDs, one per line."`
	ProgramID        *bpfmancli.ProgramID `name:"program-id" help:"Filter by program ID (supports hex with 0x prefix)."`
	Kind             []bpfman.LinkKind    `name:"kind" sep:"," help:"Filter by link kind (e.g., --kind=xdp,kprobe)."`
	ProgramType      []bpfman.ProgramType `name:"program-type" short:"p" sep:"," help:"Filter by the owning program's type (e.g. xdp,kprobe)."`
	Application      string               `name:"application" help:"Filter by the owning program's application metadata."`
	MetadataSelector []bpfmancli.KeyValue `name:"metadata-selector" short:"m" help:"Filter by the owning program's KEY=VALUE metadata (can be repeated)."`
}

func (c *ListLinksCmd) buildLinkListOptions() ([]bpfman.LinkListOption, error) {
	var opts []bpfman.LinkListOption

	if c.ProgramID != nil {
		opts = append(opts, bpfman.WithProgramID(c.ProgramID.Value))
	}

	if len(c.Kind) > 0 {
		opts = append(opts, bpfman.WithKinds(c.Kind...))
	}

	return opts, nil
}

// programScopeOptions builds the program-list options for Rust-style
// program-scoped link filtering: --program-type, --application and
// --metadata-selector select the owning program. The bool reports whether
// any program-scope filter was supplied; when false, all links are listed
// (subject only to the link-level filters).
func (c *ListLinksCmd) programScopeOptions() ([]bpfman.ListOption, bool) {
	var opts []bpfman.ListOption
	scoped := false

	if len(c.ProgramType) > 0 {
		opts = append(opts, bpfman.WithTypes(c.ProgramType...))
		scoped = true
	}

	metadata := bpfmancli.MetadataMap(c.MetadataSelector)
	if c.Application != "" {
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata[manager.ApplicationMetadataKey] = c.Application
	}
	if len(metadata) > 0 {
		opts = append(opts, bpfman.MatchingSelector(labels.SelectorFromSet(labels.Set(metadata))))
		scoped = true
	}

	return opts, scoped
}

// Run executes the list links command.
func (c *ListLinksCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	opts, err := c.buildLinkListOptions()
	if err != nil {
		return err
	}

	var links []bpfman.LinkRecord
	if progOpts, scoped := c.programScopeOptions(); scoped {
		links, err = mgr.ListLinksScopedToPrograms(ctx, progOpts, opts)
	} else {
		links, err = mgr.ListLinks(ctx, opts...)
	}
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

	output, err := cliformat.FormatLinkList(links, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}
