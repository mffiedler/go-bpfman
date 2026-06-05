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

func structuredLink(name string, linkID kernel.LinkID) runtime.Arg {
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
			ID:        kernel.LinkID(10),
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
			args:       []runtime.Arg{word("-p"), word("/tmp/test.o")},
			wantPath:   "/tmp/test.o",
			wantOutput: "table",
		},
		{
			name:       "long path flag",
			args:       []runtime.Arg{word("--path"), word("/tmp/test.o")},
			wantPath:   "/tmp/test.o",
			wantOutput: "table",
		},
		{
			name: "all flags",
			args: []runtime.Arg{
				word("-p"), word("/tmp/test.o"),
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
				word("-p"), word("/tmp/test.o"),
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
				word("-p"), word("/tmp/test.o"),
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
			wantErr: "--path is required",
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "--path is required",
		},
		{
			name:    "path flag without value",
			args:    []runtime.Arg{word("-p")},
			wantErr: "requires a value",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("-p"), word("/tmp/test.o"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "unexpected positional",
			args:    []runtime.Arg{word("-p"), word("/tmp/test.o"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name:    "duplicate -o flag",
			args:    []runtime.Arg{word("-p"), word("/tmp/test.o"), word("-o"), word("json"), word("-o"), word("wide")},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "invalid program spec",
			args:    []runtime.Arg{word("-p"), word("/tmp/test.o"), word("--programs"), word("badspec")},
			wantErr: "invalid program spec",
		},
		{
			name:    "invalid metadata",
			args:    []runtime.Arg{word("-p"), word("/tmp/test.o"), word("-m"), word("noequalssign")},
			wantErr: "invalid format",
		},
		{
			name:    "invalid global data",
			args:    []runtime.Arg{word("-p"), word("/tmp/test.o"), word("-g"), word("BAD=notahex!")},
			wantErr: "invalid hex data",
		},
		{
			name:    "invalid map-owner-id",
			args:    []runtime.Arg{word("-p"), word("/tmp/test.o"), word("--map-owner-id"), word("abc")},
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
			args:           []runtime.Arg{word("-i"), word("quay.io/bpfman/xdp_pass:latest")},
			wantURL:        "quay.io/bpfman/xdp_pass:latest",
			wantPullPolicy: "IfNotPresent",
			wantOutput:     "table",
		},
		{
			name:           "long image-url flag",
			args:           []runtime.Arg{word("--image-url"), word("quay.io/bpfman/xdp_pass:latest")},
			wantURL:        "quay.io/bpfman/xdp_pass:latest",
			wantPullPolicy: "IfNotPresent",
			wantOutput:     "table",
		},
		{
			name: "all flags",
			args: []runtime.Arg{
				word("-i"), word("quay.io/bpfman/xdp_pass:latest"),
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
			wantErr: "--image-url is required",
		},
		{
			name:    "no arguments",
			args:    []runtime.Arg{},
			wantErr: "--image-url is required",
		},
		{
			name:    "image-url flag without value",
			args:    []runtime.Arg{word("-i")},
			wantErr: "requires a value",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("-i"), word("img"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "unexpected positional",
			args:    []runtime.Arg{word("-i"), word("img"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name:    "duplicate -o flag",
			args:    []runtime.Arg{word("-i"), word("img"), word("-o"), word("json"), word("-o"), word("wide")},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "invalid program spec",
			args:    []runtime.Arg{word("-i"), word("img"), word("--programs"), word("bad")},
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
			name: "metadata flag rejected",
			args: []runtime.Arg{
				word("tracepoint"), word("-m"), word("key=val"),
				word("42"), word("sched/sched_switch"),
			},
			wantErr: "not supported for attach",
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
			args:       []runtime.Arg{word("kprobe"), word("-f"), word("do_unlinkat"), word("42")},
			wantOutput: "table",
		},
		{
			name: "with offset",
			args: []runtime.Arg{
				word("kprobe"), word("-f"), word("do_unlinkat"),
				word("--offset"), word("16"), word("42"),
			},
			wantOutput: "table",
		},
		{
			name:    "missing fn-name",
			args:    []runtime.Arg{word("kprobe"), word("42")},
			wantErr: "--fn-name is required",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("kprobe"), word("-f"), word("do_unlinkat")},
			wantErr: "requires a program ID",
		},
		{
			name:    "invalid offset",
			args:    []runtime.Arg{word("kprobe"), word("-f"), word("do_unlinkat"), word("--offset"), word("abc"), word("42")},
			wantErr: "invalid offset",
		},
		{
			name:    "metadata flag rejected",
			args:    []runtime.Arg{word("kprobe"), word("-f"), word("do_unlinkat"), word("-m"), word("k=v"), word("42")},
			wantErr: "not supported for attach",
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
			args:       []runtime.Arg{word("uprobe"), word("--target"), word("/usr/lib/libc.so.6"), word("42")},
			wantOutput: "table",
		},
		{
			name: "all optional flags",
			args: []runtime.Arg{
				word("uprobe"), word("--target"), word("/usr/lib/libc.so.6"),
				word("-f"), word("malloc"), word("--offset"), word("8"),
				word("--container-pid"), word("1234"),
				word("-o"), word("json"), word("42"),
			},
			wantOutput: "json",
		},
		{
			name:    "missing target",
			args:    []runtime.Arg{word("uprobe"), word("42")},
			wantErr: "--target is required",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("uprobe"), word("--target"), word("/bin/foo")},
			wantErr: "requires a program ID",
		},
		{
			name:    "invalid container-pid",
			args:    []runtime.Arg{word("uprobe"), word("--target"), word("/bin/foo"), word("--container-pid"), word("abc"), word("42")},
			wantErr: "invalid container-pid",
		},
		{
			name:    "metadata flag rejected",
			args:    []runtime.Arg{word("uprobe"), word("--target"), word("/bin/foo"), word("-m"), word("k=v"), word("42")},
			wantErr: "not supported for attach",
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
		{
			name:    "metadata flag rejected",
			args:    []runtime.Arg{word("fentry"), word("-m"), word("k=v"), word("42")},
			wantErr: "not supported for attach",
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
		{
			name:    "metadata flag rejected",
			args:    []runtime.Arg{word("fexit"), word("-m"), word("k=v"), word("42")},
			wantErr: "not supported for attach",
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
			wantErr: "--iface is required",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("xdp"), word("-i"), word("lo")},
			wantErr: "requires a program ID",
		},
		{
			name:    "unknown flag",
			args:    []runtime.Arg{word("xdp"), word("-i"), word("lo"), word("--verbose"), word("42")},
			wantErr: "unknown flag",
		},
		{
			name:    "invalid priority",
			args:    []runtime.Arg{word("xdp"), word("-i"), word("lo"), word("-p"), word("abc"), word("42")},
			wantErr: "invalid priority",
		},
		{
			name:    "metadata flag rejected",
			args:    []runtime.Arg{word("xdp"), word("-i"), word("lo"), word("-m"), word("k=v"), word("42")},
			wantErr: "not supported for attach",
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
			args:    []runtime.Arg{word("tc"), word("-d"), word("ingress"), word("42")},
			wantErr: "--iface is required",
		},
		{
			name:    "missing direction",
			args:    []runtime.Arg{word("tc"), word("-i"), word("lo"), word("42")},
			wantErr: "--direction is required",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("tc"), word("-i"), word("lo"), word("-d"), word("ingress")},
			wantErr: "requires a program ID",
		},
		{
			name:    "metadata flag rejected",
			args:    []runtime.Arg{word("tc"), word("-i"), word("lo"), word("-d"), word("ingress"), word("-m"), word("k=v"), word("42")},
			wantErr: "not supported for attach",
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
			args:    []runtime.Arg{word("tcx"), word("-d"), word("ingress"), word("42")},
			wantErr: "--iface is required",
		},
		{
			name:    "missing direction",
			args:    []runtime.Arg{word("tcx"), word("-i"), word("lo"), word("42")},
			wantErr: "--direction is required",
		},
		{
			name:    "missing program ID",
			args:    []runtime.Arg{word("tcx"), word("-i"), word("lo"), word("-d"), word("ingress")},
			wantErr: "requires a program ID",
		},
		{
			name:    "metadata flag rejected",
			args:    []runtime.Arg{word("tcx"), word("-i"), word("lo"), word("-d"), word("ingress"), word("-m"), word("k=v"), word("42")},
			wantErr: "not supported for attach",
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

func TestParseLinkDetach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []runtime.Arg
		wantIDs []kernel.LinkID
		wantErr string
	}{
		{
			name:    "single numeric ID",
			args:    []runtime.Arg{word("42")},
			wantIDs: []kernel.LinkID{42},
		},
		{
			name:    "multiple numeric IDs",
			args:    []runtime.Arg{word("10"), word("20"), word("30")},
			wantIDs: []kernel.LinkID{10, 20, 30},
		},
		{
			name:    "structured variable ref",
			args:    []runtime.Arg{structuredLink("lnk", 77)},
			wantIDs: []kernel.LinkID{77},
		},
		{
			name: "mixed numeric and structured",
			args: []runtime.Arg{
				word("5"),
				structuredLink("lnk", 99),
			},
			wantIDs: []kernel.LinkID{5, 99},
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

func TestParseGetProgram(t *testing.T) {
	t.Parallel()

	structuredVal, err := runtime.ValueFromStruct(bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: kernel.ProgramID(42)},
	})
	require.NoError(t, err)
	structuredVal = structuredVal.WithKind(semantics.OriginProgram)

	linkVal, err := runtime.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        kernel.LinkID(10),
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
		Record: bpfman.LinkRecord{ID: kernel.LinkID(77)},
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
		wantID     kernel.LinkID
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
