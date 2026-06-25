package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTimelineBuildsRunningAndPausedIntervals(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestRoot"}`,
		`{"Time":"2026-05-28T12:00:00.100Z","Action":"run","Package":"example.test","Test":"TestRoot/TestParallel"}`,
		`{"Time":"2026-05-28T12:00:00.200Z","Action":"pause","Package":"example.test","Test":"TestRoot/TestParallel"}`,
		`{"Time":"2026-05-28T12:00:00.500Z","Action":"cont","Package":"example.test","Test":"TestRoot/TestParallel"}`,
		`{"Time":"2026-05-28T12:00:00.900Z","Action":"pass","Package":"example.test","Test":"TestRoot/TestParallel","Elapsed":0.8}`,
		`{"Time":"2026-05-28T12:00:01Z","Action":"pass","Package":"example.test","Test":"TestRoot","Elapsed":1.0}`,
	}, "\n"))

	tl, err := parseTimeline(input, "parallel tests")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	if got, want := len(tl.Rows), 2; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}

	var parallel *testRow
	for _, row := range tl.Rows {
		if row.Test == "TestRoot/TestParallel" {
			parallel = row
			break
		}
	}
	if parallel == nil {
		t.Fatal("parallel subtest row not found")
	}
	if got, want := parallel.Terminal, "pass"; got != want {
		t.Fatalf("terminal: got %q want %q", got, want)
	}
	if got, want := len(parallel.Intervals), 3; got != want {
		t.Fatalf("interval count: got %d want %d", got, want)
	}
	if !parallel.Paused {
		t.Fatal("parallel subtest should be marked paused")
	}
	states := []string{parallel.Intervals[0].State, parallel.Intervals[1].State, parallel.Intervals[2].State}
	if got, want := strings.Join(states, ","), "running,paused,running"; got != want {
		t.Fatalf("states: got %q want %q", got, want)
	}
}

func TestRenderTraceIncludesRowsAndStates(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestFast"}`,
		`{"Time":"2026-05-28T12:00:00.010Z","Action":"pass","Package":"example.test","Test":"TestFast","Elapsed":0.01}`,
		`{"Time":"2026-05-28T12:00:00.020Z","Action":"run","Package":"example.test","Test":"TestSlow"}`,
		`{"Time":"2026-05-28T12:00:00.030Z","Action":"fail","Package":"example.test","Test":"TestSlow","Elapsed":0.01}`,
	}, "\n"))

	tl, err := parseTimeline(input, "sample")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	var out bytes.Buffer
	if err := renderTrace(&out, tl); err != nil {
		t.Fatalf("renderTrace: %v", err)
	}

	trace := out.String()
	for _, want := range []string{
		"sample",
		"example.test/TestFast",
		"example.test/TestSlow",
		`"name": "lane"`,
		`"cat": "running"`,
		`"cat": "failed"`,
		`"ph": "X"`,
	} {
		if !strings.Contains(trace, want) {
			t.Fatalf("rendered trace missing %q:\n%s", want, trace)
		}
	}
}

func TestAssignLanesReusesNonOverlappingLane(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestA"}`,
		`{"Time":"2026-05-28T12:00:00.500Z","Action":"run","Package":"example.test","Test":"TestB"}`,
		`{"Time":"2026-05-28T12:00:01Z","Action":"pass","Package":"example.test","Test":"TestA","Elapsed":1.0}`,
		`{"Time":"2026-05-28T12:00:01.200Z","Action":"pass","Package":"example.test","Test":"TestB","Elapsed":0.7}`,
		`{"Time":"2026-05-28T12:00:01.200Z","Action":"run","Package":"example.test","Test":"TestC"}`,
		`{"Time":"2026-05-28T12:00:01.300Z","Action":"pass","Package":"example.test","Test":"TestC","Elapsed":0.1}`,
	}, "\n"))

	tl, err := parseTimeline(input, "lanes")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	scheduled, laneCount := assignLanes(tl.Rows)
	if got, want := laneCount, 2; got != want {
		t.Fatalf("lane count: got %d want %d", got, want)
	}

	lanes := map[string][]int{}
	for _, item := range scheduled {
		lanes[item.Row.Test] = append(lanes[item.Row.Test], item.Lane)
	}
	if got, want := lanes["TestA"][0], 0; got != want {
		t.Fatalf("TestA lane: got %d want %d", got, want)
	}
	if got, want := lanes["TestB"][0], 1; got != want {
		t.Fatalf("TestB lane: got %d want %d", got, want)
	}
	if got, want := lanes["TestC"][0], 0; got != want {
		t.Fatalf("TestC lane: got %d want %d", got, want)
	}
}

func TestAssignLanesIgnoresPausedIntervals(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestA"}`,
		`{"Time":"2026-05-28T12:00:00.010Z","Action":"pause","Package":"example.test","Test":"TestA"}`,
		`{"Time":"2026-05-28T12:00:00.020Z","Action":"run","Package":"example.test","Test":"TestB"}`,
		`{"Time":"2026-05-28T12:00:00.030Z","Action":"pass","Package":"example.test","Test":"TestB","Elapsed":0.01}`,
		`{"Time":"2026-05-28T12:00:01Z","Action":"cont","Package":"example.test","Test":"TestA"}`,
		`{"Time":"2026-05-28T12:00:01.010Z","Action":"pass","Package":"example.test","Test":"TestA","Elapsed":1.01}`,
	}, "\n"))

	tl, err := parseTimeline(input, "paused")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	scheduled, laneCount := assignLanes(tl.Rows)
	if got, want := laneCount, 1; got != want {
		t.Fatalf("lane count: got %d want %d", got, want)
	}
	if got, want := len(scheduled), 2; got != want {
		t.Fatalf("scheduled running intervals: got %d want %d", got, want)
	}
	for _, item := range scheduled {
		if item.Interval.State != "running" {
			t.Fatalf("scheduled non-running interval: %s", item.Interval.State)
		}
	}
}

func TestAssignLanesDropsParallelRegistrationInterval(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestParallel"}`,
		`{"Time":"2026-05-28T12:00:00.001Z","Action":"pause","Package":"example.test","Test":"TestParallel"}`,
		`{"Time":"2026-05-28T12:00:01Z","Action":"cont","Package":"example.test","Test":"TestParallel"}`,
		`{"Time":"2026-05-28T12:00:01.500Z","Action":"pass","Package":"example.test","Test":"TestParallel","Elapsed":0.5}`,
	}, "\n"))

	tl, err := parseTimeline(input, "registration")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	scheduled, laneCount := assignLanes(tl.Rows)
	if got, want := laneCount, 1; got != want {
		t.Fatalf("lane count: got %d want %d", got, want)
	}
	if got, want := len(scheduled), 1; got != want {
		t.Fatalf("scheduled running intervals: got %d want %d", got, want)
	}
	if got, want := scheduled[0].Interval.Start.Sub(tl.Start).Seconds(), 1.0; got != want {
		t.Fatalf("scheduled start offset: got %v want %v", got, want)
	}
}

func TestTerminalEventUsesElapsedWhenSummaryIsDelayed(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestRoot/TestParallel"}`,
		`{"Time":"2026-05-28T12:00:00.001Z","Action":"pause","Package":"example.test","Test":"TestRoot/TestParallel"}`,
		`{"Time":"2026-05-28T12:00:01Z","Action":"cont","Package":"example.test","Test":"TestRoot/TestParallel"}`,
		`{"Time":"2026-05-28T12:00:10Z","Action":"pass","Package":"example.test","Test":"TestRoot/TestParallel","Elapsed":0.25}`,
	}, "\n"))

	tl, err := parseTimeline(input, "delayed summary")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	scheduled, laneCount := assignLanes(tl.Rows)
	if got, want := laneCount, 1; got != want {
		t.Fatalf("lane count: got %d want %d", got, want)
	}
	if got, want := len(scheduled), 1; got != want {
		t.Fatalf("scheduled running intervals: got %d want %d", got, want)
	}
	if got, want := scheduled[0].Dur, int64(250000); got != want {
		t.Fatalf("duration: got %d want %d", got, want)
	}
}

func TestApplyMarkersOverridesRoundedElapsedIntervals(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestRoot/scripts/TestA.bpfman"}`,
		`{"Time":"2026-05-28T12:00:00.001Z","Action":"pause","Package":"example.test","Test":"TestRoot/scripts/TestA.bpfman"}`,
		`{"Time":"2026-05-28T12:00:01Z","Action":"cont","Package":"example.test","Test":"TestRoot/scripts/TestA.bpfman"}`,
		`{"Time":"2026-05-28T12:00:10Z","Action":"pass","Package":"example.test","Test":"TestRoot/scripts/TestA.bpfman","Elapsed":1.08}`,
	}, "\n"))

	tl, err := parseTimeline(input, "markers")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	markers := strings.Join([]string{
		`{"Time":"2026-05-28T12:00:01Z","Action":"script_start","Package":"example.test","Test":"TestRoot/scripts/TestA.bpfman"}`,
		`{"Time":"2026-05-28T12:00:01.076Z","Action":"script_end","Package":"example.test","Test":"TestRoot/scripts/TestA.bpfman"}`,
	}, "\n")
	path := filepath.Join(t.TempDir(), "markers.jsonl")
	if err := os.WriteFile(path, []byte(markers), 0o600); err != nil {
		t.Fatalf("write markers: %v", err)
	}

	if err := applyMarkers(tl, path); err != nil {
		t.Fatalf("applyMarkers: %v", err)
	}

	scheduled, laneCount := assignLanes(tl.Rows)
	if got, want := laneCount, 1; got != want {
		t.Fatalf("lane count: got %d want %d", got, want)
	}
	if got, want := scheduled[0].Dur, int64(76000); got != want {
		t.Fatalf("duration: got %d want %d", got, want)
	}
}

func TestRenderTraceNamesSyntheticLanes(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestA"}`,
		`{"Time":"2026-05-28T12:00:00.100Z","Action":"run","Package":"example.test","Test":"TestB"}`,
		`{"Time":"2026-05-28T12:00:00.200Z","Action":"pass","Package":"example.test","Test":"TestA","Elapsed":0.2}`,
		`{"Time":"2026-05-28T12:00:00.300Z","Action":"pass","Package":"example.test","Test":"TestB","Elapsed":0.2}`,
	}, "\n"))

	tl, err := parseTimeline(input, "lanes")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	var out bytes.Buffer
	if err := renderTrace(&out, tl); err != nil {
		t.Fatalf("renderTrace: %v", err)
	}

	var trace traceFile
	if err := json.Unmarshal(out.Bytes(), &trace); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}

	var laneNames []string
	for _, ev := range trace.TraceEvents {
		if ev.Name == "thread_name" {
			laneNames = append(laneNames, ev.Args["name"].(string))
		}
	}
	if got, want := strings.Join(laneNames, ","), "runner busy,lane,lane"; got != want {
		t.Fatalf("lane names: got %q want %q", got, want)
	}
}

func TestRenderTraceAddsDerivedRunnerBusyTrack(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"Time":"2026-05-28T12:00:00Z","Action":"run","Package":"example.test","Test":"TestA"}`,
		`{"Time":"2026-05-28T12:00:00.100Z","Action":"run","Package":"example.test","Test":"TestB"}`,
		`{"Time":"2026-05-28T12:00:00.200Z","Action":"pass","Package":"example.test","Test":"TestA","Elapsed":0.2}`,
		`{"Time":"2026-05-28T12:00:00.300Z","Action":"pass","Package":"example.test","Test":"TestB","Elapsed":0.2}`,
		`{"Time":"2026-05-28T12:00:00.500Z","Action":"run","Package":"example.test","Test":"TestC"}`,
		`{"Time":"2026-05-28T12:00:00.600Z","Action":"pass","Package":"example.test","Test":"TestC","Elapsed":0.1}`,
	}, "\n"))

	tl, err := parseTimeline(input, "busy")
	if err != nil {
		t.Fatalf("parseTimeline: %v", err)
	}

	var out bytes.Buffer
	if err := renderTrace(&out, tl); err != nil {
		t.Fatalf("renderTrace: %v", err)
	}

	var trace traceFile
	if err := json.Unmarshal(out.Bytes(), &trace); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}

	var busy []traceEvent
	for _, ev := range trace.TraceEvents {
		if ev.Name == "runner busy" && ev.Ph == "X" {
			busy = append(busy, ev)
		}
	}
	if got, want := len(busy), 2; got != want {
		t.Fatalf("busy interval count: got %d want %d", got, want)
	}
	if got, want := busy[0].Dur, int64(300000); got != want {
		t.Fatalf("first busy duration: got %d want %d", got, want)
	}
	if got, want := busy[1].Dur, int64(100000); got != want {
		t.Fatalf("second busy duration: got %d want %d", got, want)
	}
}

func TestParseTimelineRejectsUntimedEvents(t *testing.T) {
	t.Parallel()
	_, err := parseTimeline(strings.NewReader(`{"Action":"run","Package":"example.test","Test":"TestNoTime"}`), "bad")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "has no Time") {
		t.Fatalf("unexpected error: %v", err)
	}
}
