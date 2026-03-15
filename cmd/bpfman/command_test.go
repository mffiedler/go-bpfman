package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/replang"
)

// structured helper builds a program-origin structured value with a
// given program ID for use in attach/detach parse tests.
func structuredProgram(name string, progID kernel.ProgramID) replang.Arg {
	val, err := replang.ValueFromStruct(bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: progID},
	})
	if err != nil {
		panic(err)
	}
	return replang.StructuredValueArg{Name: name, Value: val}
}

func structuredLink(name string, linkID kernel.LinkID) replang.Arg {
	val, err := replang.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{ID: linkID},
	})
	if err != nil {
		panic(err)
	}
	return replang.StructuredValueArg{Name: name, Value: val}
}

func word(s string) replang.Arg { return replang.WordArg{Text: s} }

func TestParseShowProgram(t *testing.T) {
	structuredVal, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":42}}`))
	require.NoError(t, err)

	linkVal, err := replang.ValueFromStruct(bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        kernel.LinkID(10),
			ProgramID: kernel.ProgramID(42),
		},
	})
	require.NoError(t, err)

	tests := []struct {
		name       string
		args       []replang.Arg
		wantID     kernel.ProgramID
		wantView   string
		wantOutput string
		wantErr    string
	}{
		{
			name:       "numeric ID only",
			args:       []replang.Arg{replang.WordArg{Text: "123"}},
			wantID:     123,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name:       "hex ID",
			args:       []replang.Arg{replang.WordArg{Text: "0x1a"}},
			wantID:     26,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "structured variable ref",
			args: []replang.Arg{
				replang.StructuredValueArg{Name: "prog", Value: structuredVal},
			},
			wantID:     42,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "scalar value arg",
			args: []replang.Arg{
				replang.ScalarValueArg{Text: "55"},
			},
			wantID:     55,
			wantView:   "summary",
			wantOutput: "table",
		},
		{
			name: "with view argument",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "links"},
			},
			wantID:     100,
			wantView:   "links",
			wantOutput: "table",
		},
		{
			name: "with output flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "json"},
			},
			wantID:     100,
			wantView:   "summary",
			wantOutput: "json",
		},
		{
			name: "view and output flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "maps"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "wide"},
			},
			wantID:     100,
			wantView:   "maps",
			wantOutput: "wide",
		},
		{
			name: "output flag before view",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "json"},
				replang.WordArg{Text: "paths"},
			},
			wantID:     100,
			wantView:   "paths",
			wantOutput: "json",
		},
		{
			name: "structured ref with view",
			args: []replang.Arg{
				replang.StructuredValueArg{Name: "prog", Value: structuredVal},
				replang.WordArg{Text: "maps"},
			},
			wantID:     42,
			wantView:   "maps",
			wantOutput: "table",
		},
		{
			name:    "no arguments",
			args:    []replang.Arg{},
			wantErr: "requires a program ID",
		},
		{
			name: "duplicate -o flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "json"},
				replang.WordArg{Text: "-o"},
				replang.WordArg{Text: "wide"},
			},
			wantErr: "duplicate -o flag",
		},
		{
			name: "unknown flag",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "--verbose"},
			},
			wantErr: "unknown flag",
		},
		{
			name: "unknown view",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "nonsense"},
			},
			wantErr: "unknown view",
		},
		{
			name: "-o without value",
			args: []replang.Arg{
				replang.WordArg{Text: "100"},
				replang.WordArg{Text: "-o"},
			},
			wantErr: "-o requires a value",
		},
		{
			name: "wrong origin type on structured ref",
			args: []replang.Arg{
				replang.StructuredValueArg{Name: "mylink", Value: linkVal},
			},
			wantErr: "not a program",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name        string
		args        []replang.Arg
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
			args:       []replang.Arg{word("-p"), word("/tmp/test.o")},
			wantPath:   "/tmp/test.o",
			wantOutput: "table",
		},
		{
			name:       "long path flag",
			args:       []replang.Arg{word("--path"), word("/tmp/test.o")},
			wantPath:   "/tmp/test.o",
			wantOutput: "table",
		},
		{
			name: "all flags",
			args: []replang.Arg{
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
			args: []replang.Arg{
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
			args: []replang.Arg{
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
			args:    []replang.Arg{word("-m"), word("a=1")},
			wantErr: "--path is required",
		},
		{
			name:    "no arguments",
			args:    []replang.Arg{},
			wantErr: "--path is required",
		},
		{
			name:    "path flag without value",
			args:    []replang.Arg{word("-p")},
			wantErr: "requires a value",
		},
		{
			name:    "unknown flag",
			args:    []replang.Arg{word("-p"), word("/tmp/test.o"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "unexpected positional",
			args:    []replang.Arg{word("-p"), word("/tmp/test.o"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name:    "duplicate -o flag",
			args:    []replang.Arg{word("-p"), word("/tmp/test.o"), word("-o"), word("json"), word("-o"), word("wide")},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "invalid program spec",
			args:    []replang.Arg{word("-p"), word("/tmp/test.o"), word("--programs"), word("badspec")},
			wantErr: "invalid program spec",
		},
		{
			name:    "invalid metadata",
			args:    []replang.Arg{word("-p"), word("/tmp/test.o"), word("-m"), word("noequalssign")},
			wantErr: "invalid format",
		},
		{
			name:    "invalid global data",
			args:    []replang.Arg{word("-p"), word("/tmp/test.o"), word("-g"), word("BAD=notahex!")},
			wantErr: "invalid hex data",
		},
		{
			name:    "invalid map-owner-id",
			args:    []replang.Arg{word("-p"), word("/tmp/test.o"), word("--map-owner-id"), word("abc")},
			wantErr: "invalid program ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name           string
		args           []replang.Arg
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
			args:           []replang.Arg{word("-i"), word("quay.io/bpfman/xdp_pass:latest")},
			wantURL:        "quay.io/bpfman/xdp_pass:latest",
			wantPullPolicy: "IfNotPresent",
			wantOutput:     "table",
		},
		{
			name:           "long image-url flag",
			args:           []replang.Arg{word("--image-url"), word("quay.io/bpfman/xdp_pass:latest")},
			wantURL:        "quay.io/bpfman/xdp_pass:latest",
			wantPullPolicy: "IfNotPresent",
			wantOutput:     "table",
		},
		{
			name: "all flags",
			args: []replang.Arg{
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
			args:    []replang.Arg{word("--programs"), word("xdp:xdp_pass")},
			wantErr: "--image-url is required",
		},
		{
			name:    "no arguments",
			args:    []replang.Arg{},
			wantErr: "--image-url is required",
		},
		{
			name:    "image-url flag without value",
			args:    []replang.Arg{word("-i")},
			wantErr: "requires a value",
		},
		{
			name:    "unknown flag",
			args:    []replang.Arg{word("-i"), word("img"), word("--verbose")},
			wantErr: "unknown flag",
		},
		{
			name:    "unexpected positional",
			args:    []replang.Arg{word("-i"), word("img"), word("extra")},
			wantErr: "unexpected argument",
		},
		{
			name:    "duplicate -o flag",
			args:    []replang.Arg{word("-i"), word("img"), word("-o"), word("json"), word("-o"), word("wide")},
			wantErr: "duplicate -o flag",
		},
		{
			name:    "invalid program spec",
			args:    []replang.Arg{word("-i"), word("img"), word("--programs"), word("bad")},
			wantErr: "invalid program spec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name    string
		args    []replang.Arg
		wantErr string
	}{
		{
			name:    "no arguments",
			args:    []replang.Arg{},
			wantErr: "requires a type",
		},
		{
			name:    "unknown type",
			args:    []replang.Arg{word("rawsock"), word("42")},
			wantErr: "unknown attach type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkAttachTracepoint(t *testing.T) {
	tests := []struct {
		name       string
		args       []replang.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "minimal",
			args:       []replang.Arg{word("tracepoint"), word("-t"), word("sched/sched_switch"), word("42")},
			wantOutput: "table",
		},
		{
			name: "with output flag",
			args: []replang.Arg{
				word("tracepoint"), word("-t"), word("sched/sched_switch"),
				word("-o"), word("json"), word("42"),
			},
			wantOutput: "json",
		},
		{
			name: "structured program ref",
			args: []replang.Arg{
				word("tracepoint"), word("-t"), word("sched/sched_switch"),
				structuredProgram("prog", 99),
			},
			wantOutput: "table",
		},
		{
			name:    "missing tracepoint flag",
			args:    []replang.Arg{word("tracepoint"), word("42")},
			wantErr: "--tracepoint is required",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("tracepoint"), word("-t"), word("sched/sched_switch")},
			wantErr: "requires a program ID",
		},
		{
			name:    "bad tracepoint format",
			args:    []replang.Arg{word("tracepoint"), word("-t"), word("noslash"), word("42")},
			wantErr: "group/name",
		},
		{
			name:    "unknown flag",
			args:    []replang.Arg{word("tracepoint"), word("-t"), word("sched/sched_switch"), word("--verbose"), word("42")},
			wantErr: "unknown flag",
		},
		{
			name: "metadata silently consumed",
			args: []replang.Arg{
				word("tracepoint"), word("-t"), word("sched/sched_switch"),
				word("-m"), word("key=val"), word("42"),
			},
			wantOutput: "table",
		},
		{
			name:    "duplicate -o flag",
			args:    []replang.Arg{word("tracepoint"), word("-t"), word("sched/sched_switch"), word("-o"), word("json"), word("-o"), word("wide"), word("42")},
			wantErr: "duplicate -o flag",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name       string
		args       []replang.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "minimal",
			args:       []replang.Arg{word("kprobe"), word("-f"), word("do_unlinkat"), word("42")},
			wantOutput: "table",
		},
		{
			name: "with offset",
			args: []replang.Arg{
				word("kprobe"), word("-f"), word("do_unlinkat"),
				word("--offset"), word("16"), word("42"),
			},
			wantOutput: "table",
		},
		{
			name:    "missing fn-name",
			args:    []replang.Arg{word("kprobe"), word("42")},
			wantErr: "--fn-name is required",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("kprobe"), word("-f"), word("do_unlinkat")},
			wantErr: "requires a program ID",
		},
		{
			name:    "invalid offset",
			args:    []replang.Arg{word("kprobe"), word("-f"), word("do_unlinkat"), word("--offset"), word("abc"), word("42")},
			wantErr: "invalid offset",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name       string
		args       []replang.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "minimal",
			args:       []replang.Arg{word("uprobe"), word("--target"), word("/usr/lib/libc.so.6"), word("42")},
			wantOutput: "table",
		},
		{
			name: "all optional flags",
			args: []replang.Arg{
				word("uprobe"), word("--target"), word("/usr/lib/libc.so.6"),
				word("-f"), word("malloc"), word("--offset"), word("8"),
				word("--container-pid"), word("1234"),
				word("-o"), word("json"), word("42"),
			},
			wantOutput: "json",
		},
		{
			name:    "missing target",
			args:    []replang.Arg{word("uprobe"), word("42")},
			wantErr: "--target is required",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("uprobe"), word("--target"), word("/bin/foo")},
			wantErr: "requires a program ID",
		},
		{
			name:    "invalid container-pid",
			args:    []replang.Arg{word("uprobe"), word("--target"), word("/bin/foo"), word("--container-pid"), word("abc"), word("42")},
			wantErr: "invalid container-pid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name       string
		args       []replang.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "program ID only",
			args:       []replang.Arg{word("fentry"), word("42")},
			wantOutput: "table",
		},
		{
			name:       "with output flag",
			args:       []replang.Arg{word("fentry"), word("-o"), word("json"), word("42")},
			wantOutput: "json",
		},
		{
			name: "structured program ref",
			args: []replang.Arg{
				word("fentry"), structuredProgram("prog", 55),
			},
			wantOutput: "table",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("fentry")},
			wantErr: "requires a program ID",
		},
		{
			name:    "unknown flag",
			args:    []replang.Arg{word("fentry"), word("--verbose"), word("42")},
			wantErr: "unknown flag",
		},
		{
			name:    "wrong origin type",
			args:    []replang.Arg{word("fentry"), structuredLink("lnk", 10)},
			wantErr: "not a program",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name       string
		args       []replang.Arg
		wantOutput string
		wantErr    string
	}{
		{
			name:       "program ID only",
			args:       []replang.Arg{word("fexit"), word("42")},
			wantOutput: "table",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("fexit")},
			wantErr: "requires a program ID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name    string
		args    []replang.Arg
		wantErr string
	}{
		{
			name:    "missing iface",
			args:    []replang.Arg{word("xdp"), word("42")},
			wantErr: "--iface is required",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("xdp"), word("-i"), word("lo")},
			wantErr: "requires a program ID",
		},
		{
			name:    "unknown flag",
			args:    []replang.Arg{word("xdp"), word("-i"), word("lo"), word("--verbose"), word("42")},
			wantErr: "unknown flag",
		},
		{
			name:    "invalid priority",
			args:    []replang.Arg{word("xdp"), word("-i"), word("lo"), word("-p"), word("abc"), word("42")},
			wantErr: "invalid priority",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkAttachTC_Errors(t *testing.T) {
	tests := []struct {
		name    string
		args    []replang.Arg
		wantErr string
	}{
		{
			name:    "missing iface",
			args:    []replang.Arg{word("tc"), word("-d"), word("ingress"), word("42")},
			wantErr: "--iface is required",
		},
		{
			name:    "missing direction",
			args:    []replang.Arg{word("tc"), word("-i"), word("lo"), word("42")},
			wantErr: "--direction is required",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("tc"), word("-i"), word("lo"), word("-d"), word("ingress")},
			wantErr: "requires a program ID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkAttachTCX_Errors(t *testing.T) {
	tests := []struct {
		name    string
		args    []replang.Arg
		wantErr string
	}{
		{
			name:    "missing iface",
			args:    []replang.Arg{word("tcx"), word("-d"), word("ingress"), word("42")},
			wantErr: "--iface is required",
		},
		{
			name:    "missing direction",
			args:    []replang.Arg{word("tcx"), word("-i"), word("lo"), word("42")},
			wantErr: "--direction is required",
		},
		{
			name:    "missing program ID",
			args:    []replang.Arg{word("tcx"), word("-i"), word("lo"), word("-d"), word("ingress")},
			wantErr: "requires a program ID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseLinkAttach(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseLinkDetach(t *testing.T) {
	tests := []struct {
		name    string
		args    []replang.Arg
		wantIDs []kernel.LinkID
		wantErr string
	}{
		{
			name:    "single numeric ID",
			args:    []replang.Arg{word("42")},
			wantIDs: []kernel.LinkID{42},
		},
		{
			name:    "multiple numeric IDs",
			args:    []replang.Arg{word("10"), word("20"), word("30")},
			wantIDs: []kernel.LinkID{10, 20, 30},
		},
		{
			name:    "structured variable ref",
			args:    []replang.Arg{structuredLink("lnk", 77)},
			wantIDs: []kernel.LinkID{77},
		},
		{
			name: "mixed numeric and structured",
			args: []replang.Arg{
				word("5"),
				structuredLink("lnk", 99),
			},
			wantIDs: []kernel.LinkID{5, 99},
		},
		{
			name:    "no arguments",
			args:    []replang.Arg{},
			wantErr: "requires at least one link ID",
		},
		{
			name:    "invalid ID",
			args:    []replang.Arg{word("abc")},
			wantErr: "invalid link ID",
		},
		{
			name:    "wrong origin type",
			args:    []replang.Arg{structuredProgram("prog", 42)},
			wantErr: "not a link",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
