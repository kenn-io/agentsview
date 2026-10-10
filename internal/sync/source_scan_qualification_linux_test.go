package sync

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// This opt-in experiment uses real parsers, disposable SQLite archives, and
// the production scanner. It does not read local transcripts or live archives.
// Build a standalone tester with go test -c and run this test by name.
func TestSourceScanQualification(t *testing.T) {
	if os.Getenv("AGENTSVIEW_SOURCE_SCAN_QUALIFY") != "1" {
		t.Skip("opt-in production source-scan experiment")
	}
	count := 50000
	if value := os.Getenv("AGENTSVIEW_SOURCE_SCAN_FILES"); value != "" {
		var err error
		count, err = strconv.Atoi(value)
		require.NoError(t, err)
		require.GreaterOrEqual(t, count, 100)
	}
	fixture := newSourceScanQualification(t, count)
	report := sourceScanQualificationReport{Files: count, Revision: os.Getenv("AGENTSVIEW_SOURCE_SCAN_REVISION"), GoVersion: runtime.Version(), CoverageIntervalSeconds: 30, PriorPollIntervalSeconds: 120}
	baseline := fixture.engine(t)
	candidate := fixture.engine(t)
	for _, method := range []struct {
		name   string
		engine *Engine
	}{{"poll", baseline}, {"scan", candidate}} {
		sample := qualifySourceScanMeasure(t, "initial-import", method.name, func() {
			stats := method.engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			require.Equal(t, count, stats.Synced)
		})
		report.Samples = append(report.Samples, sample)
	}
	scanner := newSourceScanner(fixture.roots, nil)
	emit := func(ctx context.Context, batch WatchBatch) error { return ApplyWatchBatch(ctx, candidate, batch, nil) }
	poll := func() { require.NoError(t, baseline.ReconcileProviderRootsGrouped(t.Context(), fixture.groups)) }
	scan := func() { require.NoError(t, scanner.scan(t.Context(), emit)) }
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "scanner-startup-extra", "scan", scan))
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	report.ScannerRetainedBytes = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	report.ScannerStats = scanner.stats()
	probe := count - 1
	if report.ScannerStats.Saturated {
		probe = fixture.uncachedSource(scanner)
		require.GreaterOrEqual(t, probe, 0)
	}
	for _, root := range scanner.roots {
		_, cached := root.directories[filepath.Dir(fixture.paths[probe])][filepath.Base(fixture.paths[probe])]
		report.ProbeSourceCached = report.ProbeSourceCached || cached
	}
	// Alternate order to avoid always giving one path the warmer filesystem.
	for i := range 5 {
		if i%2 == 0 {
			report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "idle", "poll", poll), qualifySourceScanMeasure(t, "idle", "scan", scan))
		} else {
			report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "idle", "scan", scan), qualifySourceScanMeasure(t, "idle", "poll", poll))
		}
	}
	for cycle := range 5 {
		for i := range 20 {
			fixture.append(t, i, fmt.Sprintf("sparse-%d", cycle))
		}
		report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "sparse-20", "scan", scan), qualifySourceScanMeasure(t, "sparse-20", "poll", poll))
		for i := range 20 {
			fixture.verify(t, baseline.db, i, cycle+2, fmt.Sprintf("sparse-%d", cycle))
			fixture.verify(t, candidate.db, i, cycle+2, fmt.Sprintf("sparse-%d", cycle))
		}
	}
	burst := min(count, 512)
	for i := range burst {
		fixture.append(t, i, "burst")
	}
	report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "burst-512", "poll", poll), qualifySourceScanMeasure(t, "burst-512", "scan", scan))
	for i := range burst {
		messages := 2
		if i < 20 {
			messages = 7
		}
		fixture.verify(t, baseline.db, i, messages, "burst")
		fixture.verify(t, candidate.db, i, messages, "burst")
	}
	// Exact-stat atomic replacement and title companion updates exercise source
	// identity and provider fan-out without injecting a native notice.
	info, err := os.Stat(fixture.paths[0])
	require.NoError(t, err)
	replacement := filepath.Join(filepath.Dir(fixture.paths[0]), "replacement.tmp")
	content, err := os.ReadFile(fixture.paths[0])
	require.NoError(t, err)
	content = bytes.ReplaceAll(content, []byte(`"burst"`), []byte(`"other"`))
	require.NoError(t, os.WriteFile(replacement, content, 0o600))
	require.NoError(t, os.Chtimes(replacement, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Rename(replacement, fixture.paths[0]))
	fixture.lastUUIDs[0] = "other"
	index := filepath.Join(fixture.codexHome, parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(index, []byte(fmt.Sprintf("{\"id\":\"%s\",\"thread_name\":\"Qualification rename\",\"updated_at\":\"2026-10-05T12:00:00Z\"}\n", fixture.rawIDs[1])), 0o600))
	report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "replacement-and-title", "scan", scan), qualifySourceScanMeasure(t, "replacement-and-title", "poll", poll))
	for _, engine := range []*Engine{baseline, candidate} {
		fixture.verify(t, engine.db, 0, 7, "other")
		session, err := engine.db.GetSession(t.Context(), fixture.ids[1])
		require.NoError(t, err)
		require.NotNil(t, session.DisplayName)
		require.Equal(t, "Qualification rename", *session.DisplayName)
	}

	// A reported native loss must force content verification even when the
	// writer restores size and mtime on the same inode.
	lostInfo, err := os.Stat(fixture.paths[4])
	require.NoError(t, err)
	lostContent, err := os.ReadFile(fixture.paths[4])
	require.NoError(t, err)
	lostContent = bytes.ReplaceAll(lostContent, []byte(`"burst"`), []byte(`"other"`))
	require.NoError(t, os.WriteFile(fixture.paths[4], lostContent, 0o600))
	require.NoError(t, os.Chtimes(fixture.paths[4], lostInfo.ModTime(), lostInfo.ModTime()))
	fixture.lastUUIDs[4] = "other"
	for _, method := range []struct {
		name   string
		engine *Engine
	}{{"scan", candidate}, {"poll", baseline}} {
		report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "reported-loss-equal-stat", method.name, func() {
			require.NoError(t, ApplyWatchBatch(t.Context(), method.engine, WatchBatch{ReconcileRoots: fixture.configuredRoots, LostEvents: true}, nil))
		}))
		fixture.verify(t, method.engine.db, 4, 7, "other")
	}
	require.NoError(t, os.Remove(fixture.paths[2]))
	report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "delete-one", "scan", scan), qualifySourceScanMeasure(t, "delete-one", "poll", poll))
	for _, engine := range []*Engine{baseline, candidate} {
		session, err := engine.db.GetSessionFull(t.Context(), fixture.ids[2])
		require.NoError(t, err)
		require.NotNil(t, session)
		require.NotNil(t, session.SourceMissingAt, "complete provider discovery must record source disappearance")
	}
	// Directory enumeration order does not guarantee that the final fixture
	// path is uncached. Use the source selected from actual cache membership.
	tail := probe
	fixture.append(t, tail, "tail-change")
	report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "tail-change", "scan", scan), qualifySourceScanMeasure(t, "tail-change", "poll", poll))
	tailMessages := 2
	if tail < burst {
		tailMessages++
	}
	for _, engine := range []*Engine{baseline, candidate} {
		fixture.verify(t, engine.db, tail, tailMessages, "tail-change")
	}
	require.NoError(t, os.Remove(fixture.paths[tail]))
	report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "tail-delete", "scan", scan), qualifySourceScanMeasure(t, "tail-delete", "poll", poll))
	for _, engine := range []*Engine{baseline, candidate} {
		session, err := engine.db.GetSessionFull(t.Context(), fixture.ids[tail])
		require.NoError(t, err)
		require.NotNil(t, session)
		require.NotNil(t, session.SourceMissingAt)
	}
	report.UncachedSourceChecksPassed = !report.ProbeSourceCached
	// Retention/churn: repeat changed passes and report forced-GC heap after
	// warmup and after 20 rounds, with the same fixture and archive cardinality.
	runtime.GC()
	runtime.ReadMemStats(&before)
	for cycle := range 20 {
		fixture.append(t, 3, fmt.Sprintf("retention-%d", cycle))
		scan()
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	report.RetentionHeapDeltaBytes = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	fixture.verify(t, candidate.db, 3, 27, "retention-19")
	// Use the real native backend and production dispatch floor. Its initial
	// coverage must complete, then a covered source append reaches SQLite.
	watcher, err := NewWatcherWithCallback(500*time.Millisecond, 5*time.Second,
		func(ctx context.Context, batch WatchBatch) error {
			return ApplyWatchBatch(ctx, candidate, batch, &WatchRecoveryScope{AvailableRoots: fixture.configuredRoots})
		}, nil, WatcherOptions{})
	require.NoError(t, err)
	watcher.RegisterRoots(fixture.roots, 64)
	require.NoError(t, watcher.Start())
	t.Cleanup(watcher.Stop)
	require.Eventually(t, func() bool { return watcher.SourceScanStats().Passes > 0 }, 30*time.Second, 10*time.Millisecond)
	require.Zero(t, watcher.SourceScanStats().Failures)
	nativeStarted := time.Now()
	fixture.append(t, 0, "native-delivery")
	require.Eventually(t, func() bool {
		messages, err := candidate.db.GetMessages(t.Context(), fixture.ids[0], 0, 100, true)
		return err == nil && len(messages) == 8 && messages[7].Content == "native-delivery"
	}, 15*time.Second, 10*time.Millisecond)
	report.NativeCommitLatencyMS = float64(time.Since(nativeStarted).Microseconds()) / 1000
	watcher.Stop()
	report.ScannerStats = scanner.stats()
	report.CorrectnessChecksPassed = true
	writeSourceScanQualificationReport(t, report)
}

// The capacity override belongs only to this experiment. It tests whether
// saturation, rather than the larger collection itself, causes repeated work.
func TestSourceScanCapacityControl(t *testing.T) {
	if os.Getenv("AGENTSVIEW_SOURCE_SCAN_CAPACITY_CONTROL") != "1" {
		t.Skip("opt-in source-scan capacity control")
	}
	count := 70000
	if value := os.Getenv("AGENTSVIEW_SOURCE_SCAN_FILES"); value != "" {
		var err error
		count, err = strconv.Atoi(value)
		require.NoError(t, err)
		require.GreaterOrEqual(t, count, 100)
	}
	fixture := newSourceScanQualification(t, count)
	engine := fixture.engine(t)
	report := sourceScanQualificationReport{Files: count, Revision: os.Getenv("AGENTSVIEW_SOURCE_SCAN_REVISION"), GoVersion: runtime.Version(), CoverageIntervalSeconds: 30, PriorPollIntervalSeconds: 120}
	report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "initial-import", "control", func() {
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		require.Equal(t, count, stats.Synced)
	}))
	var scanners []*sourceScanner
	methods := []string{"production-capacity", "expanded-capacity-control"}
	for i, method := range methods {
		scanner := newSourceScanner(fixture.roots, nil)
		if i == 1 {
			scanner.maxEntries = max(scanner.maxEntries, count)
		}
		scanners = append(scanners, scanner)
		report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "scanner-startup-extra", method, func() {
			require.NoError(t, scanner.scan(t.Context(), func(ctx context.Context, batch WatchBatch) error {
				return ApplyWatchBatch(ctx, engine, batch, nil)
			}))
		}))
		require.Equal(t, count > scanner.maxEntries, scanner.stats().Saturated)
	}
	for cycle := range 5 {
		order := []int{0, 1}
		if cycle%2 != 0 {
			order = []int{1, 0}
		}
		for _, i := range order {
			report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "idle", methods[i], func() {
				require.NoError(t, scanners[i].scan(t.Context(), func(ctx context.Context, batch WatchBatch) error {
					return ApplyWatchBatch(ctx, engine, batch, nil)
				}))
			}))
		}
	}
	fixture.verify(t, engine.db, count-1, 1, "initial")
	report.ScannerStats = scanners[1].stats()
	require.Equal(t, uint64(count), report.ScannerStats.CachedFiles)
	if scanners[0].stats().Saturated {
		probe := fixture.uncachedSource(scanners[0])
		require.GreaterOrEqual(t, probe, 0)
		fixture.append(t, probe, "uncached-change")
		report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "uncached-change", methods[0], func() {
			require.NoError(t, scanners[0].scan(t.Context(), func(ctx context.Context, batch WatchBatch) error {
				return ApplyWatchBatch(ctx, engine, batch, nil)
			}))
		}))
		fixture.verify(t, engine.db, probe, 2, "uncached-change")
		require.NoError(t, os.Remove(fixture.paths[probe]))
		report.Samples = append(report.Samples, qualifySourceScanMeasure(t, "uncached-delete", methods[0], func() {
			require.NoError(t, scanners[0].scan(t.Context(), func(ctx context.Context, batch WatchBatch) error {
				return ApplyWatchBatch(ctx, engine, batch, nil)
			}))
		}))
		session, err := engine.db.GetSessionFull(t.Context(), fixture.ids[probe])
		require.NoError(t, err)
		require.NotNil(t, session)
		require.NotNil(t, session.SourceMissingAt)
		report.UncachedSourceChecksPassed = true
	}
	report.CorrectnessChecksPassed = true
	writeSourceScanQualificationReport(t, report)
}

func writeSourceScanQualificationReport(t *testing.T, report sourceScanQualificationReport) {
	t.Helper()
	encoded, err := json.Marshal(report, jsontext.WithIndent("  "), json.WithMarshalers(json.MarshalToFunc(func(enc *jsontext.Encoder, value time.Duration) error {
		return enc.WriteToken(jsontext.Int(int64(value)))
	})))
	require.NoError(t, err)
	if path := os.Getenv("AGENTSVIEW_SOURCE_SCAN_REPORT"); path != "" {
		require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
	}
	t.Logf("QUALIFICATION_REPORT\n%s", encoded)
}

type sourceScanQualificationReport struct {
	Files                      int
	Revision                   string
	GoVersion                  string
	ProbeSourceCached          bool
	UncachedSourceChecksPassed bool
	CoverageIntervalSeconds    int
	PriorPollIntervalSeconds   int
	ScannerRetainedBytes       int64
	RetentionHeapDeltaBytes    int64
	NativeCommitLatencyMS      float64
	ScannerStats               SourceScanStats
	CorrectnessChecksPassed    bool
	Samples                    []sourceScanQualificationSample
}

type sourceScanQualificationSample struct {
	Scenario       string
	Method         string
	WallMS         float64
	CPUMS          float64
	AllocatedBytes uint64
}

func qualifySourceScanMeasure(t *testing.T, scenario, method string, work func()) sourceScanQualificationSample {
	t.Helper()
	var before, after runtime.MemStats
	var cpuBefore, cpuAfter syscall.Rusage
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &cpuBefore))
	runtime.ReadMemStats(&before)
	started := time.Now()
	work()
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &cpuAfter))
	cpuUS := (cpuAfter.Utime.Sec-cpuBefore.Utime.Sec+cpuAfter.Stime.Sec-cpuBefore.Stime.Sec)*1000000 + cpuAfter.Utime.Usec - cpuBefore.Utime.Usec + cpuAfter.Stime.Usec - cpuBefore.Stime.Usec
	sample := sourceScanQualificationSample{Scenario: scenario, Method: method, WallMS: float64(elapsed.Microseconds()) / 1000, CPUMS: float64(cpuUS) / 1000, AllocatedBytes: after.TotalAlloc - before.TotalAlloc}
	t.Logf("phase=%s method=%s wall_ms=%.3f cpu_ms=%.3f allocated_bytes=%d", scenario, method, sample.WallMS, sample.CPUMS, sample.AllocatedBytes)
	return sample
}

type sourceScanQualificationFixture struct {
	paths, ids, rawIDs, configuredRoots, lastUUIDs []string
	codexHome                                      string
	dirs                                           map[parser.AgentType][]string
	roots                                          []WatchRoot
	groups                                         []ProviderRootsGroup
}

func newSourceScanQualification(t *testing.T, count int) *sourceScanQualificationFixture {
	t.Helper()
	base := t.TempDir()
	claude := filepath.Join(base, "projects")
	home := filepath.Join(base, "codex")
	codex := filepath.Join(home, "sessions")
	f := &sourceScanQualificationFixture{codexHome: home, configuredRoots: []string{claude, codex}, dirs: map[parser.AgentType][]string{parser.AgentClaude: {claude}, parser.AgentCodex: {codex}}}
	for i := range count {
		raw := fmt.Sprintf("019f0000-0000-7000-8000-%012d", i)
		var path, id, body string
		if i%2 == 0 {
			id = fmt.Sprintf("claude-%05d", i)
			path = filepath.Join(claude, fmt.Sprintf("project-%03d", i/256), id+".jsonl")
			body = testjsonl.NewSessionBuilder().AddClaudeUserWithUUID("2026-10-05T10:00:00Z", "initial", "first", "").String()
		} else {
			id = "codex:" + raw
			path = filepath.Join(codex, "2026", "10", fmt.Sprintf("%03d", i/256), "rollout-2026-10-05T10-00-00-"+raw+".jsonl")
			body = testjsonl.NewSessionBuilder().AddCodexMeta("2026-10-05T10:00:00Z", raw, "/workspace/project", "codex_cli_rs").AddCodexMessage("2026-10-05T10:00:01Z", "user", "initial").String()
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		f.paths = append(f.paths, path)
		f.ids = append(f.ids, id)
		f.rawIDs = append(f.rawIDs, raw)
		f.lastUUIDs = append(f.lastUUIDs, "first")
	}
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex} {
		f.groups = append(f.groups, ProviderRootsGroup{Agent: agent, Roots: f.dirs[agent]})
		provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: f.dirs[agent]})
		require.True(t, ok)
		plan, err := parser.ResolveWatchRoots(t.Context(), provider)
		require.NoError(t, err)
		for _, root := range plan {
			scopes := []WatchScope{{Agent: string(agent), SyncDir: f.dirs[agent][0]}}
			f.roots = append(f.roots, WatchRoot{Path: root.Path, Recursive: root.Recursive, Exists: true, SourceFileGlobs: root.SourceFileGlobs, FollowChildDirectorySymlinks: root.FollowChildDirectorySymlinks, Scopes: scopes, SourceScanScopes: scopes})
		}
	}
	return f
}

func (f *sourceScanQualificationFixture) uncachedSource(scanner *sourceScanner) int {
	for i, path := range f.paths {
		if i < 512 {
			continue // Keep burst and retention sources available for later checks.
		}
		cached := false
		for _, root := range scanner.roots {
			_, exists := root.directories[filepath.Dir(path)][filepath.Base(path)]
			cached = cached || exists
		}
		if !cached {
			return i
		}
	}
	return -1
}

func (f *sourceScanQualificationFixture) engine(t *testing.T) *Engine {
	t.Helper()
	engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{AgentDirs: f.dirs, Machine: "qualification"})
	t.Cleanup(engine.Close)
	return engine
}

func (f *sourceScanQualificationFixture) append(t *testing.T, i int, content string) {
	t.Helper()
	var record string
	if i%2 == 0 {
		record = testjsonl.NewSessionBuilder().AddClaudeUserWithUUID("2026-10-05T11:00:00Z", content, content, f.lastUUIDs[i]).String()
	} else {
		record = testjsonl.NewSessionBuilder().AddCodexMessage("2026-10-05T11:00:00Z", "assistant", content).String()
	}
	file, err := os.OpenFile(f.paths[i], os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString(record)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	if i%2 == 0 {
		f.lastUUIDs[i] = content
	}
}

func (f *sourceScanQualificationFixture) verify(t *testing.T, database *db.DB, i, count int, content string) {
	t.Helper()
	messages, err := database.GetMessages(t.Context(), f.ids[i], 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, count)
	require.Equal(t, content, messages[count-1].Content)
}
