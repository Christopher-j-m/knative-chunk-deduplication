package sim

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
)

// Invocation is one function call taken from a production trace
type Invocation struct {
	AtMs  int64
	App   string
	DurMs int64
}

// LoadTrace reads the canonical trace CSV (header at_ms,app,dur_ms) and
// enforces the invariants the simulator relies on: ascending timestamps
// and non-negative durations.
func LoadTrace(path string) ([]Invocation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open trace: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read trace header: %w", err)
	}
	if len(header) != 3 || header[0] != "at_ms" || header[1] != "app" || header[2] != "dur_ms" {
		return nil, fmt.Errorf("unexpected trace header %v, want [at_ms app dur_ms]", header)
	}

	var invs []Invocation
	var prev int64 = -1
	for line := 2; ; line++ {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read trace line %d: %w", line, err)
		}

		at, err := strconv.ParseInt(record[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad at_ms on line %d: %w", line, err)
		}
		dur, err := strconv.ParseInt(record[2], 10, 64)
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

		invs = append(invs, Invocation{AtMs: at, App: record[1], DurMs: dur})
	}
	return invs, nil
}
