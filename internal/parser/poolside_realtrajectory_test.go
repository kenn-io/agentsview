// ABOUTME: Build-tagged end-to-end validation: parses real
// ABOUTME: poolside trajectory files from a host path and prints a
// ABOUTME: per-model breakdown to confirm mid-session model switches
// ABOUTME: are attributed correctly. Not used by CI; opt in via
// ABOUTME: POOLSIDE_REAL_TRAJECTORIES:
// ABOUTME:   POOLSIDE_REAL_TRAJECTORIES=/path/to/poolside/trajectories \
// ABOUTME:     CGO_ENABLED=1 go test \
// ABOUTME:       -tags "fts5,poolsiderealtrajectory" \
// ABOUTME:       -run TestPoolsideRealTrajectoryBreakdown \
// ABOUTME:       ./internal/parser/ -v

//go:build poolsiderealtrajectory

package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPoolsideRealTrajectoryBreakdown(t *testing.T) {
	dir := os.Getenv("POOLSIDE_REAL_TRAJECTORIES")
	if dir == "" {
		t.Skip("set POOLSIDE_REAL_TRAJECTORIES to a directory of trajectory-*.ndjson files to enable real-data validation")
	}
	files, err := filepath.Glob(filepath.Join(dir, "trajectory-*.ndjson"))
	require.NoError(t, err)
	if len(files) == 0 {
		t.Skipf("no trajectory-*.ndjson under %s", dir)
	}
	sort.Strings(files)
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			runRealPoolsideBreakdown(t, path)
		})
	}
}

func runRealPoolsideBreakdown(t *testing.T, path string) {
	t.Helper()
	sess, msgs, events, err := parsePoolsideSession(path, "", "realdata-validation")
	require.NoError(t, err)
	require.NotNil(t, sess)

	type modelAgg struct {
		input      int
		output     int
		cacheRead  int
		cacheWrite int
		count      int
	}
	perModel := map[string]*modelAgg{}

	// Chronological model timeline at inference granularity. Used
	// for switch detection and spot-printing per-message changes.
	type inferEvt struct {
		ts    string
		model string
	}
	var timeline []inferEvt

	for _, ev := range events {
		agg, ok := perModel[ev.Model]
		if !ok {
			agg = &modelAgg{}
			perModel[ev.Model] = agg
		}
		agg.input += ev.InputTokens
		agg.output += ev.OutputTokens
		agg.cacheRead += ev.CacheReadInputTokens
		agg.cacheWrite += ev.CacheCreationInputTokens
		agg.count++
		timeline = append(timeline, inferEvt{ts: ev.OccurredAt, model: ev.Model})
	}

	// Switch detection: every time the model changes counts as
	// one switch (the very first model is also a "switch in").
	switches := 0
	lastModel := ""
	for _, ev := range timeline {
		if ev.model != lastModel {
			switches++
			lastModel = ev.model
		}
	}

	// Report per-file.
	fmt.Printf("\n=== %s ===\n", filepath.Base(path))
	fmt.Printf("  session_id:    %s\n", sess.ID)
	fmt.Printf("  started_at:    %v\n", sess.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Printf("  ended_at:      %v\n", sess.EndedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Printf("  messages:      %d  (user=%d)\n", sess.MessageCount, sess.UserMessageCount)
	fmt.Printf("  termination:   %s\n", sess.TerminationStatus)
	fmt.Printf("  inferences:    %d per-inference events emitted\n", len(events))
	fmt.Printf("  peak_input:    %d\n", sess.PeakContextTokens)
	fmt.Printf("  output_total:  %d (sum across all inferences)\n", sess.TotalOutputTokens)
	fmt.Printf("  model switches: %d (midsession changes in chat-completion model)\n", switches)

	fmt.Printf("\n  per-model breakdown:\n")
	modelNames := make([]string, 0, len(perModel))
	for m := range perModel {
		modelNames = append(modelNames, m)
	}
	sort.Strings(modelNames)
	for _, m := range modelNames {
		a := perModel[m]
		fmt.Printf("    %-32s  inferences=%-4d  input=%-9d  output=%-8d  cache_read=%-9d  cache_write=%-8d\n",
			m, a.count, a.input, a.output, a.cacheRead, a.cacheWrite)
	}

	// Cross-check: per-model output sums must equal the
	// session-level TotalOutputTokens. PeakContextTokens is the
	// max input_tokens of any single inference, not the cumulative
	// sum, so input is not aggregated here.
	var sumOut int
	for _, a := range perModel {
		sumOut += a.output
	}
	require.Equal(t, sess.TotalOutputTokens, sumOut,
		"TotalOutputTokens session field must equal the sum across per-model events")

	// Show the first 10 transitions when switches occurred.
	if switches > 1 {
		fmt.Printf("\n  chronological model transitions (first 10):\n")
		shown := 0
		last := ""
		for _, ev := range timeline {
			if ev.model != last {
				fmt.Printf("    [%s]  -> %s\n", ev.ts, ev.model)
				last = ev.model
				shown++
				if shown >= 10 {
					break
				}
			}
		}
	}

	// Models that will surface as unpriced: lagoon-* are absent
	// from the LiteLLM catalog, so the cost engine will list them
	// under UnpricedModels with HasCost=false.
	fmt.Printf("\n  UnpricedModels (cost engine will report unpriced): %v\n", modelNames)

	// Per-message Model stamping: every assistant message whose
	// owning turn includes at least one inference should carry a
	// model. Assistant messages with no Model in real data have
	// a turn that contains no inference cycle (rare).
	asstStamped, asstUnstamped := 0, 0
	for _, m := range msgs {
		if m.Role != RoleAssistant {
			continue
		}
		if m.Model != "" {
			asstStamped++
		} else {
			asstUnstamped++
		}
	}
	fmt.Printf("  assistant msg model stamped: %d / %d  (unstamped: %d)\n",
		asstStamped, asstStamped+asstUnstamped, asstUnstamped)

	// Spot-check that events emitted are tagged with Source: "inference"
	// and have non-empty DedupKeys.
	require.NotEmpty(t, events, "real trajectory must have at least one inference event")
	for i, ev := range events {
		require.Equal(t, "inference", ev.Source,
			"event %d must be Source=inference, got %q", i, ev.Source)
		require.NotEmpty(t, ev.DedupKey,
			"event %d must have a dedup_key", i)
		require.Contains(t, ev.SessionID, sess.ID,
			"event %d SessionID must match canonical session ID", i)
	}
	fmt.Printf("  event shape (Source, DedupKey, SessionID): all %d events pass\n", len(events))
}
