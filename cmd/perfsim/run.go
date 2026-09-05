package main

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"go.kenn.io/agentsview/internal/db"
	syncengine "go.kenn.io/agentsview/internal/sync"
)

type observation struct {
	CompletedUpdates int     `json:"completed_updates"`
	Phase            string  `json:"phase"`
	Operation        string  `json:"operation"`
	Milliseconds     float64 `json:"ms"`
	AllocatedBytes   uint64  `json:"allocated_bytes"`
	Allocations      uint64  `json:"allocations"`
	HeapBytes        uint64  `json:"heap_bytes"`
}
type report struct {
	Provenance       buildProvenance `json:"provenance"`
	completedUpdates int
	Options          options       `json:"options"`
	GoVersion        string        `json:"go_version"`
	GOOS             string        `json:"goos"`
	GOARCH           string        `json:"goarch"`
	Observations     []observation `json:"observations"`
	Initial          db.Stats      `json:"initial_stats"`
	Final            db.Stats      `json:"final_stats"`
}

func (r *report) measure(phase, name string, fn func() error) error {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := fn()
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	r.Observations = append(r.Observations, observation{CompletedUpdates: r.completedUpdates, Phase: phase, Operation: name, Milliseconds: float64(elapsed) / float64(time.Millisecond), AllocatedBytes: after.TotalAlloc - before.TotalAlloc, Allocations: after.Mallocs - before.Mallocs, HeapBytes: after.HeapAlloc})
	if err != nil {
		return fmt.Errorf("%s %s: %w", phase, name, err)
	}
	return nil
}

func run(ctx context.Context, o options, dir string) (report, error) {
	r := report{Options: o}
	sources, roots, err := corpus(dir, o)
	if err != nil {
		return r, err
	}
	if sources[0].Store != nil {
		defer sources[0].Store.Close()
	}
	database, err := db.OpenIsolated(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return r, err
	}
	defer database.Close()
	engine := syncengine.NewEngine(database, syncengine.EngineConfig{AgentDirs: roots, Machine: "simulation", DisableFilesystemProjectDiscovery: true})
	defer engine.Close()
	if err := r.measure("cold", "ingest", func() error {
		stats := engine.SyncAll(ctx, nil)
		if stats.Failed != 0 || stats.Aborted {
			return fmt.Errorf("ingest failed: %+v", stats)
		}
		return ctx.Err()
	}); err != nil {
		return r, err
	}
	r.Initial, err = database.GetStats(ctx, false, false)
	if err != nil {
		return r, err
	}
	initialMessages := o.Sessions * o.Turns * 2
	if o.ActiveTurns > 0 {
		initialMessages += o.Active * (o.ActiveTurns - o.Turns) * 2
	}
	if r.Initial.SessionCount != o.Sessions || r.Initial.MessageCount != initialMessages {
		return r, fmt.Errorf("parsed corpus mismatch: got %d sessions/%d messages, want %d/%d", r.Initial.SessionCount, r.Initial.MessageCount, o.Sessions, initialMessages)
	}
	filter := db.AnalyticsFilter{From: "2026-06-01", To: "2026-06-30", Timezone: "UTC"}
	queries := []struct {
		name string
		run  func() error
	}{
		{"stats", func() error { _, err := database.GetStats(ctx, false, false); return err }},
		{"sidebar", func() error {
			_, err := database.GetSidebarSessionIndex(ctx, db.SessionFilter{IncludeChildren: true})
			return err
		}},
		{"analytics_summary", func() error { _, err := database.GetAnalyticsSummary(ctx, filter); return err }},
		{"analytics_activity", func() error { _, err := database.GetAnalyticsActivity(ctx, filter, "day"); return err }},
		{"daily_usage", func() error {
			_, err := database.GetDailyUsage(ctx, db.UsageFilter{From: filter.From, To: filter.To, Timezone: "UTC"})
			return err
		}},
		{"search", func() error { _, err := database.Search(ctx, db.SearchFilter{Query: "latency", Limit: 50}); return err }},
	}
	for _, q := range queries {
		if err := r.measure("cold", q.name, q.run); err != nil {
			return r, err
		}
	}
	stopProfile, err := cpuProfile(filepath.Join(o.Output, "steady.cpu.pprof"))
	if err != nil {
		return r, err
	}
	defer stopProfile()
	// No sleep or filesystem watcher debounce is included: each measurement
	// exercises a completed engine batch, making equal workloads comparable.
	for i := 0; i < o.Iterations; i++ {
		if err := ctx.Err(); err != nil {
			return r, err
		}

		var changed []string
		for j := 0; j < o.Active; j++ {
			s := &sources[j]
			if err := s.appendTurns(1, o.ContentBytes); err != nil {
				return r, err
			}
			if s.Store == nil || len(changed) == 0 {
				changed = append(changed, s.Path)
			}
		}
		if err := r.measure("active", "append", func() error { return engine.SyncPathsContext(ctx, changed) }); err != nil {
			return r, err
		}
		r.completedUpdates = i + 1
		if o.SourceFormat == "opencode" {
			if err := syncSQLiteChildEdits(ctx, &r, engine, database, sources[:o.Active]); err != nil {
				return r, err
			}
		}
		if o.ReconcileEvery > 0 && (i+1)%o.ReconcileEvery == 0 {
			if o.SourceFormat == "opencode" {
				if err := measureSQLiteScans(ctx, &r, sources, o.Active); err != nil {
					return r, err
				}
				if err := r.measure("warm", "container_event", func() error {
					return engine.SyncPathsContext(ctx, []string{sources[0].Path})
				}); err != nil {
					return r, err
				}
			}
			if err := r.measure("warm", "reconcile", func() error {
				stats, _, err := engine.ReconcileWatchRootsWithStats(ctx, nil, true, nil)
				if err != nil {
					return err
				}
				if stats.Synced != 0 {
					return fmt.Errorf("unchanged sources resynced: %d", stats.Synced)
				}
				return nil
			}); err != nil {
				return r, err
			}
		}
		if o.QueryEvery > 0 && (i+1)%o.QueryEvery == 0 {
			for _, q := range queries {
				if err := r.measure("active", q.name, q.run); err != nil {
					return r, err
				}
			}
		}
	}
	if o.SourceFormat == "opencode" {
		if err := sources[0].Store.Close(); err != nil {
			return r, err
		}
		for range 3 {
			if err := r.measure("recovery", "closed_writer_event", func() error {
				return engine.SyncPathsContext(ctx, []string{sources[0].Path + "-wal"})
			}); err != nil {
				return r, err
			}
		}
	}
	r.Final, err = database.GetStats(ctx, false, false)
	if err != nil {
		return r, err
	}
	wantMessages := initialMessages + o.Active*o.Iterations*2
	if r.Final.SessionCount != o.Sessions || r.Final.MessageCount != wantMessages {
		return r, fmt.Errorf("append lost data: got %d sessions/%d messages, want %d/%d", r.Final.SessionCount, r.Final.MessageCount, o.Sessions, wantMessages)
	}
	return r, nil
}
