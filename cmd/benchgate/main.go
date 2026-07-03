// Command benchgate compares two `go test -bench` outputs (baseline
// vs candidate) and exits non-zero when a benchmark regresses beyond
// configured thresholds.
//
// It is the comparison step of the bench-gate CI workflow: allocs/op
// and B/op are deterministic for the same code on the same machine,
// so they get tight thresholds that catch O(archive)-instead-of-
// O(delta) work regressions; ns/op is noisy on shared runners, so it
// gets a loose threshold that only catches algorithmic blowups.
// Small baselines below the per-metric floors are skipped entirely,
// since a few extra allocations on a tiny benchmark is noise, not a
// regression.
//
// Multiple runs of the same benchmark (-count=N) are aggregated by
// taking the minimum per metric, the standard way to strip scheduler
// and GC noise. Benchmarks present on only one side are reported but
// never fail the gate, so adding or removing benchmarks in a PR does
// not wedge it.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// benchResult maps a metric unit (ns/op, B/op, allocs/op, or any
// custom -benchmem/ReportMetric unit) to the best (lowest) value
// observed across runs.
type benchResult map[string]float64

// gate is one metric's regression rule: fail when
// candidate/baseline exceeds maxRatio, unless the baseline is below
// floor (too small to compare meaningfully).
type gate struct {
	unit     string
	maxRatio float64
	floor    float64
}

// violation describes one gate failure.
type violation struct {
	name     string
	unit     string
	old, new float64
	ratio    float64
	maxRatio float64
}

func (v violation) String() string {
	return fmt.Sprintf(
		"%s: %s regressed %.2fx (%.0f -> %.0f, limit %.2fx)",
		v.name, v.unit, v.ratio, v.old, v.new, v.maxRatio,
	)
}

// parseBench extracts benchmark results from `go test -bench`
// output. Lines that do not look like benchmark result lines
// (package headers, logs, PASS/ok trailers) are ignored. Repeated
// names keep the minimum value per metric.
func parseBench(r io.Reader) (map[string]benchResult, error) {
	out := make(map[string]benchResult)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 ||
			!strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		// The second column is the iteration count; anything else
		// (e.g. a log line that happens to start with "Benchmark")
		// is not a result line.
		if _, err := strconv.Atoi(fields[1]); err != nil {
			continue
		}
		name := fields[0]
		res := out[name]
		if res == nil {
			res = make(benchResult)
			out[name] = res
		}
		for i := 2; i+1 < len(fields); i += 2 {
			value, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				break
			}
			unit := fields[i+1]
			if prev, ok := res[unit]; !ok || value < prev {
				res[unit] = value
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// compare applies the gates to every benchmark present in both maps
// and returns the violations plus a human-readable report.
func compare(
	oldRes, newRes map[string]benchResult, gates []gate,
) (report []string, violations []violation) {
	names := make([]string, 0, len(newRes))
	for name := range newRes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		oldBench, ok := oldRes[name]
		if !ok {
			report = append(report, fmt.Sprintf(
				"%s: new benchmark, no baseline to compare", name,
			))
			continue
		}
		var parts []string
		for _, g := range gates {
			oldV, okOld := oldBench[g.unit]
			newV, okNew := newRes[name][g.unit]
			if !okOld || !okNew {
				continue
			}
			if oldV < g.floor {
				parts = append(parts, fmt.Sprintf(
					"%s %.0f -> %.0f (below %.0f floor, not gated)",
					g.unit, oldV, newV, g.floor,
				))
				continue
			}
			ratio := newV / oldV
			parts = append(parts, fmt.Sprintf(
				"%s %.0f -> %.0f (%.2fx, limit %.2fx)",
				g.unit, oldV, newV, ratio, g.maxRatio,
			))
			if ratio > g.maxRatio {
				violations = append(violations, violation{
					name: name, unit: g.unit,
					old: oldV, new: newV,
					ratio: ratio, maxRatio: g.maxRatio,
				})
			}
		}
		report = append(report, fmt.Sprintf(
			"%s: %s", name, strings.Join(parts, ", "),
		))
	}

	var removed []string
	for name := range oldRes {
		if _, ok := newRes[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, name := range removed {
		report = append(report, fmt.Sprintf(
			"%s: present in baseline but missing from candidate",
			name,
		))
	}
	return report, violations
}

func parseFile(path string) (map[string]benchResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseBench(f)
}

func main() {
	oldPath := flag.String(
		"old", "", "baseline `go test -bench` output file",
	)
	newPath := flag.String(
		"new", "", "candidate `go test -bench` output file",
	)
	maxTimeRatio := flag.Float64(
		"max-time-ratio", 2.0,
		"fail when ns/op exceeds baseline by this factor",
	)
	maxAllocRatio := flag.Float64(
		"max-alloc-ratio", 1.25,
		"fail when allocs/op exceeds baseline by this factor",
	)
	maxBytesRatio := flag.Float64(
		"max-bytes-ratio", 1.35,
		"fail when B/op exceeds baseline by this factor",
	)
	timeFloorNs := flag.Float64(
		"time-floor-ns", 100_000,
		"skip the ns/op gate when the baseline is below this",
	)
	allocFloor := flag.Float64(
		"alloc-floor", 64,
		"skip the allocs/op gate when the baseline is below this",
	)
	bytesFloor := flag.Float64(
		"bytes-floor", 16_384,
		"skip the B/op gate when the baseline is below this",
	)
	flag.Parse()

	if *oldPath == "" || *newPath == "" {
		fmt.Fprintln(os.Stderr, "benchgate: -old and -new are required")
		flag.Usage()
		os.Exit(2)
	}

	oldRes, err := parseFile(*oldPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchgate: reading baseline: %v\n", err)
		os.Exit(2)
	}
	newRes, err := parseFile(*newPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchgate: reading candidate: %v\n", err)
		os.Exit(2)
	}

	gates := []gate{
		{unit: "allocs/op", maxRatio: *maxAllocRatio, floor: *allocFloor},
		{unit: "B/op", maxRatio: *maxBytesRatio, floor: *bytesFloor},
		{unit: "ns/op", maxRatio: *maxTimeRatio, floor: *timeFloorNs},
	}
	report, violations := compare(oldRes, newRes, gates)
	for _, line := range report {
		fmt.Println(line)
	}
	if len(newRes) == 0 {
		fmt.Println("benchgate: candidate output contains no benchmarks")
		os.Exit(2)
	}
	if len(violations) > 0 {
		fmt.Println()
		fmt.Printf("benchgate: %d regression(s):\n", len(violations))
		for _, v := range violations {
			fmt.Printf("  %s\n", v)
		}
		os.Exit(1)
	}
	fmt.Println("benchgate: no regressions beyond thresholds")
}
