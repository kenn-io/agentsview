package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

// TestRealDBUsagePayload measures the JSON payload the dashboard must
// serialize, transfer, parse, and render — the cost query timing hides.
//
//	REAL_DB=/Users/wesm/.agentsview/sessions.db \
//	  CGO_ENABLED=1 go test -tags fts5 -run TestRealDBUsagePayload \
//	  -v -timeout 600s ./internal/db/
func TestRealDBUsagePayload(t *testing.T) {
	path := os.Getenv("REAL_DB")
	if path == "" {
		t.Skip("set REAL_DB to the sessions.db path to run")
	}
	reader, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	reader.SetMaxOpenConns(4)
	defer reader.Close()
	d := &DB{path: path}
	d.reader.Store(reader)
	ctx := context.Background()
	tz := "America/New_York"

	f := UsageFilter{From: "2000-01-01", To: "2035-01-01", Timezone: tz, Breakdowns: true}
	r, err := d.GetDailyUsage(ctx, f)
	if err != nil {
		t.Fatalf("GetDailyUsage: %v", err)
	}
	var proj, agent, model int
	for _, day := range r.Daily {
		proj += len(day.ProjectBreakdowns)
		agent += len(day.AgentBreakdowns)
		model += len(day.ModelBreakdowns)
	}
	start := time.Now()
	blob, _ := json.Marshal(r.Daily)
	t.Logf("summary .Daily: %d days, breakdown entries: %d project + %d agent + %d model",
		len(r.Daily), proj, agent, model)
	t.Logf("summary .Daily JSON: %.2f MB, marshal=%s",
		float64(len(blob))/1e6, round(time.Since(start)))

	ix, err := d.GetSidebarSessionIndex(ctx, SessionFilter{})
	if err != nil {
		t.Fatalf("sidebar: %v", err)
	}
	start = time.Now()
	sb, _ := json.Marshal(ix)
	t.Logf("sidebar-index: %d rows, JSON %.2f MB, marshal=%s",
		len(ix.Sessions), float64(len(sb))/1e6, round(time.Since(start)))
	if out := os.Getenv("DUMP_SIDEBAR"); out != "" {
		if err := dumpSidebarJSON(out, path, sb); err != nil {
			t.Fatalf("dump sidebar: %v", err)
		}
		t.Logf("wrote sidebar JSON to %s", out)
	}
}

// TestRealDBUsagePerf times every query the usage dashboard triggers,
// against a real prod DB. Gated behind REAL_DB so it never runs in CI.
//
//	REAL_DB=/Users/wesm/.agentsview/sessions.db \
//	  CGO_ENABLED=1 go test -tags fts5 -run TestRealDBUsagePerf \
//	  -v -timeout 1200s ./internal/db/
func TestRealDBUsagePerf(t *testing.T) {
	path := os.Getenv("REAL_DB")
	if path == "" {
		t.Skip("set REAL_DB to the sessions.db path to run")
	}

	// makeDSN(path, true) sets mode=ro: this connection cannot write.
	// No Open(), so no migrations / drops touch the archive.
	reader, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	reader.SetMaxOpenConns(4) // matches production reader pool
	defer reader.Close()

	d := &DB{path: path}
	d.reader.Store(reader)
	ctx := context.Background()
	tz := "America/New_York"

	walActive := fileExists(path + "-wal")
	t.Logf("DB=%s  reader_pool=4  wal_active=%v (writes in flight inflate reads)",
		path, walActive)

	allHist := UsageFilter{From: "2000-01-01", To: "2035-01-01", Timezone: tz}
	win30 := UsageFilter{
		From:     time.Now().AddDate(0, 0, -30).Format("2006-01-02"),
		To:       time.Now().Format("2006-01-02"),
		Timezone: tz,
	}

	// Each DB method the dashboard's load fan-out triggers. Two runs
	// per entry: run 1 is cold (disk), run 2 is warm (OS page cache).
	type probe struct {
		name string
		fn   func() (string, error)
	}
	probes := []probe{
		{"stats", func() (string, error) {
			s, err := d.GetStats(ctx, true, true)
			return fmt.Sprintf("%+v", s), err
		}},
		{"projects (GROUP BY sessions)", func() (string, error) {
			p, err := d.GetProjects(ctx, false, false)
			return fmt.Sprintf("%d projects", len(p)), err
		}},
		{"agents (GROUP BY sessions)", func() (string, error) {
			a, err := d.GetAgents(ctx, false, false)
			return fmt.Sprintf("%d agents", len(a)), err
		}},
		{"machines (DISTINCT)", func() (string, error) {
			m, err := d.GetMachines(ctx, false, false)
			return fmt.Sprintf("%d machines", len(m)), err
		}},
		{"sidebar-index (all sessions)", func() (string, error) {
			ix, err := d.GetSidebarSessionIndex(ctx, SessionFilter{})
			return fmt.Sprintf("%d rows", len(ix.Sessions)), err
		}},
		{"usage/summary: GetDailyUsage allHist (breakdowns)", func() (string, error) {
			f := allHist
			f.Breakdowns = true
			r, err := d.GetDailyUsage(ctx, f)
			return fmt.Sprintf("%d days, %s", len(r.Daily), money.FormatUSD(r.Totals.TotalCost, money.DisplayCents)), err
		}},
		{"usage/session-counts diagnostic allHist (not live path)", func() (string, error) {
			c, err := d.GetUsageSessionCounts(ctx, allHist)
			return fmt.Sprintf("%d sessions", c.Total), err
		}},
		{"usage/comparison: GetDailyUsage prior-window", func() (string, error) {
			// prior period for an all-history view: empty window far in past
			f := UsageFilter{From: "1900-01-01", To: "1999-12-31", Timezone: tz}
			r, err := d.GetDailyUsage(ctx, f)
			return fmt.Sprintf("%d days", len(r.Daily)), err
		}},
		{"usage/top-sessions: GetTopSessionsByCost allHist", func() (string, error) {
			e, err := d.GetTopSessionsByCost(ctx, allHist, 20)
			return fmt.Sprintf("%d rows", len(e)), err
		}},
		{"usage/summary: GetDailyUsage 30d (breakdowns)", func() (string, error) {
			f := win30
			f.Breakdowns = true
			r, err := d.GetDailyUsage(ctx, f)
			return fmt.Sprintf("%d days, %s", len(r.Daily), money.FormatUSD(r.Totals.TotalCost, money.DisplayCents)), err
		}},
		{"usage/top-sessions: GetTopSessionsByCost 30d", func() (string, error) {
			e, err := d.GetTopSessionsByCost(ctx, win30, 20)
			return fmt.Sprintf("%d rows", len(e)), err
		}},
	}

	t.Logf("%-52s  %10s  %10s  %s", "QUERY (isolated)", "cold", "warm", "result")
	for _, p := range probes {
		var cold, warm time.Duration
		var info string
		for run := range 2 {
			start := time.Now()
			res, err := p.fn()
			d := time.Since(start)
			if err != nil {
				t.Fatalf("%s: %v", p.name, err)
			}
			if run == 0 {
				cold, info = d, res
			} else {
				warm = d
			}
		}
		t.Logf("%-52s  %10s  %10s  %s",
			p.name, round(cold), round(warm), info)
	}

	// Concurrent pattern 1: usage.fetchAll() = summary + comparison +
	// top-sessions firing at once (3 live dashboard endpoints, 4-conn pool).
	t.Logf("")
	timeConcurrent(t, "CONCURRENT fetchAll (summary+comparison+top, allHist)", []func() error{
		func() error { f := allHist; f.Breakdowns = true; _, e := d.GetDailyUsage(ctx, f); return e },
		func() error {
			f := UsageFilter{From: "1900-01-01", To: "1999-12-31", Timezone: tz}
			_, e := d.GetDailyUsage(ctx, f)
			return e
		},
		func() error { _, e := d.GetTopSessionsByCost(ctx, allHist, 20); return e },
	})

	// Concurrent pattern 2: the full page-open fan-out (everything the
	// browser fires when you click the Usage tab), through one pool.
	timeConcurrent(t, "CONCURRENT full page-open fan-out (8 endpoints, allHist)", []func() error{
		func() error { _, e := d.GetStats(ctx, true, true); return e },
		func() error { _, e := d.GetProjects(ctx, false, false); return e },
		func() error { _, e := d.GetAgents(ctx, false, false); return e },
		func() error { _, e := d.GetMachines(ctx, false, false); return e },
		func() error { _, e := d.GetSidebarSessionIndex(ctx, SessionFilter{}); return e },
		func() error { f := allHist; f.Breakdowns = true; _, e := d.GetDailyUsage(ctx, f); return e },
		func() error {
			f := UsageFilter{From: "1900-01-01", To: "1999-12-31", Timezone: tz}
			_, e := d.GetDailyUsage(ctx, f)
			return e
		},
		func() error { _, e := d.GetTopSessionsByCost(ctx, allHist, 20); return e },
	})
}

func TestDumpSidebarJSONRejectsDBAndSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("db"), 0o644))

	for _, out := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		t.Run(filepath.Base(out), func(t *testing.T) {
			err := dumpSidebarJSON(out, dbPath, []byte(`{"sessions":[]}`))
			require.Error(t, err)

			got, readErr := os.ReadFile(dbPath)
			require.NoError(t, readErr)
			assert.Equal(t, "db", string(got))
		})
	}
}

func TestDumpSidebarJSONDoesNotClobberExistingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	outPath := filepath.Join(dir, "sidebar.json")
	require.NoError(t, os.WriteFile(dbPath, []byte("db"), 0o644))
	require.NoError(t, os.WriteFile(outPath, []byte("existing"), 0o644))

	err := dumpSidebarJSON(outPath, dbPath, []byte(`{"sessions":[]}`))
	require.Error(t, err)

	got, readErr := os.ReadFile(outPath)
	require.NoError(t, readErr)
	assert.Equal(t, "existing", string(got))
}

func TestDumpSidebarJSONCreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	outPath := filepath.Join(dir, "sidebar.json")
	payload := []byte(`{"sessions":[]}`)
	require.NoError(t, os.WriteFile(dbPath, []byte("db"), 0o644))

	require.NoError(t, dumpSidebarJSON(outPath, dbPath, payload))

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func dumpSidebarJSON(out, dbPath string, payload []byte) error {
	if err := rejectSidebarDumpPath(out, dbPath); err != nil {
		return err
	}

	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create sidebar dump %q: %w", out, err)
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return fmt.Errorf("write sidebar dump %q: %w", out, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close sidebar dump %q: %w", out, err)
	}
	return nil
}

func rejectSidebarDumpPath(out, dbPath string) error {
	outPath, err := cleanAbsPath(out)
	if err != nil {
		return fmt.Errorf("resolve sidebar dump path: %w", err)
	}
	for _, forbidden := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		forbiddenPath, err := cleanAbsPath(forbidden)
		if err != nil {
			return fmt.Errorf("resolve protected DB path %q: %w", forbidden, err)
		}
		if outPath == forbiddenPath {
			return fmt.Errorf("refusing to write sidebar dump over protected DB path %q", out)
		}
	}
	return nil
}

func cleanAbsPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func timeConcurrent(t *testing.T, label string, fns []func() error) {
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, len(fns))
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			errs[i] = fn()
		}(i, fn)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatalf("%s: %v", label, e)
		}
	}
	t.Logf("%-52s  wall=%s", label, round(time.Since(start)))
}

func round(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
