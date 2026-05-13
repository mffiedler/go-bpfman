package bpfman_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

// TestLinkRecord_UnmarshalJSON_RoundTripsEveryLinkKind asserts
// that every LinkKind round-trips its Details field through
// json.Marshal -> json.Unmarshal, including the kretprobe /
// uretprobe variants which share a struct with their kprobe /
// uprobe partners and rely on the Retprobe bool to discriminate.
func TestLinkRecord_UnmarshalJSON_RoundTripsEveryLinkKind(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pin := bpfman.LinkPath("/run/bpfman/fs/example/link_0")

	cases := []struct {
		name    string
		details bpfman.LinkDetails
		kind    bpfman.LinkKind
	}{
		{
			"xdp",
			bpfman.XDPDetails{Interface: "eth0", Ifindex: 3, Priority: 50, Position: 0},
			bpfman.LinkKindXDP,
		},
		{
			"tc",
			bpfman.TCDetails{Interface: "eth0", Ifindex: 3, Direction: bpfman.TCDirectionIngress, Priority: 100, Position: 0, ProceedOn: []int32{0, 3, 30}},
			bpfman.LinkKindTC,
		},
		{
			"tcx",
			bpfman.TCXDetails{Interface: "eth0", Ifindex: 3, Direction: bpfman.TCDirectionEgress, Priority: 50, Position: 0},
			bpfman.LinkKindTCX,
		},
		{
			"tracepoint",
			bpfman.TracepointDetails{Group: "sched", Name: "sched_switch"},
			bpfman.LinkKindTracepoint,
		},
		{
			"kprobe",
			bpfman.KprobeDetails{FnName: "do_unlinkat"},
			bpfman.LinkKindKprobe,
		},
		{
			"kretprobe",
			bpfman.KprobeDetails{FnName: "do_unlinkat", Retprobe: true},
			bpfman.LinkKindKretprobe,
		},
		{
			"uprobe",
			bpfman.UprobeDetails{Target: "/bin/sh", FnName: "main"},
			bpfman.LinkKindUprobe,
		},
		{
			"uretprobe",
			bpfman.UprobeDetails{Target: "/bin/sh", FnName: "main", Retprobe: true},
			bpfman.LinkKindUretprobe,
		},
		{
			"fentry",
			bpfman.FentryDetails{FnName: "do_unlinkat"},
			bpfman.LinkKindFentry,
		},
		{
			"fexit",
			bpfman.FexitDetails{FnName: "do_unlinkat"},
			bpfman.LinkKindFexit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			original := bpfman.LinkRecord{
				ID:        kernel.LinkID(42),
				ProgramID: kernel.ProgramID(43),
				Kind:      tc.kind,
				PinPath:   &pin,
				Details:   tc.details,
				CreatedAt: fixed,
			}

			data, err := json.Marshal(original)
			require.NoError(t, err)

			var got bpfman.LinkRecord
			require.NoError(t, json.Unmarshal(data, &got))

			assert.Equal(t, original, got)
		})
	}
}

// TestLinkRecord_UnmarshalJSON_AcceptsNilDetails verifies that a
// record with no Details (e.g. a synthetic perf_event link)
// round-trips with Details left nil.
func TestLinkRecord_UnmarshalJSON_AcceptsNilDetails(t *testing.T) {
	t.Parallel()

	data := []byte(`{"id":1,"program_id":2,"kind":"kprobe","created_at":"2026-01-01T00:00:00Z"}`)
	var got bpfman.LinkRecord
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Nil(t, got.Details)
	assert.Equal(t, bpfman.LinkKindKprobe, got.Kind)
}

// TestLinkAttachKindDetailsType_CoversEveryAttachKind asserts
// that every attach subcommand keyword in LinkAttachKinds()
// resolves to a concrete reflect.Type and that an unknown
// keyword resolves to nil.
func TestLinkAttachKindDetailsType_CoversEveryAttachKind(t *testing.T) {
	t.Parallel()

	want := map[string]reflect.Type{
		"xdp":        reflect.TypeOf(bpfman.XDPDetails{}),
		"tc":         reflect.TypeOf(bpfman.TCDetails{}),
		"tcx":        reflect.TypeOf(bpfman.TCXDetails{}),
		"tracepoint": reflect.TypeOf(bpfman.TracepointDetails{}),
		"kprobe":     reflect.TypeOf(bpfman.KprobeDetails{}),
		"uprobe":     reflect.TypeOf(bpfman.UprobeDetails{}),
		"fentry":     reflect.TypeOf(bpfman.FentryDetails{}),
		"fexit":      reflect.TypeOf(bpfman.FexitDetails{}),
	}

	for _, kind := range bpfman.LinkAttachKinds() {
		assert.Equal(t, want[kind], bpfman.LinkAttachKindDetailsType(kind), "attach kind %q", kind)
	}

	assert.Nil(t, bpfman.LinkAttachKindDetailsType("nonexistent_kind"))
}
