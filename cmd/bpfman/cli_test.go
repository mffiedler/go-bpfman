package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestMaybeInjectServe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"no args",
			[]string{"bpfman"},
			[]string{"bpfman"},
		},
		{
			"csi-support only",
			[]string{"bpfman", "--csi-support"},
			[]string{"bpfman", "serve", "--csi-support"},
		},
		{
			"csi-support followed by another flag",
			[]string{"bpfman", "--csi-support", "--tcp-address=:50051"},
			[]string{"bpfman", "serve", "--csi-support", "--tcp-address=:50051"},
		},
		{
			"csi-support followed by separated flag value",
			[]string{"bpfman", "--csi-support", "--tcp-address", ":50051"},
			[]string{"bpfman", "serve", "--csi-support", "--tcp-address", ":50051"},
		},
		{
			"explicit subcommand",
			[]string{"bpfman", "get", "link", "5"},
			[]string{"bpfman", "get", "link", "5"},
		},
		{
			"version subcommand",
			[]string{"bpfman", "version"},
			[]string{"bpfman", "version"},
		},
		{
			"explicit serve",
			[]string{"bpfman", "serve", "--csi-support"},
			[]string{"bpfman", "serve", "--csi-support"},
		},
		{
			"non-marker flag alone",
			[]string{"bpfman", "--tcp-address=:50051"},
			[]string{"bpfman", "--tcp-address=:50051"},
		},
		{
			"marker not at argv[1]",
			[]string{"bpfman", "--tcp-address=:50051", "--csi-support"},
			[]string{"bpfman", "--tcp-address=:50051", "--csi-support"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := maybeInjectServe(tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("maybeInjectServe(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestRootHelpShowsOnlyPublicLifecycleCommands(t *testing.T) {
	t.Parallel()

	type helpExit struct{}

	var cli CLI
	var out bytes.Buffer
	parser, err := kong.New(&cli, append(KongOptions(),
		kong.Writers(&out, &out),
		kong.Exit(func(int) { panic(helpExit{}) }),
	)...)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	exited := false
	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			if _, ok := r.(helpExit); !ok {
				panic(r)
			}
			exited = true
		}()
		_, err = parser.Parse([]string{"--help"})
	}()
	if err != nil {
		t.Fatalf("Parse(--help): %v", err)
	}
	if !exited {
		t.Fatal("Parse(--help) did not exit after printing help")
	}

	help := out.String()
	for _, want := range []string{
		"program load file",
		"program load image",
		"program unload",
		"program get",
		"program list",
		"link attach xdp",
		"link attach tc",
		"link attach tcx",
		"link attach tracepoint",
		"link attach kprobe",
		"link attach uprobe",
		"link attach fentry",
		"link attach fexit",
		"link detach",
		"link get",
		"link list",
		"version",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("root help missing public command %q:\n%s", want, help)
		}
	}

	for _, hidden := range []string{
		"program delete",
		"link delete",
		"dispatcher list",
		"dispatcher get",
		"image build",
		"image generate-build-args",
		"image inspect",
		"image verify",
		"serve",
	} {
		if strings.Contains(help, hidden) {
			t.Fatalf("root help exposes hidden command %q:\n%s", hidden, help)
		}
	}
}
