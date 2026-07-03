// Command benchgate compares two `go test -bench` outputs (baseline
// vs candidate) and exits non-zero when a benchmark regresses beyond
// configured thresholds.
//
// Parsing and statistics come from golang.org/x/perf — benchfmt for
// the benchmark format and benchmath (the engine behind benchstat)
// for summaries and significance tests. benchgate only adds the
// policy benchstat deliberately does not provide: thresholds, floors,
// and a failing exit code for CI.
//
// It is the comparison step of the bench-gate CI workflow: allocs/op
// and B/op are deterministic for the same code on the same machine,
// so they get tight ratio thresholds that catch O(archive)-instead-
// of-O(delta) work regressions regardless of sample count; time
// (sec/op) is noisy on shared runners, so it gets a loose threshold
// and additionally must be a statistically significant difference
// (Mann-Whitney U, as in benchstat) before it fails the gate.
// Baselines below a per-metric floor are skipped entirely, since a
// few extra allocations on a tiny benchmark is noise, not a
// regression.
//
// Multiple runs of the same benchmark (-count=N) are kept as a
// sample. The baseline is summarized by its median; the candidate is
// gated on its median for time but on its WORST run for allocs/op
// and B/op — those are deterministic, so a single outlier run there
// is a real intermittent allocation path, not noise, and must fail.
// Gating is per benchmark: any one benchmark over its threshold
// fails the gate; there is no cross-benchmark averaging. Benchmarks
// present on only one side are reported but never fail the gate, so
// adding or removing benchmarks in a PR does not wedge it.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"golang.org/x/perf/benchfmt"
	"golang.org/x/perf/benchmath"
	"golang.org/x/perf/benchunit"
)

// benchSamples collects every measured value per benchmark and unit:
// benchmark key -> tidied unit (sec/op, B/op, allocs/op, ...) ->
// samples across -count runs. The key includes the package path when
// the output carries one, so same-named benchmarks in different
// packages never merge.
type benchSamples map[string]map[string][]float64

// gate is one metric's regression rule: fail when the candidate
// exceeds the baseline median by more than maxRatio, unless the
// baseline is below floor (too small to compare meaningfully). With
// worstCase set, the candidate is judged by its worst (highest) run
// rather than its median — for deterministic metrics, where any
// outlier run is a real intermittent code path. With
// needSignificance set, the samples must also differ significantly
// under the benchmath comparison test — the benchstat noise guard,
// used for wall-clock time.
type gate struct {
	unit             string
	maxRatio         float64
	floor            float64
	worstCase        bool
	needSignificance bool
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
	cls := benchunit.ClassOf(v.unit)
	return fmt.Sprintf(
		"%s: %s regressed %.2fx (%s -> %s, limit %.2fx)",
		v.name, v.unit, v.ratio,
		benchunit.Scale(v.old, cls), benchunit.Scale(v.new, cls),
		v.maxRatio,
	)
}

// parseBench extracts benchmark samples from `go test -bench` output
// using the official format parser. Non-result records (unit
// metadata, syntax errors from stray output) are skipped. Values
// arrive tidied by benchfmt: ns/op becomes sec/op, MB/s becomes B/s.
func parseBench(reader *benchfmt.Reader) (benchSamples, error) {
	out := make(benchSamples)
	for reader.Scan() {
		res, ok := reader.Result().(*benchfmt.Result)
		if !ok {
			continue
		}
		name := string(res.Name.Full())
		if pkg := res.GetConfig("pkg"); pkg != "" {
			name = pkg + "." + name
		}
		units := out[name]
		if units == nil {
			units = make(map[string][]float64)
			out[name] = units
		}
		for _, v := range res.Values {
			units[v.Unit] = append(units[v.Unit], v.Value)
		}
	}
	if err := reader.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// compare applies the gates to every benchmark present in both maps
// and returns a human-readable report plus the violations.
func compare(
	oldRes, newRes benchSamples, gates []gate,
) (report []string, violations []violation) {
	thresholds := benchmath.DefaultThresholds
	names := make([]string, 0, len(newRes))
	for name := range newRes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		oldUnits, ok := oldRes[name]
		if !ok {
			report = append(report, fmt.Sprintf(
				"%s: new benchmark, no baseline to compare", name,
			))
			continue
		}
		var parts []string
		for _, g := range gates {
			oldVals, okOld := oldUnits[g.unit]
			newVals, okNew := newRes[name][g.unit]
			if !okOld || !okNew {
				continue
			}
			oldSample := benchmath.NewSample(oldVals, &thresholds)
			newSample := benchmath.NewSample(newVals, &thresholds)
			oldCenter := benchmath.AssumeNothing.
				Summary(oldSample, 0.95).Center
			newCenter := benchmath.AssumeNothing.
				Summary(newSample, 0.95).Center
			if g.worstCase {
				// Samples are sorted ascending; the worst
				// candidate run is the last one.
				newCenter = newSample.Values[len(newSample.Values)-1]
			}
			cls := benchunit.ClassOf(g.unit)

			if oldCenter <= 0 || oldCenter < g.floor {
				parts = append(parts, fmt.Sprintf(
					"%s %s -> %s (below %s floor, not gated)",
					g.unit,
					benchunit.Scale(oldCenter, cls),
					benchunit.Scale(newCenter, cls),
					benchunit.Scale(g.floor, cls),
				))
				continue
			}

			ratio := newCenter / oldCenter
			cmp := benchmath.AssumeNothing.Compare(oldSample, newSample)
			significant := cmp.P < cmp.Alpha
			var detail string
			if g.worstCase {
				detail = fmt.Sprintf(
					"%s %s -> %s (%.2fx, limit %.2fx, worst of %d run(s))",
					g.unit,
					benchunit.Scale(oldCenter, cls),
					benchunit.Scale(newCenter, cls),
					ratio, g.maxRatio, len(newSample.Values),
				)
			} else {
				detail = fmt.Sprintf(
					"%s %s -> %s (%.2fx, limit %.2fx, %s)",
					g.unit,
					benchunit.Scale(oldCenter, cls),
					benchunit.Scale(newCenter, cls),
					ratio, g.maxRatio, cmp,
				)
				if g.needSignificance && !significant {
					detail += " [not significant, not gated]"
				}
			}
			parts = append(parts, detail)

			if ratio > g.maxRatio &&
				(!g.needSignificance || significant) &&
				!math.IsNaN(ratio) {
				violations = append(violations, violation{
					name: name, unit: g.unit,
					old: oldCenter, new: newCenter,
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

func parseFile(path string) (benchSamples, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseBench(benchfmt.NewReader(f, path))
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
		"fail when median sec/op exceeds baseline by this factor "+
			"(only when the difference is statistically significant)",
	)
	maxAllocRatio := flag.Float64(
		"max-alloc-ratio", 1.25,
		"fail when median allocs/op exceeds baseline by this factor",
	)
	maxBytesRatio := flag.Float64(
		"max-bytes-ratio", 1.35,
		"fail when median B/op exceeds baseline by this factor",
	)
	timeFloorNs := flag.Float64(
		"time-floor-ns", 100_000,
		"skip the time gate when the baseline is below this many ns",
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
		{
			unit:      "allocs/op",
			maxRatio:  *maxAllocRatio,
			floor:     *allocFloor,
			worstCase: true,
		},
		{
			unit:      "B/op",
			maxRatio:  *maxBytesRatio,
			floor:     *bytesFloor,
			worstCase: true,
		},
		{
			unit:             "sec/op",
			maxRatio:         *maxTimeRatio,
			floor:            *timeFloorNs / 1e9,
			needSignificance: true,
		},
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
