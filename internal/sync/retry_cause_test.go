package sync

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Archives can hold thousands of forks whose parents were deleted. A pass logs
// the first few causes one by one, then one summary that counts the rest.
func TestSyncLogsBoundedRetryCauses(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const forks = 25
	root := t.TempDir()
	for i := range forks {
		writeOrphanCodexFork(t, root, fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012d", i), parent)
	}
	engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{
		AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:                           "local",
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	var logs summaryLogBuffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	stats := engine.SyncAll(t.Context(), nil)

	require.Equal(t, forks, stats.Deferred)
	var perSession, summaries []string
	for line := range strings.Lines(logs.take()) {
		switch {
		case strings.Contains(line, "sync: session needs retry: "):
			perSession = append(perSession, line)
		case strings.Contains(line, "sync: 25 sessions need retry: "):
			summaries = append(summaries, line)
		}
	}
	assert.Len(t, perSession, 20)
	for _, line := range perSession {
		assert.Contains(t, line, "could not read the Codex session it was forked from ("+parent+")")
	}
	require.Len(t, summaries, 1)
	assert.Equal(t, 5, strings.Count(summaries[0], "could not read the Codex session it was forked from ("+parent+")"))
	assert.True(t, strings.HasSuffix(summaries[0], "; 20 more\n"), summaries[0])
	cause := stats.IncompleteCause()
	assert.True(t, strings.HasPrefix(cause, "25 sessions wait for a retry; why: codex:bbbbbbbb-"), cause)
	assert.True(t, strings.HasSuffix(cause, "; 20 more"), cause)
}

// An attached archive source whose fork lacks its parent still stops the
// rebuild. The abort must name the contributor and the fork.
func TestRebuildAbortNamesContributorForkWithoutParent(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	childPath := writeOrphanCodexFork(t, remoteRoot, child, parent)
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {localRoot}},
		Machine:   "local", DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	options := RebuildOptions{Contributors: []RebuildContributor{{
		Name: "remote",
		Config: EngineConfig{
			AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {remoteRoot}},
			Machine:   "remote", IDPrefix: "remote~", Ephemeral: true,
			DisableFilesystemProjectDiscovery: true,
		},
	}}}

	stats, err := engine.ResyncAllWithOptions(t.Context(), nil, options)

	require.NoError(t, err)
	require.True(t, stats.Aborted)
	want := `sync source "remote" did not finish: 1 session waits for a retry; why: remote~codex:` +
		child + " (" + childPath + "): could not read the Codex session it was forked from (" + parent + ")"
	assert.Equal(t, want, stats.AbortCause())
	assert.Contains(t, stats.Warnings, "resync aborted: "+want)
}
