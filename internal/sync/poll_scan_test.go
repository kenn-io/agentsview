package sync

import (
	"errors"
	"fmt"
	"hash/maphash"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type pollScanFixture struct {
	engine     *Engine
	database   *db.DB
	reconciled atomic.Int32 // Reconciled provider groups.
}

func newPollScanFixture(t *testing.T, dirs map[parser.AgentType][]string) *pollScanFixture {
	t.Helper()
	f := &pollScanFixture{database: openTestDB(t)}
	f.engine = NewEngine(t.Context(), f.database, EngineConfig{AgentDirs: dirs, Machine: "local"})
	t.Cleanup(f.engine.Close)
	f.engine.groupedReconcileGroupHook = func(int) { f.reconciled.Add(1) }
	return f
}

func (f *pollScanFixture) poll(t *testing.T, groups ...ProviderRootsGroup) {
	t.Helper()
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, false))
}

func (f *pollScanFixture) session(t *testing.T, id string) *db.Session {
	t.Helper()
	session, err := f.database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session
}

func writeClaudePollSession(t *testing.T, path string, prompts ...string) {
	t.Helper()
	builder := testjsonl.NewSessionBuilder()
	for i, prompt := range prompts {
		builder.AddClaudeUser(time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339), prompt)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))
}

func requirePollChangeTimes(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("this platform reports no change times, so polling reconciles every pass")
	}
}

func TestPollProviderRootsGroupedSyncsChangedFilesWithoutReconciling(t *testing.T) {
	requirePollChangeTimes(t)
	root := t.TempDir()
	path := filepath.Join(root, "project", "polled.jsonl")
	writeClaudePollSession(t, path, "first")
	f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {root}})
	group := ProviderRootsGroup{Agent: parser.AgentClaude, Roots: []string{root}}

	f.poll(t, group)
	require.EqualValues(t, 1, f.reconciled.Load(), "the first pass has nothing to compare against")
	assert.Equal(t, 1, f.session(t, "polled").UserMessageCount)

	writeClaudePollSession(t, path, "first", "second")
	f.poll(t, group)
	assert.EqualValues(t, 1, f.reconciled.Load(), "an append is synced by path")
	assert.Equal(t, 2, f.session(t, "polled").UserMessageCount)

	f.poll(t, group)
	assert.EqualValues(t, 1, f.reconciled.Load(), "an unchanged root needs no work")
}

func TestPollProviderRootsGroupedSeesEqualSizeRewriteWithRestoredMtime(t *testing.T) {
	requirePollChangeTimes(t)
	root := t.TempDir()
	path := filepath.Join(root, "project", "rewritten.jsonl")
	writeClaudePollSession(t, path, "before")
	info, err := os.Stat(path)
	require.NoError(t, err)
	f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {root}})
	group := ProviderRootsGroup{Agent: parser.AgentClaude, Roots: []string{root}}
	f.poll(t, group)

	writeClaudePollSession(t, path, "after!")
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
	rewritten, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, info.Size(), rewritten.Size())
	f.poll(t, group)

	session := f.session(t, "rewritten")
	require.NotNil(t, session.FirstMessage)
	assert.Equal(t, "after!", *session.FirstMessage)
	assert.EqualValues(t, 1, f.reconciled.Load())
}

func TestPollProviderRootsGroupedReconcilesVanishedFiles(t *testing.T) {
	requirePollChangeTimes(t)
	root := t.TempDir()
	kept := filepath.Join(root, "project", "kept.jsonl")
	removed := filepath.Join(root, "project", "removed.jsonl")
	writeClaudePollSession(t, kept, "kept")
	writeClaudePollSession(t, removed, "removed")
	f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {root}})
	group := ProviderRootsGroup{Agent: parser.AgentClaude, Roots: []string{root}}
	f.poll(t, group)
	require.Nil(t, f.session(t, "removed").SourceMissingAt)

	require.NoError(t, os.Remove(removed))
	f.poll(t, group)

	assert.EqualValues(t, 2, f.reconciled.Load(), "only reconciliation may mark a source missing")
	assert.NotNil(t, f.session(t, "removed").SourceMissingAt)
	assert.Nil(t, f.session(t, "kept").SourceMissingAt)
}

func TestPollProviderRootsGroupedSyncsCodexTitleIndexChange(t *testing.T) {
	requirePollChangeTimes(t)
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c12aaaa"
	rollout := filepath.Join(root, "2026", "08", "15", "rollout-2026-08-15T10-00-00-"+uuid+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(rollout), 0o755))
	require.NoError(t, os.WriteFile(rollout, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "codex_cli_rs", "2026-08-15T10:00:00Z"),
		testjsonl.CodexMsgJSON("user", "hello", "2026-08-15T10:00:01Z"),
	)), 0o600))
	index := filepath.Join(home, parser.CodexSessionIndexFilename)
	writeIndex := func(title string) {
		require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+uuid+`","thread_name":"`+title+`"}`+"\n"), 0o600))
	}
	writeIndex("Old title")
	f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentCodex: {root}})
	group := ProviderRootsGroup{Agent: parser.AgentCodex, Roots: []string{root}}
	f.poll(t, group)
	session := f.session(t, "codex:"+uuid)
	require.NotNil(t, session.SessionName)
	require.Equal(t, "Old title", *session.SessionName)

	writeIndex("New title, only in the index")
	f.poll(t, group)

	session = f.session(t, "codex:"+uuid)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "New title, only in the index", *session.SessionName)
	assert.EqualValues(t, 1, f.reconciled.Load(), "the index change is synced by path")
}

func TestPollProviderRootsGroupedKeepsSnapshotsOnlyAfterArchiveWork(t *testing.T) {
	requirePollChangeTimes(t)
	archiveDown := errors.New("archive unavailable")
	t.Run("failed path sync", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "project", "retried.jsonl")
		writeClaudePollSession(t, path, "first")
		f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {root}})
		group := ProviderRootsGroup{Agent: parser.AgentClaude, Roots: []string{root}}
		f.poll(t, group)

		writeClaudePollSession(t, path, "first", "second")
		f.engine.sourceSyncErr = archiveDown
		require.ErrorIs(t, f.engine.PollProviderRootsGrouped(t.Context(), []ProviderRootsGroup{group}, false), archiveDown)
		f.engine.sourceSyncErr = nil
		f.poll(t, group)

		assert.Equal(t, 2, f.session(t, "retried").UserMessageCount,
			"the unchanged file is still compared with the snapshot before the failure")
		assert.EqualValues(t, 1, f.reconciled.Load())
	})
	t.Run("failed reconciliation", func(t *testing.T) {
		root := t.TempDir()
		writeClaudePollSession(t, filepath.Join(root, "project", "reconciled.jsonl"), "first")
		f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {root}})
		group := ProviderRootsGroup{Agent: parser.AgentClaude, Roots: []string{root}}
		f.engine.sourceSyncErr = archiveDown
		require.ErrorIs(t, f.engine.PollProviderRootsGrouped(t.Context(), []ProviderRootsGroup{group}, false), archiveDown)
		f.engine.sourceSyncErr = nil

		f.poll(t, group)
		assert.EqualValues(t, 2, f.reconciled.Load(), "a failed first pass leaves nothing to compare against")
		assert.Equal(t, 1, f.session(t, "reconciled").UserMessageCount)
		f.poll(t, group)
		assert.EqualValues(t, 2, f.reconciled.Load())
	})
}

func TestPollProviderRootsGroupedSharesRowBudget(t *testing.T) {
	requirePollChangeTimes(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	for _, root := range []string{rootA, rootB} {
		for _, name := range []string{"one", "two"} {
			writeClaudePollSession(t, filepath.Join(root, "project", filepath.Base(root)+name+".jsonl"), name)
		}
	}
	f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {rootA, rootB}})
	f.engine.pollScans.maxRows = 3
	groups := []ProviderRootsGroup{
		{Agent: parser.AgentClaude, Roots: []string{rootA}},
		{Agent: parser.AgentClaude, Roots: []string{rootB}},
	}

	f.poll(t, groups...)
	assert.Equal(t, 2, f.engine.pollScans.rows, "only the first root's snapshot fits")
	f.poll(t, groups...)

	assert.EqualValues(t, 3, f.reconciled.Load(),
		"the second root does not fit beside the first and keeps reconciling")
}

func TestPollProviderRootsGroupedSyncsChangesAcrossPages(t *testing.T) {
	requirePollChangeTimes(t)
	root := t.TempDir()
	writeClaudePollSession(t, filepath.Join(root, "project", "seed.jsonl"), "seed")
	f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {root}})
	group := ProviderRootsGroup{Agent: parser.AgentClaude, Roots: []string{root}}
	f.poll(t, group)

	const added = pollScanPageSize + 5
	for i := range added {
		writeClaudePollSession(t, filepath.Join(root, "project", fmt.Sprintf("added-%03d.jsonl", i)), "added")
	}
	f.poll(t, group)

	assert.EqualValues(t, 1, f.reconciled.Load())
	for _, i := range []int{0, pollScanPageSize - 1, pollScanPageSize, added - 1} {
		f.session(t, fmt.Sprintf("added-%03d", i))
	}
}

func TestPollProviderRootsGroupedReconcilesRootsItCannotCompare(t *testing.T) {
	requirePollChangeTimes(t)
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	claudeRoot := t.TempDir()
	linked := filepath.Join(t.TempDir(), "linked-project")
	writeClaudePollSession(t, filepath.Join(linked, "linked.jsonl"), "linked")
	require.NoError(t, os.Symlink(linked, filepath.Join(claudeRoot, "project")))
	geminiRoot := t.TempDir()
	f := newPollScanFixture(t, map[parser.AgentType][]string{
		parser.AgentClaude: {claudeRoot},
		parser.AgentGemini: {geminiRoot},
	})
	groups := []ProviderRootsGroup{
		{Agent: parser.AgentClaude, Roots: []string{claudeRoot}},
		{Agent: parser.AgentGemini, Roots: []string{geminiRoot}},
	}

	f.poll(t, groups...)
	f.poll(t, groups...)

	assert.EqualValues(t, 4, f.reconciled.Load(),
		"a linked project directory and a provider without metadata polling reconcile every pass")
}

func TestPollProviderRootsGroupedReleasesSnapshots(t *testing.T) {
	requirePollChangeTimes(t)
	root := t.TempDir()
	writeClaudePollSession(t, filepath.Join(root, "project", "released.jsonl"), "first")
	f := newPollScanFixture(t, map[parser.AgentType][]string{parser.AgentClaude: {root}})
	group := ProviderRootsGroup{Agent: parser.AgentClaude, Roots: []string{root}}
	f.poll(t, group)
	f.poll(t, group)
	require.EqualValues(t, 1, f.reconciled.Load())

	f.poll(t)
	f.poll(t, group)
	assert.EqualValues(t, 2, f.reconciled.Load(), "a released root starts over with reconciliation")

	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), []ProviderRootsGroup{group}, true))
	assert.EqualValues(t, 3, f.reconciled.Load(), "an authoritative pass always reconciles")
}

func TestScanPollRootsLimits(t *testing.T) {
	requirePollChangeTimes(t)
	root := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("{}\n"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ignored"), 0o644))
	plan := []parser.WatchRoot{{Path: root, Recursive: true, IncludeGlobs: []string{"*.jsonl"}}}
	seed := maphash.MakeSeed()

	full, err := scanPollRoots(t.Context(), seed, plan, nil, 3)
	require.NoError(t, err)
	assert.Len(t, full.rows, 3, "only matching files are rows")
	assert.Empty(t, full.changed, "a first scan reports no changes")

	_, err = scanPollRoots(t.Context(), seed, plan, nil, 2)
	require.ErrorIs(t, err, errPollScanIneligible, "too many files to retain")

	edited := slices.Clone(full.rows)
	for i := range edited {
		edited[i].signature++
	}
	changed, err := scanPollRoots(t.Context(), seed, plan, edited, 3)
	require.NoError(t, err)
	assert.Len(t, changed.changed, 3, "every file whose signature differs is changed")

	missing, err := scanPollRoots(t.Context(), seed,
		[]parser.WatchRoot{{Path: filepath.Join(root, "absent"), IncludeGlobs: []string{"*.jsonl"}}}, full.rows, 3)
	require.NoError(t, err)
	assert.True(t, missing.vanished, "a missing root vanishes every previous row")
}

func TestScanPollRootsReconcilesLargeChangeSets(t *testing.T) {
	requirePollChangeTimes(t)
	root := t.TempDir()
	for i := range pollScanMaxChangedPaths + 1 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d.jsonl", i)), []byte("{}\n"), 0o644))
	}
	plan := []parser.WatchRoot{{Path: root, IncludeGlobs: []string{"*.jsonl"}}}
	seed := maphash.MakeSeed()
	first, err := scanPollRoots(t.Context(), seed, plan, nil, pollScanMaxRows)
	require.NoError(t, err)
	edited := slices.Clone(first.rows)
	for i := range edited {
		edited[i].signature++
	}

	_, err = scanPollRoots(t.Context(), seed, plan, edited, pollScanMaxRows)

	require.ErrorIs(t, err, errPollScanIneligible)
}
