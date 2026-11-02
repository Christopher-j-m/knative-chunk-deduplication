package sim

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Contains a single app invocation of the ibm trace
// => This uses only roughly 3gb mem, while using this format instead of the raw data with roughly 20 gb
type Invocation struct {
	AtMs  int64 // When the call happens (ms) from start of recording
	App   int32 // app index number we assigned
	DurMs int32 // duration of the function invocation (ms)
}

type Trace struct {
	Invocations []Invocation // All app calls
	Apps        []string     // lookup app id -> app name from traces
	Counts      []int        // how many times each app was called via app id
}

// Reads the ibm trace csv file and parses the fields at_ms, app, dur_ms rows into Invocation format
// Also assigns an app, which hasnt been seen yet, the next available app id
func LoadTrace(path string) (*Trace, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open trace csv file: %w", err)
	}
	defer file.Close()

	// Check that the csv header is as expected
	reader := bufio.NewReaderSize(file, 1<<20) // Always read in 1mb parts and not tiny reads, since we want to be fast on this big trace file
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("error while trying to read trace header: %w", err)
	}
	if strings.TrimSpace(header) != "at_ms,app,dur_ms" {
		return nil, fmt.Errorf("unexpected trace header %q, expected at_ms,app,dur_ms", strings.TrimSpace(header))
	}

	trace := &Trace{}
	ids := map[string]int32{}
	var prev int64 = -1

	// Start reading the trace from line 2 (the first one after csv header)
	for line := 2; ; line++ {
		record, err := reader.ReadString('\n')
		// Empty/End of File => stop right away
		if err == io.EOF && record == "" {
			break
		}

		// Error while not at end of file => Error for this row
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("error while trying to read trace line %d: %w", line, err)
		}

		// Curr row has more fields than the previously checked header => Error
		fields := strings.Split(strings.TrimSpace(record), ",")
		if len(fields) != 3 {
			return nil, fmt.Errorf("trace line %d has %d fields, expected 3 just like the csv headers", line, len(fields))
		}

		// Parse at_ms column value on curr line
		at, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad at_ms on line %d: %w", line, err)
		}

		// dur_ms value
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

		// Assign the app from curr row a id (if not seen and assigned already)
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
