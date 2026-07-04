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
// adding or removing benchmarks in a PR does not wedge it. A gated
// unit missing from the baseline (a legitimately older or partial
// base run) is reported as not gated; a gated unit missing from the
// candidate (e.g. -benchmem dropped) is a configuration error, since
// it would otherwise silently disable that gate for good.
//
// Lines that look like benchmark results but fail to parse (for
// example test log output interleaved into a result line) are a
// corrupted capture: they are reported and the gate exits 2, because
// the corrupted benchmark would otherwise silently vanish from both
// sides and never gate again.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/perf/benchfmt"
	"golang.org/x/perf/benchmath"
	"golang.org/x/perf/benchunit"
)

// minTimeSamples is the per-side sample count the sec/op
// significance test needs before its verdict means anything.
const minTimeSamples = 5

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

// configIssue describes a capture that cannot support the gate it
// was given (e.g. too few candidate samples for significance
// testing) — a CI configuration error, not a regression.
type configIssue struct {
	name string
	msg  string
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
// using the official format parser. Values arrive tidied by
// benchfmt: ns/op becomes sec/op, MB/s becomes B/s. Lines that look
// like results but fail to parse are returned as syntax errors so a
// corrupted capture is loud instead of silently missing benchmarks.
func parseBench(
	reader *benchfmt.Reader,
) (benchSamples, []string, error) {
	out := make(benchSamples)
	var syntaxErrs []string
	for reader.Scan() {
		res, ok := reader.Result().(*benchfmt.Result)
		if !ok {
			if serr, isSyntax := reader.Result().(*benchfmt.SyntaxError); isSyntax {
				syntaxErrs = append(syntaxErrs, serr.Error())
			}
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
		return nil, nil, err
	}
	return out, syntaxErrs, nil
}

// evalGate applies one gate to one benchmark's samples and returns
// the report fragment plus an optional violation or config issue
// (their name fields are filled in by the caller).
func evalGate(
	g gate, oldVals, newVals []float64,
) (string, *violation, *configIssue) {
	thresholds := benchmath.DefaultThresholds
	oldSample := benchmath.NewSample(oldVals, &thresholds)
	newSample := benchmath.NewSample(newVals, &thresholds)
	oldCenter := benchmath.AssumeNothing.
		Summary(oldSample, 0.95).Center
	var newCenter float64
	if g.worstCase {
		// Samples are sorted ascending; the worst candidate run
		// is the last one.
		newCenter = newSample.Values[len(newSample.Values)-1]
	} else {
		newCenter = benchmath.AssumeNothing.
			Summary(newSample, 0.95).Center
	}
	cls := benchunit.ClassOf(g.unit)
	span := fmt.Sprintf(
		"%s %s -> %s", g.unit,
		benchunit.Scale(oldCenter, cls),
		benchunit.Scale(newCenter, cls),
	)

	if oldCenter <= 0 || oldCenter < g.floor {
		return fmt.Sprintf(
			"%s (below %s floor, not gated)",
			span, benchunit.Scale(g.floor, cls),
		), nil, nil
	}
	if g.needSignificance && len(newVals) < minTimeSamples {
		issue := &configIssue{msg: fmt.Sprintf(
			"%s needs at least %d candidate samples for significance gating, got %d",
			g.unit, minTimeSamples, len(newVals),
		)}
		return span + " (too few candidate samples, not gated)",
			nil, issue
	}
	if g.needSignificance && len(oldVals) < minTimeSamples {
		// A short baseline is not a configuration error: the base
		// run may legitimately be partial (e.g. it failed part-way
		// and the workflow gates against what it produced).
		return fmt.Sprintf(
			"%s (baseline has only %d sample(s), significance needs %d, not gated)",
			span, len(oldVals), minTimeSamples,
		), nil, nil
	}

	ratio := newCenter / oldCenter
	detail, significant := gateDetail(
		g, oldSample, newSample, span, ratio,
	)
	var v *violation
	if ratio > g.maxRatio && (!g.needSignificance || significant) {
		v = &violation{
			unit: g.unit,
			old:  oldCenter, new: newCenter,
			ratio: ratio, maxRatio: g.maxRatio,
		}
	}
	return detail, v, nil
}

// gateDetail renders the gated report fragment and, for
// significance-gated units, runs the benchmath comparison.
func gateDetail(
	g gate, oldSample, newSample *benchmath.Sample,
	span string, ratio float64,
) (string, bool) {
	if g.worstCase {
		// Also surface the baseline's worst run: the gate is
		// deliberately candidate-worst vs baseline-median, so
		// pre-existing baseline instability should at least be
		// visible when reading a failure.
		cls := benchunit.ClassOf(g.unit)
		oldWorst := oldSample.Values[len(oldSample.Values)-1]
		return fmt.Sprintf(
			"%s (%.2fx, limit %.2fx, worst of %d run(s), baseline worst %s)",
			span, ratio, g.maxRatio, len(newSample.Values),
			benchunit.Scale(oldWorst, cls),
		), false
	}
	cmp := benchmath.AssumeNothing.Compare(oldSample, newSample)
	significant := cmp.P < cmp.Alpha
	detail := fmt.Sprintf(
		"%s (%.2fx, limit %.2fx, %s)", span, ratio, g.maxRatio, cmp,
	)
	if g.needSignificance && !significant {
		detail += " [not significant, not gated]"
	}
	return detail, significant
}

// compare applies the gates to every benchmark present in both maps
// and returns a human-readable report plus the violations and
// config issues.
func compare(
	oldRes, newRes benchSamples, gates []gate,
) (report []string, violations []violation, issues []configIssue) {
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
		parts, vs, is := compareUnits(gates, oldUnits, newRes[name])
		for i := range vs {
			vs[i].name = name
		}
		for i := range is {
			is[i].name = name
		}
		violations = append(violations, vs...)
		issues = append(issues, is...)
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
	return report, violations, issues
}

// compareUnits evaluates every gate for one benchmark, plus a note
// for candidate units no gate covers (custom b.ReportMetric units
// are collected but deliberately never gated).
func compareUnits(
	gates []gate, oldUnits, newUnits map[string][]float64,
) (parts []string, vs []violation, is []configIssue) {
	gated := make(map[string]bool, len(gates))
	for _, g := range gates {
		gated[g.unit] = true
		oldVals, okOld := oldUnits[g.unit]
		newVals, okNew := newUnits[g.unit]
		switch {
		case !okOld && !okNew:
			// The benchmark doesn't emit this unit at all.
			continue
		case !okOld:
			// A baseline may legitimately lack a unit (older or
			// partial base run): report, don't gate.
			parts = append(parts, fmt.Sprintf(
				"%s missing from baseline, not gated", g.unit,
			))
			continue
		case !okNew:
			// The candidate capture is under the workflow's
			// control; losing a gated unit the baseline has (e.g.
			// -benchmem dropped) would silently disable this gate,
			// so it is a configuration error.
			parts = append(parts, fmt.Sprintf(
				"%s missing from candidate", g.unit,
			))
			is = append(is, configIssue{msg: fmt.Sprintf(
				"%s present in baseline but missing from candidate capture (was -benchmem dropped?)",
				g.unit,
			)})
			continue
		}
		part, v, issue := evalGate(g, oldVals, newVals)
		parts = append(parts, part)
		if v != nil {
			vs = append(vs, *v)
		}
		if issue != nil {
			is = append(is, *issue)
		}
	}
	var custom []string
	for unit := range newUnits {
		if !gated[unit] {
			custom = append(custom, unit)
		}
	}
	sort.Strings(custom)
	for _, unit := range custom {
		parts = append(parts, fmt.Sprintf(
			"%s has no gate, not gated", unit,
		))
	}
	return parts, vs, is
}

func parseFile(path string) (benchSamples, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return parseBench(benchfmt.NewReader(f, path))
}

// results bundles everything render needs to produce the final
// output and exit code.
type results struct {
	report               []string
	violations           []violation
	issues               []configIssue
	newCount             int
	oldSyntax, newSyntax []string
}

// render formats the human-readable outcome and picks the exit
// code: 2 for unusable input or configuration errors, 1 for
// regressions, 0 otherwise. Violations always print, even when a
// config issue or corrupted capture also occurred, so a detected
// regression is never hidden behind an exit-2.
func render(r results) (string, int) {
	var b strings.Builder
	for _, line := range r.report {
		fmt.Fprintln(&b, line)
	}
	if len(r.violations) > 0 {
		fmt.Fprintf(&b, "\nbenchgate: %d regression(s):\n",
			len(r.violations))
		for _, v := range r.violations {
			fmt.Fprintf(&b, "  %s\n", v)
		}
	}
	renderSyntax(&b, "baseline", r.oldSyntax)
	renderSyntax(&b, "candidate", r.newSyntax)
	if len(r.issues) > 0 {
		fmt.Fprintln(&b, "\nbenchgate: invalid benchmark configuration:")
		for _, issue := range r.issues {
			fmt.Fprintf(&b, "  %s: %s\n", issue.name, issue.msg)
		}
	}
	switch {
	case len(r.oldSyntax)+len(r.newSyntax) > 0 || len(r.issues) > 0:
		return b.String(), 2
	case r.newCount == 0:
		fmt.Fprintln(&b, "benchgate: candidate output contains no benchmarks")
		return b.String(), 2
	case len(r.violations) > 0:
		return b.String(), 1
	}
	fmt.Fprintln(&b, "benchgate: no regressions beyond thresholds")
	return b.String(), 0
}

// renderSyntax reports unparseable result lines in one capture. A
// benchmark whose result line is corrupted (e.g. by interleaved log
// output) parses on neither side and would otherwise vanish from
// the gate without a trace.
func renderSyntax(b *strings.Builder, side string, errs []string) {
	if len(errs) == 0 {
		return
	}
	fmt.Fprintf(
		b,
		"\nbenchgate: %s capture is corrupted (%d unparseable result line(s); benchmarks on those lines are not gated):\n",
		side, len(errs),
	)
	for _, e := range errs {
		fmt.Fprintf(b, "  %s\n", e)
	}
}

// flags holds the parsed command line.
type flags struct {
	oldPath, newPath string
	gates            []gate
}

func parseFlags() flags {
	oldPath := flag.String(
		"old", "", "baseline `go test -bench` output file",
	)
	newPath := flag.String(
		"new", "", "candidate `go test -bench` output file",
	)
	maxTimeRatio := flag.Float64(
		"max-time-ratio", 2.0,
		"fail when candidate median sec/op exceeds baseline by this factor "+
			"(only when the difference is statistically significant; needs at "+
			"least 5 candidate samples)",
	)
	maxAllocRatio := flag.Float64(
		"max-alloc-ratio", 1.25,
		"fail when candidate worst-run allocs/op exceeds baseline median by this factor",
	)
	maxBytesRatio := flag.Float64(
		"max-bytes-ratio", 1.35,
		"fail when candidate worst-run B/op exceeds baseline median by this factor",
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
	return flags{
		oldPath: *oldPath,
		newPath: *newPath,
		gates: []gate{
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
		},
	}
}

func main() {
	cfg := parseFlags()
	oldRes, oldSyntax, err := parseFile(cfg.oldPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchgate: reading baseline: %v\n", err)
		os.Exit(2)
	}
	newRes, newSyntax, err := parseFile(cfg.newPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchgate: reading candidate: %v\n", err)
		os.Exit(2)
	}

	report, violations, issues := compare(oldRes, newRes, cfg.gates)
	out, code := render(results{
		report:     report,
		violations: violations,
		issues:     issues,
		newCount:   len(newRes),
		oldSyntax:  oldSyntax,
		newSyntax:  newSyntax,
	})
	fmt.Print(out)
	os.Exit(code)
}
