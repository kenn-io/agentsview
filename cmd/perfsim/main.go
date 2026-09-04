// Command perfsim measures parsed synthetic sessions through the real sync and query paths.
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
)

type options struct {
	SourceFormat   string `json:"source_format"`
	Sessions       int    `json:"sessions"`
	Turns          int    `json:"turns_per_session"`
	ActiveTurns    int    `json:"active_session_turns"`
	Active         int    `json:"active_sessions"`
	Iterations     int    `json:"iterations"`
	Empty          int    `json:"empty_sources"`
	ReconcileEvery int    `json:"reconcile_every"`
	QueryEvery     int    `json:"query_every"`
	ContentBytes   int    `json:"message_bytes"`
	Output         string `json:"-"`
	Keep           bool   `json:"-"`
}

func main() {
	o := options{}
	flag.StringVar(&o.SourceFormat, "source-format", "jsonl", "Source layout: jsonl (Claude/Codex) or opencode (SQLite)")
	flag.IntVar(&o.Sessions, "sessions", 1000, "Total sessions in the selected source format")
	flag.IntVar(&o.Turns, "turns", 20, "User/assistant pairs per session")
	flag.IntVar(&o.ActiveTurns, "active-turns", 0, "Initial turns in active sessions; 0 uses --turns")
	flag.IntVar(&o.Active, "active", 2, "Sessions receiving one pair per iteration")
	flag.IntVar(&o.Iterations, "iterations", 10, "Warm reconciliation, append, and query repetitions")
	flag.IntVar(&o.Empty, "empty", 0, "Additional empty Claude source files")
	flag.IntVar(&o.ReconcileEvery, "reconcile-every", 1, "Reconcile every N iterations; 0 isolates appends")
	flag.IntVar(&o.QueryEvery, "query-every", 1, "Run analytics every N iterations; 0 isolates sync")
	flag.IntVar(&o.ContentBytes, "message-bytes", 512, "Approximate content bytes per message")
	flag.StringVar(&o.Output, "output-dir", "", "New output directory; defaults to a temporary directory")
	flag.BoolVar(&o.Keep, "keep-data", false, "Keep the synthetic archive and sources for inspection")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, o options) error {
	if o.SourceFormat != "jsonl" && o.SourceFormat != "opencode" {
		return fmt.Errorf("source-format must be jsonl or opencode")
	}
	if o.SourceFormat == "opencode" && o.Empty != 0 {
		return fmt.Errorf("empty sources apply only to the jsonl workload")
	}
	if o.Sessions < 2 || o.Turns < 1 || o.ActiveTurns < 0 || o.Active < 1 || o.Active > o.Sessions || o.Iterations < 1 || o.Empty < 0 || o.ReconcileEvery < 0 || o.QueryEvery < 0 || o.ContentBytes < 1 {
		return fmt.Errorf("sessions >= 2, turns/iterations/message-bytes >= 1, 1 <= active <= sessions, and active-turns/empty/reconcile-every/query-every >= 0 are required")
	}
	if o.Output == "" {
		var err error
		o.Output, err = os.MkdirTemp("", "agentsview-perfsim-")
		if err != nil {
			return err
		}
	} else if err := os.Mkdir(o.Output, 0o700); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Performance artifacts:", o.Output)
	data := filepath.Join(o.Output, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		return err
	}
	if !o.Keep {
		defer os.RemoveAll(data)
	}
	report, err := run(ctx, o, data)
	if err != nil {
		return err
	}
	report.GoVersion = runtime.Version()
	report.GOOS = runtime.GOOS
	report.GOARCH = runtime.GOARCH
	encoded, err := json.Marshal(report, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.Output, "results.json"), append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	var bench strings.Builder
	bench.WriteString("pkg: go.kenn.io/agentsview/cmd/perfsim\n")
	for _, v := range report.Observations {
		if v.Phase == "cold" {
			continue
		}
		fmt.Fprintf(&bench, "BenchmarkPerfsim_%s_%s 1 %.0f ns/op %d B/op %d allocs/op\n", v.Phase, v.Operation, v.Milliseconds*1e6, v.AllocatedBytes, v.Allocations)
	}
	return os.WriteFile(filepath.Join(o.Output, "bench.txt"), []byte(bench.String()), 0o600)
}

func cpuProfile(path string) (func(), error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { pprof.StopCPUProfile(); f.Close() }, nil
}
