package bpfmanbuiltin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/internal/registryfixture"
	"github.com/frobware/go-bpfman/kernel"
)

// structured helper builds a program-origin structured value with a
// given program ID for use in attach/detach parse tests.
func structuredProgram(name string, progID kernel.ProgramID) runtime.Arg {
	val, err := runtime.ValueFromStruct(bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: progID},
	})
	if err != nil {
		panic(err)
	}
	return runtime.StructuredValueArg{Name: name, Value: val.WithKind(semantics.OriginProgram)}
}

func structuredLink(name string, linkID bpfman.LinkID) runtime.Arg {
	val, err := runtime.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{ID: linkID},
	})
	if err != nil {
		panic(err)
	}
	return runtime.StructuredValueArg{Name: name, Value: val.WithKind(semantics.OriginLink)}
}

func word(s string) runtime.Arg { return runtime.WordArg{Text: s} }

func TestDispatchCommandLibrary_NilManagerIsRejected(t *testing.T) {
	t.Parallel()

	_, err := dispatchCommandLibrary(t.Context(), nil, nil, []runtime.Arg{
		word("program"),
		word("list"),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a manager")
}

func TestParseListProgramsRustCompatibleFlags(t *testing.T) {
	t.Parallel()

	cmd, err := parseListPrograms([]runtime.Arg{
		word("--program-type"), word("xdp"),
		word("-p"), word("kprobe"),
		word("--application"), word("demo"),
		word("--metadata-selector"), word("env=prod"),
		word("-m"), word("team=net"),
		word("--all"),
	})
	require.NoError(t, err)

	assert.True(t, cmd.All)
	assert.Equal(t, "demo", cmd.Application)
	assert.Equal(t, []bpfman.ProgramType{
		bpfman.ProgramTypeXDP,
		bpfman.ProgramTypeKprobe,
	}, cmd.Types)
	require.Len(t, cmd.MetadataSelector, 2)
	assert.Equal(t, "env", cmd.MetadataSelector[0].Key)
	assert.Equal(t, "prod", cmd.MetadataSelector[0].Value)
	assert.Equal(t, "team", cmd.MetadataSelector[1].Key)
	assert.Equal(t, "net", cmd.MetadataSelector[1].Value)
}

func TestParseListLinksRustCompatibleFlags(t *testing.T) {
	t.Parallel()

	cmd, err := parseListLinks([]runtime.Arg{
		word("--program-type"), word("tc"),
		word("-p"), word("uprobe"),
		word("--application"), word("demo"),
		word("--metadata-selector"), word("env=prod"),
		word("-m"), word("team=net"),
	})
	require.NoError(t, err)

	assert.Equal(t, "demo", cmd.Application)
	assert.Equal(t, []bpfman.ProgramType{
		bpfman.ProgramTypeTC,
		bpfman.ProgramTypeUprobe,
	}, cmd.ProgramTypes)
	require.Len(t, cmd.MetadataSelector, 2)
	assert.Equal(t, "env", cmd.MetadataSelector[0].Key)
	assert.Equal(t, "prod", cmd.MetadataSelector[0].Value)
	assert.Equal(t, "team", cmd.MetadataSelector[1].Key)
	assert.Equal(t, "net", cmd.MetadataSelector[1].Value)
}

func TestParseImageInspect(t *testing.T) {
	t.Parallel()

	cmd, err := parseCommand([]runtime.Arg{
		word("image"),
		word("inspect"),
		word("quay.io/bpfman-bytecode/go-xdp-counter:latest"),
	})
	require.NoError(t, err)
	inspect, ok := cmd.(*ImageInspectCommand)
	require.True(t, ok, "command type = %T", cmd)
	assert.Equal(t, "quay.io/bpfman-bytecode/go-xdp-counter:latest", inspect.ImageURL)
}

func TestParseImageInspectResolvesE2EImageRef(t *testing.T) {
	t.Setenv(registryfixture.RegistryEnv, "127.0.0.1:5000")

	args, err := resolveE2EImageRefsInArgs([]runtime.Arg{
		word("image"),
		word("inspect"),
		word(registryfixture.RegistryAlias + "/bpfman-e2e/xdp-pass:latest"),
	})
	require.NoError(t, err)
	cmd, err := parseCommand(args)
	require.NoError(t, err)
	inspect, ok := cmd.(*ImageInspectCommand)
	require.True(t, ok, "command type = %T", cmd)
	assert.Equal(t, "127.0.0.1:5000/bpfman-e2e/xdp-pass:latest", inspect.ImageURL)
}

func TestResolveE2EImageRefStartsRegistry(t *testing.T) {
	t.Setenv(registryfixture.RegistryEnv, "")
	defer registryfixture.Close()

	got, err := resolveE2EImageRef(registryfixture.RegistryAlias + "/bpfman-e2e/xdp-pass:latest")
	require.NoError(t, err)
	assert.NotContains(t, got, registryfixture.RegistryAlias)
	assert.Contains(t, got, "/bpfman-e2e/xdp-pass:latest")
	assert.Empty(t, os.Getenv(registryfixture.RegistryEnv))
}

func TestParseImageBuildResolvesE2ETag(t *testing.T) {
	t.Setenv(registryfixture.RegistryEnv, "localhost:5000")

	args, err := resolveE2EImageRefsInArgs([]runtime.Arg{
		word("image"),
		word("build"),
		word(registryfixture.RegistryAlias + "/bpfman-e2e/xdp-pass:latest"),
		word("e2e/testdata/bpf/xdp_pass.bpf.o"),
	})
	require.NoError(t, err)
	cmd, err := parseCommand(args)
	require.NoError(t, err)
	build, ok := cmd.(*ImageBuildCommand)
	require.True(t, ok, "command type = %T", cmd)
	assert.Equal(t, []string{
		"localhost:5000/bpfman-e2e/xdp-pass:latest",
		"e2e/testdata/bpf/xdp_pass.bpf.o",
	}, build.Args)
}

func TestCommandSupportsOutputSkipsImageBuild(t *testing.T) {
	t.Parallel()

	assert.False(t, commandSupportsOutput([]string{"image", "build"}))
	assert.False(t, commandSupportsOutput([]string{"image", "inspect"}))
	assert.True(t, commandSupportsOutput([]string{"program", "list"}))
}

func TestDispatchCommandExternalInheritsBPFMANConfig(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bpfman")
	seen := filepath.Join(dir, "seen-config")
	configPath := filepath.Join(dir, "no-signature-verification.toml")
	script := `#!/bin/sh
printf '%s' "$BPFMAN_CONFIG" > "$BPFMAN_CONFIG_SEEN"
printf '{"programs":[]}'
`
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	t.Setenv("BPFMAN_BIN", bin)
	t.Setenv("BPFMAN_CONFIG", configPath)
	t.Setenv("BPFMAN_CONFIG_SEEN", seen)

	_, err := dispatchCommandExternal(t.Context(), []runtime.Arg{
		word("program"),
		word("list"),
	})
	require.NoError(t, err)

	got, err := os.ReadFile(seen)
	require.NoError(t, err)
	assert.Equal(t, configPath, string(got))
}

func TestDispatchCommandExternal_ContextCancelInterruptsChildProcessGroup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bpfman")
	child := filepath.Join(dir, "child.sh")
	ready := filepath.Join(dir, "ready")
	ack := filepath.Join(dir, "ack")
	require.NoError(t, os.WriteFile(bin, []byte(`#!/bin/sh
"$BPFMAN_CANCEL_CHILD" "$BPFMAN_CANCEL_ACK" "$BPFMAN_CANCEL_READY"; :
`), 0o755))
	require.NoError(t, os.WriteFile(child, []byte(`#!/bin/sh
trap 'echo interrupted > "$1"; exit 0' INT
echo ready > "$2"
sleep 2
`), 0o755))

	t.Setenv("BPFMAN_BIN", bin)
	t.Setenv("BPFMAN_CANCEL_CHILD", child)
	t.Setenv("BPFMAN_CANCEL_READY", ready)
	t.Setenv("BPFMAN_CANCEL_ACK", ack)

	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("script context cancelled")
	errCh := make(chan error, 1)
	go func() {
		_, err := dispatchCommandExternal(ctx, []runtime.Arg{
			word("program"),
			word("list"),
		})
		errCh <- err
	}()

	assert.Eventually(t, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, time.Second, 20*time.Millisecond)
	cancel(cause)

	assert.Eventually(t, func() bool {
		_, err := os.Stat(ack)
		return err == nil
	}, time.Second, 20*time.Millisecond)
	assert.Equal(t, cause, <-errCh)
}

func TestParseImageInspectRejectsMissingImage(t *testing.T) {
	t.Parallel()

	_, err := parseCommand([]runtime.Arg{word("image"), word("inspect")})
	require.ErrorContains(t, err, "requires an image reference")
}

func TestParseShowProgram(t *testing.T) {
	t.Parallel()

	structuredVal, err := runtime.ValueFromStruct(bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: kernel.ProgramID(42)},
	})
	require.NoError(t, err)
	structuredVal = structuredVal.WithKind(semantics.OriginProgram)

	linkVal, err := runtime.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        bpfman.LinkID(10),
			ProgramID: kernel.ProgramID(42),
		},
	})
	require.NoError(t, err)
	linkVal = linkVal.WithKind(semantics.OriginLink)

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantID     kernel.ProgramID
		wantView   string
		wantOutput string
		wantErr    string
	}{
		{
			name:       "numeric ID only",
			args:       []runtime.Arg{runtime.WordArg{Text: "123"}},
			wantID:     123,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name:       "hex ID",
			args:       []runtime.Arg{runtime.WordArg{Text: "0x1a"}},
			wantID:     26,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "structured variable ref",
			args: []runtime.Arg{
				runtime.StructuredValueArg{Name: "prog", Value: structuredVal},
			},
			wantID:     42,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "scalar value arg",
			args: []runtime.Arg{
				runtime.ScalarValueArg{Text: "55"},
			},
			wantID:     55,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "with view argument",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "links"},
			},
			wantID:     100,
			wantView:   "links",
			wantOutput: "table",
		},
		{
			name: "with output flag",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "-o"},
				runtime.WordArg{Text: "json"},
			},
			wantID:     100,
			wantView:   "summary",
			wantOutput: "json",
		},
		{
			name: "view and output flag",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "maps"},
				runtime.WordArg{Text: "-o"},
				runtime.WordArg{Text: "wide"},
			},
			wantID:     100,
			wantView:   "maps",
			wantOutput: "wide",
		},
		{
			name: "output flag before view",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "-o"},
				runtime.WordArg{Text: "json"},
				runtime.WordArg{Text: "paths"},
			},
			wantID:     100,
			wantView:   "paths",
			wantOutput: "json",
		},
		{
			name: "structured ref with view",
			args: []runtime.Arg{
				runtime.StructuredValueArg{Name: "prog", Value: structuredVal},
				runtime.WordArg{Text: "maps"},
			},
			wantID:     42,
			wantView:   "maps",
			wantOutput: "table",
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "requires a program ID",
		},
		{
			name: "duplicate -o flag",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "-o"},
				runtime.WordArg{Text: "json"},
				runtime.WordArg{Text: "-o"},
				runtime.WordArg{Text: "wide"},
			},
			wantErr: "duplicate -o flag",
		},
		{
			name: "unknown flag",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "--verbose"},
			},
			wantErr: "unknown flag",
		},
		{
			name: "unknown view",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "nonsense"},
			},
			wantErr: "unknown view",
		},
		{
			name: "-o without value",
			args: []runtime.Arg{
				runtime.WordArg{Text: "100"},
				runtime.WordArg{Text: "-o"},
			},
			wantErr: "-o requires a value",
		},
		{
			name: "wrong origin type on structured ref",
			args: []runtime.Arg{
				runtime.StructuredValueArg{Name: "mylink", Value: linkVal},
			},
			wantErr: `variable "$mylink" is a link; expected program`,
		},
		{
			name: "duplicate view positional rejected",
			args: []runtime.Arg{
				runtime.WordArg{Text: "123"},
				runtime.WordArg{Text: "links"},
				runtime.WordArg{Text: "maps"},
			},
			wantErr: "only one view may be specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseShowProgram(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, cmd.ID)
			assert.Equal(t, tt.wantView, cmd.View)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLoadFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []runtime.Arg
		wantPath    string
		wantProgs   int
		wantMeta    int
		wantGlobal  int
		wantApp     string
		wantOwnerID kernel.ProgramID
		wantOutput  string
		wantErr     string
	}{
		{
			name:       "path only",
			args:       []runtime.Arg{word("/tmp/test.o")},
			wantPath:   "/tmp/test.o",
			wantOutput: "table",
		},
		{
			name: "all flags",
			args: []runtime.Arg{
				word("/tmp/test.o"),
				word("--programs"), word("xdp:xdp_pass"),
				word("-m"), word("app=test"),
				word("-g"), word("RATE=0a000000"),
				word("-a"), word("myapp"),
				word("--map-owner-id"), word("42"),
				word("-o"), word("json"),
			},
			wantPath:    "/tmp/test.o",
			wantProgs:   1,
			wantMeta:    1,
			wantGlobal:  1,
			wantApp:     "myapp",
			wantOwnerID: 42,
			wantOutput:  "json",
		},
		{
			name: "multiple programs",
			args: []runtime.Arg{
				word("/tmp/test.o"),
				word("--programs"), word("xdp:xdp_pass"),
				word("--programs"), word("tc:tc_stats"),
			},
			wantPath:   "/tmp/test.o",
			wantProgs:  2,
			wantOutput: "table",
		},
		{
			name: "multiple metadata",
			args: []runtime.Arg{
				word("/tmp/test.o"),
				word("-m"), word("a=1"),
				word("-m"), word("b=2"),
			},
			wantPath:   "/tmp/test.o",
			wantMeta:   2,
			wantOutput: "table",
		},
		{
			name:    "missing path",
			args:    []runtime.Arg{word("-m"), word("a=1")},
			wantErr: "requires a path",
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "requires a path",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("/tmp/test.o"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "unexpected positional",
			args:    []runtime.Arg{word("/tmp/test.o"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name:    "duplicate -o flag",
			args:    []runtime.Arg{word("/tmp/test.o"), word("-o"), word("json"), word("-o"), word("wide")},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "invalid program spec",
			args:    []runtime.Arg{word("/tmp/test.o"), word("--programs"), word("badspec")},
			wantErr: "invalid program spec",
		},
		{
			name:    "invalid metadata",
			args:    []runtime.Arg{word("/tmp/test.o"), word("-m"), word("noequalssign")},
			wantErr: "invalid format",
		},
		{
			name:    "invalid global data",
			args:    []runtime.Arg{word("/tmp/test.o"), word("-g"), word("BAD=notahex!")},
			wantErr: "invalid hex data",
		},
		{
			name:    "invalid map-owner-id",
			args:    []runtime.Arg{word("/tmp/test.o"), word("--map-owner-id"), word("abc")},
			wantErr: "invalid program ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLoadFile(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, cmd.Path)
			assert.Len(t, cmd.Programs, tt.wantProgs)
			assert.Len(t, cmd.Metadata, tt.wantMeta)
			assert.Len(t, cmd.GlobalData, tt.wantGlobal)
			assert.Equal(t, tt.wantApp, cmd.Application)
			assert.Equal(t, tt.wantOwnerID, cmd.MapOwnerID)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLoadImage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []runtime.Arg
		wantURL        string
		wantProgs      int
		wantPullPolicy string
		wantAuth       string
		wantApp        string
		wantMeta       int
		wantGlobal     int
		wantOwnerID    kernel.ProgramID
		wantOutput     string
		wantErr        string
	}{
		{
			name:           "image url only",
			args:           []runtime.Arg{word("quay.io/bpfman/xdp_pass:latest")},
			wantURL:        "quay.io/bpfman/xdp_pass:latest",
			wantPullPolicy: "IfNotPresent",
			wantOutput:     "table",
		},
		{
			name: "all flags",
			args: []runtime.Arg{
				word("quay.io/bpfman/xdp_pass:latest"),
				word("--programs"), word("xdp:xdp_pass"),
				word("-p"), word("Always"),
				word("--registry-auth"), word("dXNlcjpwYXNz"),
				word("-a"), word("myapp"),
				word("-m"), word("env=prod"),
				word("-g"), word("RATE=0a"),
				word("--map-owner-id"), word("99"),
				word("-o"), word("json"),
			},
			wantURL:        "quay.io/bpfman/xdp_pass:latest",
			wantProgs:      1,
			wantPullPolicy: "Always",
			wantAuth:       "dXNlcjpwYXNz",
			wantApp:        "myapp",
			wantMeta:       1,
			wantGlobal:     1,
			wantOwnerID:    99,
			wantOutput:     "json",
		},
		{
			name:    "missing image url",
			args:    []runtime.Arg{word("--programs"), word("xdp:xdp_pass")},
			wantErr: "requires an image",
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "requires an image",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("img"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "unexpected positional",
			args:    []runtime.Arg{word("img"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name:    "duplicate -o flag",
			args:    []runtime.Arg{word("img"), word("-o"), word("json"), word("-o"), word("wide")},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "invalid program spec",
			args:    []runtime.Arg{word("img"), word("--programs"), word("bad")},
			wantErr: "invalid program spec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLoadImage(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantURL, cmd.ImageURL)
			assert.Len(t, cmd.Programs, tt.wantProgs)
			assert.Equal(t, tt.wantPullPolicy, cmd.PullPolicy)
			assert.Equal(t, tt.wantAuth, cmd.RegistryAuth)
			assert.Equal(t, tt.wantApp, cmd.Application)
			assert.Len(t, cmd.Metadata, tt.wantMeta)
			assert.Len(t, cmd.GlobalData, tt.wantGlobal)
			assert.Equal(t, tt.wantOwnerID, cmd.MapOwnerID)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLinkAttach_Routing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []runtime.Arg
		wantErr string
	}{
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "requires a type",
		},
		{
			name:    "unknown type",
			args:    []runtime.Arg{word("rawsock"), word("42")},
			wantErr: "unknown attach type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkAttachTracepoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "minimal",
			args:       []runtime.Arg{word("tracepoint"), word("42"), word("sched/sched_switch")},
			wantOutput: "table",
		},
		{
			name: "with output flag before positionals",
			args: []runtime.Arg{
				word("tracepoint"), word("-o"), word("json"),
				word("42"), word("sched/sched_switch"),
			},
			wantOutput: "json",
		},
		{
			name: "structured program ref",
			args: []runtime.Arg{
				word("tracepoint"), structuredProgram("prog", 99), word("sched/sched_switch"),
			},
			wantOutput: "table",
		},
		{
			name:    "missing tracepoint",
			args:    []runtime.Arg{word("tracepoint"), word("42")},
			wantErr: "requires a tracepoint",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("tracepoint")},
			wantErr: "requires a program ID",
		},
		{
			name:    "bad tracepoint format",
			args:    []runtime.Arg{word("tracepoint"), word("42"), word("noslash")},
			wantErr: "expected group/name",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("tracepoint"), word("--verbose"), word("42"), word("sched/sched_switch")},
			wantErr: "unknown flag",
		},
		{
			name:    "duplicate -o flag",
			args:    []runtime.Arg{word("tracepoint"), word("-o"), word("json"), word("-o"), word("wide"), word("42"), word("sched/sched_switch")},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "extra positional",
			args:    []runtime.Arg{word("tracepoint"), word("42"), word("sched/sched_switch"), word("extra")},
			wantErr: "unexpected argument",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLinkAttach(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cmd.Spec)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLinkAttachKprobe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "minimal",
			args:       []runtime.Arg{word("kprobe"), word("42"), word("do_unlinkat")},
			wantOutput: "table",
		},
		{
			name: "with offset",
			args: []runtime.Arg{
				word("kprobe"), word("42"), word("do_unlinkat"),
				word("--offset"), word("16"),
			},
			wantOutput: "table",
		},
		{
			name:    "missing fn-name",
			args:    []runtime.Arg{word("kprobe"), word("42")},
			wantErr: "requires <program-id> <fn-name>",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("kprobe")},
			wantErr: "requires <program-id> <fn-name>",
		},
		{
			name:    "invalid offset",
			args:    []runtime.Arg{word("kprobe"), word("42"), word("do_unlinkat"), word("--offset"), word("abc")},
			wantErr: "invalid offset",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLinkAttach(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cmd.Spec)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLinkAttachUprobe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "minimal",
			args:       []runtime.Arg{word("uprobe"), word("42"), word("/usr/lib/libc.so.6")},
			wantOutput: "table",
		},
		{
			name: "all optional flags",
			args: []runtime.Arg{
				word("uprobe"), word("42"), word("/usr/lib/libc.so.6"),
				word("-f"), word("malloc"), word("--offset"), word("8"),
				word("--container-pid"), word("1234"),
				word("-o"), word("json"),
			},
			wantOutput: "json",
		},
		{
			name:    "missing target",
			args:    []runtime.Arg{word("uprobe"), word("42")},
			wantErr: "requires <program-id> <target>",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("uprobe")},
			wantErr: "requires <program-id> <target>",
		},
		{
			name:    "invalid container-pid",
			args:    []runtime.Arg{word("uprobe"), word("42"), word("/bin/foo"), word("--container-pid"), word("abc")},
			wantErr: "invalid container-pid",
		},
		{
			name:       "pid filter",
			args:       []runtime.Arg{word("uprobe"), word("42"), word("/bin/foo"), word("--pid"), word("1234")},
			wantOutput: "table",
		},
		{
			name:    "invalid pid",
			args:    []runtime.Arg{word("uprobe"), word("42"), word("/bin/foo"), word("--pid"), word("abc")},
			wantErr: "invalid pid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLinkAttach(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cmd.Spec)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

// TestParseLinkAttachUprobe_PidReachesSpec pins that --pid lands on
// the spec rather than merely parsing: the spec is what crosses into
// the manager, so a parsed-but-dropped pid would silently trace
// every process.
func TestParseLinkAttachUprobe_PidReachesSpec(t *testing.T) {
	t.Parallel()

	cmd, err := parseLinkAttach([]runtime.Arg{
		word("uprobe"), word("42"), word("/bin/foo"), word("--pid"), word("1234"),
	})
	require.NoError(t, err)
	spec, ok := cmd.Spec.(bpfman.UprobeAttachSpec)
	require.True(t, ok, "expected UprobeAttachSpec, got %T", cmd.Spec)
	assert.Equal(t, int32(1234), spec.Pid())
}

func TestParseLinkAttachFentry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "program ID only",
			args:       []runtime.Arg{word("fentry"), word("42")},
			wantOutput: "table",
		},
		{
			name:       "with output flag",
			args:       []runtime.Arg{word("fentry"), word("-o"), word("json"), word("42")},
			wantOutput: "json",
		},
		{
			name: "structured program ref",
			args: []runtime.Arg{
				word("fentry"), structuredProgram("prog", 55),
			},
			wantOutput: "table",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("fentry")},
			wantErr: "requires a program ID",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("fentry"), word("--verbose"), word("42")},
			wantErr: "unknown flag",
		},
		{
			name:    "wrong origin type",
			args:    []runtime.Arg{word("fentry"), structuredLink("lnk", 10)},
			wantErr: `variable "$lnk" is a link; expected program`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLinkAttach(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cmd.Spec)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLinkAttachFexit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "program ID only",
			args:       []runtime.Arg{word("fexit"), word("42")},
			wantOutput: "table",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("fexit")},
			wantErr: "requires a program ID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLinkAttach(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cmd.Spec)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLinkAttachNetworkTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantOutput string
	}{
		{
			name:       "xdp",
			args:       []runtime.Arg{word("xdp"), word("42"), word("lo"), word("--priority"), word("10")},
			wantOutput: "table",
		},
		{
			name:       "tc",
			args:       []runtime.Arg{word("tc"), word("42"), word("lo"), word("ingress"), word("--priority"), word("10")},
			wantOutput: "table",
		},
		{
			name:       "tcx",
			args:       []runtime.Arg{word("tcx"), word("42"), word("lo"), word("egress"), word("--priority"), word("10"), word("-o"), word("json")},
			wantOutput: "json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLinkAttach(tt.args)
			require.NoError(t, err)
			assert.NotNil(t, cmd.Spec)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseLinkAttachXDP_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []runtime.Arg
		wantErr string
	}{
		{
			name:    "missing iface",
			args:    []runtime.Arg{word("xdp"), word("42")},
			wantErr: "requires <program-id> <iface>",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("xdp")},
			wantErr: "requires <program-id> <iface>",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("xdp"), word("42"), word("lo"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "missing priority",
			args:    []runtime.Arg{word("xdp"), word("42"), word("lo")},
			wantErr: "requires --priority",
		},
		{
			name:    "invalid priority",
			args:    []runtime.Arg{word("xdp"), word("42"), word("lo"), word("--priority"), word("abc")},
			wantErr: "invalid priority",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkAttachTC_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []runtime.Arg
		wantErr string
	}{
		{
			name:    "missing iface",
			args:    []runtime.Arg{word("tc"), word("42")},
			wantErr: "requires <program-id> <iface> <direction>",
		},
		{
			name:    "missing direction",
			args:    []runtime.Arg{word("tc"), word("42"), word("lo")},
			wantErr: "requires <program-id> <iface> <direction>",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("tc")},
			wantErr: "requires <program-id> <iface> <direction>",
		},
		{
			name:    "missing priority",
			args:    []runtime.Arg{word("tc"), word("42"), word("lo"), word("ingress")},
			wantErr: "requires --priority",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkAttachTCX_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []runtime.Arg
		wantErr string
	}{
		{
			name:    "missing iface",
			args:    []runtime.Arg{word("tcx"), word("42")},
			wantErr: "requires <program-id> <iface> <direction>",
		},
		{
			name:    "missing direction",
			args:    []runtime.Arg{word("tcx"), word("42"), word("lo")},
			wantErr: "requires <program-id> <iface> <direction>",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("tcx")},
			wantErr: "requires <program-id> <iface> <direction>",
		},
		{
			name:    "missing priority",
			args:    []runtime.Arg{word("tcx"), word("42"), word("lo"), word("ingress")},
			wantErr: "requires --priority",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkAttachMetadataThreadedToSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []runtime.Arg
	}{
		{
			name: "xdp",
			args: []runtime.Arg{word("xdp"), word("42"), word("lo"), word("--priority"), word("50"), word("-m"), word("k=v")},
		},
		{
			name: "tc",
			args: []runtime.Arg{word("tc"), word("42"), word("lo"), word("ingress"), word("--priority"), word("50"), word("-m"), word("k=v")},
		},
		{
			name: "tcx",
			args: []runtime.Arg{word("tcx"), word("42"), word("lo"), word("ingress"), word("--priority"), word("50"), word("-m"), word("k=v")},
		},
		{
			name: "tracepoint",
			args: []runtime.Arg{
				word("tracepoint"), word("-m"), word("k=v"),
				word("42"), word("sched/sched_switch"),
			},
		},
		{
			name: "kprobe",
			args: []runtime.Arg{word("kprobe"), word("42"), word("do_unlinkat"), word("-m"), word("k=v")},
		},
		{
			name: "uprobe",
			args: []runtime.Arg{word("uprobe"), word("42"), word("/bin/foo"), word("-m"), word("k=v")},
		},
		{
			name: "fentry",
			args: []runtime.Arg{word("fentry"), word("-m"), word("k=v"), word("42")},
		},
		{
			name: "fexit",
			args: []runtime.Arg{word("fexit"), word("-m"), word("k=v"), word("42")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// -m/--metadata is parsed and threaded onto the attach spec,
			// so the metadata reaches the manager and is persisted.
			cmd, err := parseLinkAttach(tt.args)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"k": "v"}, cmd.Spec.Metadata(),
				"-m metadata must be threaded onto the attach spec")
		})
	}
}

func TestParseLinkDetach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []runtime.Arg
		wantIDs []bpfman.LinkID
		wantErr string
	}{
		{
			name:    "single numeric ID",
			args:    []runtime.Arg{word("42")},
			wantIDs: []bpfman.LinkID{42},
		},
		{
			name:    "multiple numeric IDs",
			args:    []runtime.Arg{word("10"), word("20"), word("30")},
			wantIDs: []bpfman.LinkID{10, 20, 30},
		},
		{
			name:    "structured variable ref",
			args:    []runtime.Arg{structuredLink("lnk", 77)},
			wantIDs: []bpfman.LinkID{77},
		},
		{
			name: "mixed numeric and structured",
			args: []runtime.Arg{
				word("5"),
				structuredLink("lnk", 99),
			},
			wantIDs: []bpfman.LinkID{5, 99},
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "requires at least one link ID",
		},
		{
			name:    "invalid ID",
			args:    []runtime.Arg{word("abc")},
			wantErr: "invalid link ID",
		},
		{
			name:    "wrong origin type",
			args:    []runtime.Arg{structuredProgram("prog", 42)},
			wantErr: `variable "$prog" is a program; expected link`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseLinkDetach(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantIDs, cmd.LinkIDs)
		})
	}
}

func TestParseLinkIDArg_RecordFieldRequiredForTypedHandle(t *testing.T) {
	t.Parallel()

	linkArg := structuredLink("link", 77).(runtime.StructuredValueArg)
	record := runtime.ValueFromRecord(map[string]runtime.Value{
		"link": linkArg.Value,
	})

	_, err := parseLinkIDArg(runtime.StructuredValueArg{
		Name:  "loaded",
		Value: record,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$loaded is structured but carries no link ID capability")

	field, err := record.LookupValue("loaded", "link")
	require.NoError(t, err)

	id, err := parseLinkIDArg(runtime.StructuredValueArg{
		Name:  "loaded.link",
		Value: field,
	})
	require.NoError(t, err)
	assert.Equal(t, bpfman.LinkID(77), id)
}

func TestParseGetProgram(t *testing.T) {
	t.Parallel()

	structuredVal, err := runtime.ValueFromStruct(bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: kernel.ProgramID(42)},
	})
	require.NoError(t, err)
	structuredVal = structuredVal.WithKind(semantics.OriginProgram)

	linkVal, err := runtime.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        bpfman.LinkID(10),
			ProgramID: kernel.ProgramID(42),
		},
	})
	require.NoError(t, err)
	linkVal = linkVal.WithKind(semantics.OriginLink)

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantID     kernel.ProgramID
		wantOutput string
		wantErr    string
	}{
		{
			name:       "numeric ID",
			args:       []runtime.Arg{word("123")},
			wantID:     123,
			wantOutput: "table",
		},
		{
			name:       "hex ID",
			args:       []runtime.Arg{word("0x1a")},
			wantID:     26,
			wantOutput: "table",
		},
		{
			name: "structured variable ref",
			args: []runtime.Arg{
				runtime.StructuredValueArg{Name: "prog", Value: structuredVal},
			},
			wantID:     42,
			wantOutput: "table",
		},
		{
			name: "scalar value arg",
			args: []runtime.Arg{
				runtime.ScalarValueArg{Text: "55"},
			},
			wantID:     55,
			wantOutput: "table",
		},
		{
			name: "with output flag",
			args: []runtime.Arg{
				word("100"),
				word("-o"),
				word("json"),
			},
			wantID:     100,
			wantOutput: "json",
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "requires a program ID",
		},
		{
			name: "duplicate -o flag",
			args: []runtime.Arg{
				word("100"),
				word("-o"), word("json"),
				word("-o"), word("wide"),
			},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("100"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "-o without value",
			args:    []runtime.Arg{word("100"), word("-o")},
			wantErr: "-o requires a value",
		},
		{
			name:    "unexpected positional",
			args:    []runtime.Arg{word("100"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name: "wrong origin type on structured ref",
			args: []runtime.Arg{
				runtime.StructuredValueArg{Name: "mylink", Value: linkVal},
			},
			wantErr: `variable "$mylink" is a link; expected program`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseGetProgram(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, cmd.ID)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseGetLink(t *testing.T) {
	t.Parallel()

	structuredVal, err := runtime.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{ID: bpfman.LinkID(77)},
	})
	require.NoError(t, err)
	structuredVal = structuredVal.WithKind(semantics.OriginLink)

	progVal, err := runtime.ValueFromStruct(bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: kernel.ProgramID(42)},
	})
	require.NoError(t, err)
	progVal = progVal.WithKind(semantics.OriginProgram)

	tests := []struct {
		name       string
		args       []runtime.Arg
		wantID     bpfman.LinkID
		wantOutput string
		wantErr    string
	}{
		{
			name:       "numeric ID",
			args:       []runtime.Arg{word("77")},
			wantID:     77,
			wantOutput: "table",
		},
		{
			name:       "hex ID",
			args:       []runtime.Arg{word("0x4d")},
			wantID:     77,
			wantOutput: "table",
		},
		{
			name: "structured variable ref",
			args: []runtime.Arg{
				runtime.StructuredValueArg{Name: "lnk", Value: structuredVal},
			},
			wantID:     77,
			wantOutput: "table",
		},
		{
			name:       "scalar value arg",
			args:       []runtime.Arg{runtime.ScalarValueArg{Text: "55"}},
			wantID:     55,
			wantOutput: "table",
		},
		{
			name: "with output flag",
			args: []runtime.Arg{
				word("77"),
				word("-o"),
				word("json"),
			},
			wantID:     77,
			wantOutput: "json",
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "requires a link ID",
		},
		{
			name: "duplicate -o flag",
			args: []runtime.Arg{
				word("77"),
				word("-o"), word("json"),
				word("-o"), word("wide"),
			},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("77"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "-o without value",
			args:    []runtime.Arg{word("77"), word("-o")},
			wantErr: "-o requires a value",
		},
		{
			name:    "unexpected positional",
			args:    []runtime.Arg{word("77"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name: "wrong origin type on structured ref",
			args: []runtime.Arg{
				runtime.StructuredValueArg{Name: "myprog", Value: progVal},
			},
			wantErr: `variable "$myprog" is a program; expected link`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseGetLink(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, cmd.ID)
			assert.Equal(t, tt.wantOutput, cmd.Output.Output.Value)
		})
	}
}

func TestParseDispatcherList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []runtime.Arg
		wantType    dispatcher.DispatcherType
		wantNsid    uint64
		wantIfindex uint32
		wantErr     string
	}{
		{
			name: "no flags",
		},
		{
			name:     "type filter",
			args:     []runtime.Arg{word("--type"), word("tc-ingress")},
			wantType: dispatcher.DispatcherTypeTCIngress,
		},
		{
			name:     "nsid filter",
			args:     []runtime.Arg{word("--nsid"), word("4026531840")},
			wantNsid: 4026531840,
		},
		{
			name:        "ifindex filter",
			args:        []runtime.Arg{word("--ifindex"), word("7")},
			wantIfindex: 7,
		},
		{
			name: "all filters combined",
			args: []runtime.Arg{
				word("--type"), word("xdp"),
				word("--nsid"), word("4026531840"),
				word("--ifindex"), word("7"),
			},
			wantType:    dispatcher.DispatcherTypeXDP,
			wantNsid:    4026531840,
			wantIfindex: 7,
		},
		{
			name:    "nsid missing value",
			args:    []runtime.Arg{word("--nsid")},
			wantErr: "--nsid requires a value",
		},
		{
			name:    "nsid not a number",
			args:    []runtime.Arg{word("--nsid"), word("root")},
			wantErr: "invalid nsid",
		},
		{
			name:    "ifindex missing value",
			args:    []runtime.Arg{word("--ifindex")},
			wantErr: "--ifindex requires a value",
		},
		{
			name:    "ifindex not a number",
			args:    []runtime.Arg{word("--ifindex"), word("eth0")},
			wantErr: "invalid ifindex",
		},
		{
			name:    "ifindex out of range",
			args:    []runtime.Arg{word("--ifindex"), word("4294967296")},
			wantErr: "invalid ifindex",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := parseDispatcherList(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, cmd.Type)
			assert.Equal(t, tt.wantNsid, cmd.Nsid)
			assert.Equal(t, tt.wantIfindex, cmd.Ifindex)
		})
	}
}
