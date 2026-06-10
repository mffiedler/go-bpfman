package cliformat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

func sampleDispatcherSummaries() []platform.DispatcherSummary {
	linkID := kernel.LinkID(9)
	return []platform.DispatcherSummary{
		{
			Key:      dispatcher.NewKey(dispatcher.DispatcherTypeXDP, 4026531840, 7),
			Revision: 3,
			Runtime: platform.DispatcherRuntime{
				ProgramID:    101,
				KernelLinkID: &linkID,
			},
			MemberCount: 2,
		},
		{
			Key:      dispatcher.NewKey(dispatcher.DispatcherTypeTCIngress, 4026531999, 12),
			Revision: 1,
			Runtime: platform.DispatcherRuntime{
				ProgramID: 202,
				NetnsPath: "/var/run/netns/blue",
			},
			MemberCount: 1,
		},
	}
}

func TestFormatDispatcherListJSON_WrapsInResult(t *testing.T) {
	t.Parallel()

	output, err := FormatDispatcherList(sampleDispatcherSummaries(), &OutputFlags{Output: OutputValue{Value: "json"}})
	if err != nil {
		t.Fatalf("FormatDispatcherList() error = %v", err)
	}
	var result platform.DispatcherListResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not a DispatcherListResult: %v\n%s", err, output)
	}
	if len(result.Dispatchers) != 2 {
		t.Errorf("expected 2 dispatchers, got %d", len(result.Dispatchers))
	}
}

func TestFormatDispatcherListJSON_EmptyListYieldsEmptyResult(t *testing.T) {
	t.Parallel()

	output, err := FormatDispatcherList(nil, &OutputFlags{Output: OutputValue{Value: "json"}})
	if err != nil {
		t.Fatalf("FormatDispatcherList() error = %v", err)
	}
	if !strings.Contains(output, `"dispatchers": []`) {
		t.Errorf("empty list should marshal as an empty dispatchers array: %s", output)
	}
}

func TestFormatDispatcherListTable_SingleListingCarriesNetns(t *testing.T) {
	t.Parallel()

	table, err := FormatDispatcherList(sampleDispatcherSummaries(), &OutputFlags{Output: OutputValue{Value: "table"}})
	if err != nil {
		t.Fatalf("FormatDispatcherList(table) error = %v", err)
	}
	for _, want := range []string{"NETNS", "/var/run/netns/blue"} {
		if !strings.Contains(table, want) {
			t.Errorf("table missing %q:\n%s", want, table)
		}
	}

	wide, err := FormatDispatcherList(sampleDispatcherSummaries(), &OutputFlags{Output: OutputValue{Value: "wide"}})
	if err != nil {
		t.Fatalf("FormatDispatcherList(wide) error = %v", err)
	}
	if wide != table {
		t.Error("wide must alias the single table listing")
	}
}
