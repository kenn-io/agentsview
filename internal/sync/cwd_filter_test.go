package sync

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type cwdFilterCase struct {
	name     string
	prefixes []string
	cwd      string
	want     bool
}

func TestCwdPrefixFilterAllows(t *testing.T) {
	tests := []cwdFilterCase{
		{"empty filter allows anything", nil, "/anywhere", true},
		{"empty filter allows empty cwd", nil, "", true},
		{"exact match", []string{"/a/b"}, "/a/b", true},
		{"child path", []string{"/a/b"}, "/a/b/c/d", true},
		{"sibling with shared prefix", []string{"/a/b"}, "/a/bc", false},
		{"outside prefix", []string{"/a/b"}, "/x", false},
		{"empty cwd rejected when filter set", []string{"/a/b"}, "", false},
		{"second prefix matches", []string{"/a/b", "/x/y"}, "/x/y/z", true},
		{"trailing separator normalized", []string{"/a/b/"}, "/a/b/c", true},
		{"prefix longer than cwd", []string{"/a/b/c"}, "/a/b", false},
		{"case sensitive", []string{"/a/B"}, "/a/b/c", false},
		{"blank entries ignored", []string{"  ", ""}, "/anywhere", true},
		{"root prefix allows any cwd", []string{"/"}, "/anywhere", true},
		{"dot-dot escaping the prefix rejected", []string{"/a/b"}, "/a/b/../c", false},
		{"dot-dot staying inside allowed", []string{"/a/b"}, "/a/b/c/../d", true},
		{"dot-dot in prefix cleaned", []string{"/a/b/../c"}, "/a/c/d", true},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			cwdFilterCase{"backslash boundary", []string{`C:\work`}, `C:\work\repo`, true},
			cwdFilterCase{"drive sibling", []string{`C:\work`}, `C:\workspace`, false},
			cwdFilterCase{"mixed separators normalized", []string{`C:/work`}, `C:\work\repo`, true},
		)
	} else {
		// On POSIX a backslash is an ordinary filename character:
		// "b\evil" is a sibling of "b" under /a, not a child of /a/b.
		tests = append(tests, cwdFilterCase{
			"backslash is not a separator", []string{"/a/b"}, `/a/b\evil`, false,
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCwdPrefixFilter(tt.prefixes)
			assert.Equal(t, tt.want, f.allows(tt.cwd))
		})
	}
}

// exclusionGateJob builds a syncJob whose parse supersedes the
// archived "stale" row (via excludedSessionIDs) with a replacement
// session recorded at the given cwd.
func exclusionGateJob(cwd string) syncJob {
	return syncJob{
		path: "/src/session.jsonl",
		processResult: processResult{
			excludedSessionIDs: []string{"stale"},
			results: []parser.ParseResult{
				{Session: parser.ParsedSession{
					ID:      "replacement",
					Agent:   parser.AgentClaude,
					Machine: "local",
					Project: "proj",
					Cwd:     cwd,
				}},
			},
		},
	}
}

// A parse whose sessions are all outside the cwd allow-list must not
// delete the archived rows its exclusion list supersedes: the
// replacement write is vetoed, so the delete would erase a session
// the filter promises to preserve.
func TestCollectAndBatchGatesParserExclusionsByCwdFilter(t *testing.T) {
	ctx := context.Background()

	t.Run("filtered source keeps archived row", func(t *testing.T) {
		database := openTestDB(t)
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "stale", Project: "proj", Machine: "local", Agent: "claude",
		}))
		e := NewEngine(database, EngineConfig{
			Machine:            "local",
			IncludeCwdPrefixes: []string{"/allowed"},
		})

		results := make(chan syncJob, 1)
		results <- exclusionGateJob("/outside/repo")
		close(results)
		stats := e.collectAndBatch(
			ctx, results, 1, 1, nil, syncWriteDefault,
		)

		gotStale, err := database.GetSession(ctx, "stale")
		require.NoError(t, err)
		assert.NotNil(t, gotStale,
			"archived row must survive exclusions from a filtered source")
		gotNew, err := database.GetSession(ctx, "replacement")
		require.NoError(t, err)
		assert.Nil(t, gotNew, "filtered replacement must not be written")
		assert.Empty(t, stats.parserExcludedIDs,
			"frozen exclusions must not reach resync orphan-copy exclusion")
		assert.Equal(t, 1, stats.cwdFilteredSessions, "filtered sessions")
		assert.Equal(t, 1, stats.cwdFilteredFiles, "filtered files")
		assert.Equal(t, 0, stats.Synced, "synced")
	})

	t.Run("allowed source deletes superseded row", func(t *testing.T) {
		database := openTestDB(t)
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "stale", Project: "proj", Machine: "local", Agent: "claude",
		}))
		e := NewEngine(database, EngineConfig{
			Machine:            "local",
			IncludeCwdPrefixes: []string{"/allowed"},
		})

		results := make(chan syncJob, 1)
		results <- exclusionGateJob("/allowed/repo")
		close(results)
		stats := e.collectAndBatch(
			ctx, results, 1, 1, nil, syncWriteDefault,
		)

		gotStale, err := database.GetSession(ctx, "stale")
		require.NoError(t, err)
		assert.Nil(t, gotStale,
			"superseded row must be deleted for an allowed source")
		gotNew, err := database.GetSession(ctx, "replacement")
		require.NoError(t, err)
		assert.NotNil(t, gotNew, "allowed replacement must be written")
		assert.Equal(t, []string{"stale"}, stats.parserExcludedIDs)
		assert.Equal(t, 0, stats.cwdFilteredSessions, "filtered sessions")
		assert.Equal(t, 1, stats.Synced, "synced")
	})
}

func TestShouldAbortResyncSwap(t *testing.T) {
	tests := []struct {
		name            string
		stats           SyncStats
		oldFileSessions int
		trashedCopied   int
		want            bool
	}{
		{
			name: "clean run proceeds",
			stats: SyncStats{
				TotalSessions: 5, Synced: 5,
				filesOK: 5, nonContainerDiscovered: 5,
			},
			oldFileSessions: 5,
		},
		{
			name:            "cancelled run aborts",
			stats:           SyncStats{Aborted: true, Synced: 5},
			oldFileSessions: 5,
			want:            true,
		},
		{
			name:            "empty discovery with old data aborts",
			stats:           SyncStats{},
			oldFileSessions: 3,
			want:            true,
		},
		{
			name: "zero writes unexplained aborts",
			stats: SyncStats{
				TotalSessions: 3, nonContainerDiscovered: 3,
			},
			oldFileSessions: 3,
			want:            true,
		},
		{
			name: "more failures than successes aborts",
			stats: SyncStats{
				TotalSessions: 6, Synced: 1, Failed: 5,
				filesOK: 1, nonContainerDiscovered: 6,
			},
			oldFileSessions: 6,
			want:            true,
		},
		{
			name: "parser-excluded-only run proceeds",
			stats: SyncStats{
				TotalSessions: 3, filesOK: 3,
				parserExcludedFiles: 3, nonContainerDiscovered: 3,
			},
			oldFileSessions: 3,
		},
		{
			name: "all-cwd-filtered run proceeds",
			stats: SyncStats{
				TotalSessions: 2, filesOK: 2,
				cwdFilteredFiles: 2, cwdFilteredSessions: 2,
				nonContainerDiscovered: 2,
			},
			oldFileSessions: 2,
		},
		{
			name: "cwd-filtered mixed with parser-excluded proceeds",
			stats: SyncStats{
				TotalSessions: 4, filesOK: 4,
				cwdFilteredFiles: 2, cwdFilteredSessions: 3,
				parserExcludedFiles: 2, nonContainerDiscovered: 4,
			},
			oldFileSessions: 4,
		},
		{
			name: "cwd-filtered with unaccounted OK file aborts",
			stats: SyncStats{
				TotalSessions: 3, filesOK: 2,
				cwdFilteredFiles: 1, cwdFilteredSessions: 1,
				nonContainerDiscovered: 3,
			},
			oldFileSessions: 3,
			want:            true,
		},
		{
			name: "cwd-filtered with failures aborts",
			stats: SyncStats{
				TotalSessions: 2, Failed: 1, filesOK: 1,
				cwdFilteredFiles: 1, cwdFilteredSessions: 1,
				nonContainerDiscovered: 2,
			},
			oldFileSessions: 2,
			want:            true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldAbortResyncSwap(
				tt.stats, tt.oldFileSessions, tt.trashedCopied,
			)
			assert.Equal(t, tt.want, got)
		})
	}
}
