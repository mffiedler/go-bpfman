package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const tracePID = 1

type testEvent struct {
	Time    time.Time `json:"Time"`
	Action  string    `json:"Action"`
	Package string    `json:"Package"`
	Test    string    `json:"Test"`
	Elapsed float64   `json:"Elapsed"`
}

type interval struct {
	State string
	Start time.Time
	End   time.Time
}

type testRow struct {
	Package   string
	Test      string
	First     time.Time
	Last      time.Time
	Terminal  string
	Elapsed   float64
	Intervals []interval
	Paused    bool

	activeState string
	activeStart time.Time
}

type timeline struct {
	Title         string
	Start         time.Time
	End           time.Time
	Rows          []*testRow
	TestNameMatch string
}

type traceFile struct {
	TraceEvents []traceEvent `json:"traceEvents"`
	DisplayTime string       `json:"displayTimeUnit,omitempty"`
}

type traceEvent struct {
	Name string         `json:"name"`
	Cat  string         `json:"cat,omitempty"`
	Ph   string         `json:"ph"`
	Ts   int64          `json:"ts"`
	Dur  int64          `json:"dur,omitempty"`
	PID  int            `json:"pid"`
	TID  int            `json:"tid"`
	Args map[string]any `json:"args,omitempty"`
}

type scheduledRow struct {
	Row      *testRow
	Interval interval
	Lane     int
	Ts       int64
	Dur      int64
}

type busyInterval struct {
	Ts  int64
	Dur int64
}

func main() {
	var inputPath string
	var outputPath string
	var markersPath string
	var title string
	var testNameMatch string

	flag.StringVar(&inputPath, "input", "-", "go test JSON input path, or - for stdin")
	flag.StringVar(&outputPath, "output", "-", "Chrome trace JSON output path, or - for stdout")
	flag.StringVar(&markersPath, "markers", "", "optional JSONL file containing exact script_start/script_end timing markers")
	flag.StringVar(&title, "title", "Go test timeline", "trace title")
	flag.StringVar(&testNameMatch, "test-name-match", "", "only render test rows whose full test name contains this substring")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] < go-test.json > trace.json\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "Input must be the JSON event stream produced by `go test -json` or `go tool test2json -t`.")
		fmt.Fprintln(flag.CommandLine.Output(), "Output is Chrome Trace Event JSON loadable by chrome://tracing or https://ui.perfetto.dev.")
		fmt.Fprintln(flag.CommandLine.Output())
		flag.PrintDefaults()
	}
	flag.Parse()

	in, closeInput, err := openInput(inputPath)
	if err != nil {
		exitf("%v", err)
	}
	defer closeInput()

	tl, err := parseTimeline(in, title)
	if err != nil {
		exitf("%v", err)
	}
	if markersPath != "" {
		if err := applyMarkers(tl, markersPath); err != nil {
			exitf("%v", err)
		}
	}
	tl.TestNameMatch = testNameMatch

	out, closeOutput, err := openOutput(outputPath)
	if err != nil {
		exitf("%v", err)
	}
	defer closeOutput()

	if err := renderTrace(out, tl); err != nil {
		exitf("render trace: %v", err)
	}
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "go-test-timeline: "+format+"\n", args...)
	os.Exit(1)
}

func openInput(path string) (io.Reader, func(), error) {
	if path == "" || path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open input %q: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}

func openOutput(path string) (io.Writer, func(), error) {
	if path == "" || path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("create output %q: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}

func parseTimeline(r io.Reader, title string) (*timeline, error) {
	dec := json.NewDecoder(r)
	rows := map[string]*testRow{}
	var ordered []*testRow
	var minTime time.Time
	var maxTime time.Time

	for {
		var ev testEvent
		err := dec.Decode(&ev)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode test event: %w", err)
		}
		if ev.Test == "" {
			continue
		}
		if ev.Time.IsZero() {
			return nil, fmt.Errorf("event for %q has no Time; use `go test -json` or `go tool test2json -t`", ev.Test)
		}

		if minTime.IsZero() || ev.Time.Before(minTime) {
			minTime = ev.Time
		}
		if maxTime.IsZero() || ev.Time.After(maxTime) {
			maxTime = ev.Time
		}

		key := ev.Package + "\x00" + ev.Test
		row := rows[key]
		if row == nil {
			row = &testRow{
				Package: ev.Package,
				Test:    ev.Test,
				First:   ev.Time,
			}
			rows[key] = row
			ordered = append(ordered, row)
		}
		applyEvent(row, ev)
	}

	if len(ordered) == 0 {
		return nil, errors.New("no test events found")
	}

	for _, row := range ordered {
		if row.Last.IsZero() {
			row.Last = maxTime
		}
		if row.activeState != "" {
			row.closeInterval(maxTime)
		}
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].First.Equal(ordered[j].First) {
			return displayName(ordered[i]) < displayName(ordered[j])
		}
		return ordered[i].First.Before(ordered[j].First)
	})

	return &timeline{
		Title: title,
		Start: minTime,
		End:   maxTime,
		Rows:  ordered,
	}, nil
}

func applyEvent(row *testRow, ev testEvent) {
	if ev.Time.Before(row.First) {
		row.First = ev.Time
	}
	if ev.Time.After(row.Last) {
		row.Last = ev.Time
	}

	switch ev.Action {
	case "run":
		row.startInterval("running", ev.Time)
	case "pause":
		row.closeInterval(ev.Time)
		row.Paused = true
		row.startInterval("paused", ev.Time)
	case "cont":
		row.closeInterval(ev.Time)
		row.startInterval("running", ev.Time)
	case "pass", "fail", "skip":
		row.closeInterval(row.terminalTime(ev))
		row.Terminal = ev.Action
		row.Elapsed = ev.Elapsed
	}
}

func applyMarkers(tl *timeline, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open markers %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	type markerPair struct {
		start time.Time
		end   time.Time
	}
	markers := map[string]markerPair{}
	dec := json.NewDecoder(f)
	for {
		var ev testEvent
		err := dec.Decode(&ev)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode marker event: %w", err)
		}
		if ev.Test == "" {
			continue
		}
		if ev.Time.IsZero() {
			return fmt.Errorf("marker for %q has no Time", ev.Test)
		}
		key := ev.Package + "\x00" + ev.Test
		pair := markers[key]
		switch ev.Action {
		case "script_start":
			pair.start = ev.Time
		case "script_end":
			pair.end = ev.Time
		default:
			continue
		}
		markers[key] = pair
	}

	for _, row := range tl.Rows {
		key := row.Package + "\x00" + row.Test
		pair := markers[key]
		if pair.start.IsZero() || pair.end.IsZero() {
			continue
		}
		if pair.end.Before(pair.start) {
			pair.end = pair.start
		}
		row.First = pair.start
		row.Last = pair.end
		row.Intervals = []interval{{
			State: "running",
			Start: pair.start,
			End:   pair.end,
		}}
		row.Paused = false
		row.activeState = ""
		row.activeStart = time.Time{}
	}
	return nil
}

func (r *testRow) terminalTime(ev testEvent) time.Time {
	if r.activeState == "" || ev.Elapsed <= 0 {
		return ev.Time
	}
	// test2json timestamps terminal actions when the final
	// "--- PASS/FAIL/SKIP" line is emitted. For parallel subtests
	// that can be much later than the test actually finished
	// because the parent prints summaries after all children have
	// completed. The elapsed field is the authoritative per-test
	// duration reported by the test binary, so use it to close the
	// active running slice at the test's actual end.
	t := r.activeStart.Add(time.Duration(ev.Elapsed * float64(time.Second)))
	if t.After(ev.Time) {
		return ev.Time
	}
	return t
}

func (r *testRow) startInterval(state string, t time.Time) {
	if r.activeState != "" {
		r.closeInterval(t)
	}
	r.activeState = state
	r.activeStart = t
}

func (r *testRow) closeInterval(t time.Time) {
	if r.activeState == "" {
		return
	}
	if t.Before(r.activeStart) {
		t = r.activeStart
	}
	r.Intervals = append(r.Intervals, interval{
		State: r.activeState,
		Start: r.activeStart,
		End:   t,
	})
	r.activeState = ""
	r.activeStart = time.Time{}
}

func renderTrace(w io.Writer, tl *timeline) error {
	scheduled, laneCount := assignLanes(filteredRows(tl.Rows, tl.TestNameMatch))
	busy := deriveBusyIntervals(scheduled)
	trace := traceFile{
		DisplayTime: "ms",
	}
	trace.TraceEvents = append(trace.TraceEvents, traceEvent{
		Name: "process_name",
		Ph:   "M",
		PID:  tracePID,
		TID:  0,
		Args: map[string]any{"name": tl.Title},
	})

	if len(busy) > 0 {
		trace.TraceEvents = append(trace.TraceEvents, traceEvent{
			Name: "thread_name",
			Ph:   "M",
			PID:  tracePID,
			TID:  1,
			Args: map[string]any{"name": "runner busy"},
		})
	}

	for lane := 0; lane < laneCount; lane++ {
		tid := lane + 2
		trace.TraceEvents = append(trace.TraceEvents, traceEvent{
			Name: "thread_name",
			Ph:   "M",
			PID:  tracePID,
			TID:  tid,
			Args: map[string]any{"name": "lane"},
		})
	}

	for _, item := range busy {
		trace.TraceEvents = append(trace.TraceEvents, traceEvent{
			Name: "runner busy",
			Cat:  "busy",
			Ph:   "X",
			Ts:   item.Ts - tl.Start.UnixMicro(),
			Dur:  item.Dur,
			PID:  tracePID,
			TID:  1,
		})
	}

	for _, item := range scheduled {
		row := item.Row
		trace.TraceEvents = append(trace.TraceEvents, traceEvent{
			Name: shortName(row.Test),
			Cat:  category(row, item.Interval),
			Ph:   "X",
			Ts:   item.Ts - tl.Start.UnixMicro(),
			Dur:  item.Dur,
			PID:  tracePID,
			TID:  item.Lane + 2,
			Args: map[string]any{
				"elapsed":      row.Elapsed,
				"full_name":    displayName(row),
				"package":      row.Package,
				"parallelLane": item.Lane,
				"state":        item.Interval.State,
				"terminal":     row.Terminal,
				"test":         row.Test,
			},
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(trace)
}

func deriveBusyIntervals(scheduled []scheduledRow) []busyInterval {
	if len(scheduled) == 0 {
		return nil
	}
	intervals := make([]scheduledRow, len(scheduled))
	copy(intervals, scheduled)
	sort.SliceStable(intervals, func(i, j int) bool {
		if intervals[i].Ts == intervals[j].Ts {
			return intervals[i].Dur < intervals[j].Dur
		}
		return intervals[i].Ts < intervals[j].Ts
	})

	var busy []busyInterval
	start := intervals[0].Ts
	end := intervals[0].Ts + intervals[0].Dur
	for _, item := range intervals[1:] {
		itemEnd := item.Ts + item.Dur
		if item.Ts <= end {
			if itemEnd > end {
				end = itemEnd
			}
			continue
		}
		busy = append(busy, busyInterval{Ts: start, Dur: end - start})
		start = item.Ts
		end = itemEnd
	}
	busy = append(busy, busyInterval{Ts: start, Dur: end - start})
	return busy
}

func filteredRows(rows []*testRow, testNameMatch string) []*testRow {
	if testNameMatch == "" {
		return rows
	}
	filtered := make([]*testRow, 0, len(rows))
	for _, row := range rows {
		if strings.Contains(displayName(row), testNameMatch) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func assignLanes(rows []*testRow) ([]scheduledRow, int) {
	var intervals []scheduledRow
	for _, row := range rows {
		for _, iv := range row.Intervals {
			if iv.State == "running" && !dropRegistrationInterval(row, iv) {
				ts := iv.Start.UnixMicro()
				dur := iv.End.Sub(iv.Start).Microseconds()
				if dur < 1 {
					dur = 1
				}
				intervals = append(intervals, scheduledRow{
					Row:      row,
					Interval: iv,
					Ts:       ts,
					Dur:      dur,
				})
			}
		}
	}
	sort.SliceStable(intervals, func(i, j int) bool {
		if intervals[i].Ts == intervals[j].Ts {
			return displayName(intervals[i].Row) < displayName(intervals[j].Row)
		}
		return intervals[i].Ts < intervals[j].Ts
	})

	laneEnds := make([]int64, 0)
	scheduled := make([]scheduledRow, 0, len(intervals))

	for _, item := range intervals {
		lane := firstAvailableLane(laneEnds, item.Ts)
		if lane == len(laneEnds) {
			laneEnds = append(laneEnds, item.Ts+item.Dur+1)
		} else {
			laneEnds[lane] = item.Ts + item.Dur + 1
		}
		item.Lane = lane
		scheduled = append(scheduled, item)
	}

	return scheduled, len(laneEnds)
}

func dropRegistrationInterval(row *testRow, iv interval) bool {
	return row.Paused && iv.Start.Equal(row.First)
}

func firstAvailableLane(laneEnds []int64, start int64) int {
	for lane, end := range laneEnds {
		if start >= end {
			return lane
		}
	}
	return len(laneEnds)
}

func category(row *testRow, iv interval) string {
	if iv.State == "paused" {
		return "paused"
	}
	switch row.Terminal {
	case "fail":
		return "failed"
	case "skip":
		return "skipped"
	default:
		return "running"
	}
}

func shortName(test string) string {
	for i := len(test) - 1; i >= 0; i-- {
		if test[i] == '/' {
			return test[i+1:]
		}
	}
	return test
}

func displayName(row *testRow) string {
	if row.Package == "" {
		return row.Test
	}
	return row.Package + "/" + row.Test
}
