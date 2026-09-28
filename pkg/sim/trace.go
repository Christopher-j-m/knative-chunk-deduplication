package sim

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Invocation is one function call taken from a production trace. The app
// is an index into Trace.Apps, which keeps a week of 180 million calls near
// three gigabytes instead of the twenty a string per row would need
type Invocation struct {
	AtMs  int64
	App   int32
	DurMs int32
}

// Trace is a replayable invocation stream with its app name table and
// per-app invocation counts, which the frequency classes are read from.
type Trace struct {
	Invocations []Invocation
	Apps        []string
	Counts      []int
}

// LoadTrace reads the canonical trace CSV (header at_ms,app,dur_ms) and
// enforces the invariants the simulator relies on: ascending timestamps
// and non-negative durations. App ids are assigned by first appearance.
func LoadTrace(path string) (*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open trace: %w", err)
	}
	defer f.Close()

	// the canonical file has no quoting, so a plain line scanner is enough
	// and several times faster than encoding/csv on a week of traffic
	reader := bufio.NewReaderSize(f, 1<<20)
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read trace header: %w", err)
	}
	if strings.TrimSpace(header) != "at_ms,app,dur_ms" {
		return nil, fmt.Errorf("unexpected trace header %q, want at_ms,app,dur_ms", strings.TrimSpace(header))
	}

	trace := &Trace{}
	ids := map[string]int32{}
	var prev int64 = -1
	for line := 2; ; line++ {
		record, err := reader.ReadString('\n')
		if err == io.EOF && record == "" {
			break
		}
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read trace line %d: %w", line, err)
		}

		fields := strings.Split(strings.TrimSpace(record), ",")
		if len(fields) != 3 {
			return nil, fmt.Errorf("trace line %d has %d fields, want 3", line, len(fields))
		}

		at, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad at_ms on line %d: %w", line, err)
		}
		dur, err := strconv.ParseInt(fields[2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad dur_ms on line %d: %w", line, err)
		}
		if dur < 0 {
			return nil, fmt.Errorf("negative duration on line %d", line)
		}
		if at < prev {
			return nil, fmt.Errorf("trace not sorted at line %d", line)
		}
		prev = at

		id, ok := ids[fields[1]]
		if !ok {
			id = int32(len(trace.Apps))
			ids[fields[1]] = id
			trace.Apps = append(trace.Apps, fields[1])
			trace.Counts = append(trace.Counts, 0)
		}
		trace.Counts[id]++
		trace.Invocations = append(trace.Invocations, Invocation{AtMs: at, App: id, DurMs: int32(dur)})

		if err == io.EOF {
			break
		}
	}
	return trace, nil
}
