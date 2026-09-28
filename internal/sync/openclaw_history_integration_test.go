package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestOpenClawShortenedLegacySourcePreservesArchive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
	}{
		{name: "full sync"},
		{name: "changed path"},
		{name: "single session"},
		{name: "remote changed path", remote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, other := t.TempDir(), t.TempDir()
			path := filepath.Join(root, "main", "sessions", "shortened.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			const shortened = `{"type":"session","version":3,"id":"shortened","timestamp":"2026-09-22T10:00:00Z","cwd":"/workspace/project-a"}
{"type":"message","id":"m1","timestamp":"2026-09-22T10:00:01Z","message":{"role":"user","content":"saved question","timestamp":"2026-09-22T10:00:01Z"}}
`
			const complete = shortened + `{"type":"message","id":"m2","timestamp":"2026-09-22T10:00:02Z","message":{"role":"assistant","content":"saved response","timestamp":"2026-09-22T10:00:02Z"}}
`
			require.NoError(t, os.WriteFile(path, []byte(complete), 0o600))
			base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
			require.NoError(t, os.Chtimes(path, base, base))
			database := dbtest.OpenTestDB(t)
			cfg := sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentOpenClaw: {root, other}},
				Machine:   "local",
			}
			sessionID := "openclaw:main:shortened"
			if tc.remote {
				cfg.IDPrefix = "host~"
				cfg.PathRewriter = func(path string) string { return "host:" + path }
				cfg.StoredPathResolver = func(path string) (string, bool) { return strings.CutPrefix(path, "host:") }
				sessionID = "host~" + sessionID
			}
			engine := sync.NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

			require.NoError(t, os.WriteFile(path, []byte(shortened), 0o600))
			shortenedAt := base.Add(time.Hour)
			require.NoError(t, os.Chtimes(path, shortenedAt, shortenedAt))
			switch tc.name {
			case "full sync":
				stats := engine.SyncAll(t.Context(), nil)
				require.False(t, stats.Aborted)
				require.Zero(t, stats.Failed)
			case "single session":
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), sessionID))
			default:
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
			}
			assertMessageContent(t, database, sessionID, "saved question", "saved response")
			session, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, 2, session.MessageCount)
			assert.Equal(t, 1, session.UserMessageCount)

			// A preferred different source is still an authoritative replacement.
			replacement := filepath.Join(other, "main", "sessions", "shortened.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(replacement), 0o755))
			require.NoError(t, os.WriteFile(replacement,
				[]byte(strings.Replace(shortened, "saved question", "replacement question", 1)), 0o600))
			replacedAt := base.Add(2 * time.Hour)
			require.NoError(t, os.Chtimes(replacement, replacedAt, replacedAt))
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{replacement}))
			assertMessageContent(t, database, sessionID, "replacement question")
			session, err = database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, 1, session.MessageCount)
			assert.Equal(t, 1, session.UserMessageCount)
			if tc.remote {
				replacement = cfg.PathRewriter(replacement)
			}
			assert.Equal(t, replacement, database.GetSessionFilePath(t.Context(), sessionID))
		})
	}
}

func TestReconcileWatchRootsOpenClawRanksAcrossRoots(t *testing.T) {
	for _, tc := range []struct {
		name         string
		firstSuffix  string
		preferredAge time.Duration
	}{
		{name: "live beats archive", firstSuffix: ".deleted.2026-09-22T10-00-00.000Z", preferredAge: -time.Hour},
		{name: "newer live copy", preferredAge: time.Hour},
		{name: "equal timestamps use path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			first, preferred := filepath.Join(root, "z-first"), filepath.Join(root, "a-preferred")
			firstPath := filepath.Join(first, "main", "sessions", "duplicate.jsonl"+tc.firstSuffix)
			preferredPath := filepath.Join(preferred, "main", "sessions", "duplicate.jsonl")
			for _, path := range []string{firstPath, preferredPath} {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"duplicate","timestamp":"2026-09-22T10:00:00Z","cwd":"/workspace/project-a"}
{"type":"message","id":"m1","timestamp":"2026-09-22T10:00:01Z","message":{"role":"user","content":"saved question","timestamp":"2026-09-22T10:00:01Z"}}
`), 0o600))
			}
			base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
			require.NoError(t, os.Chtimes(firstPath, base, base))
			preferredTime := base.Add(tc.preferredAge)
			require.NoError(t, os.Chtimes(preferredPath, preferredTime, preferredTime))
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentOpenClaw: {first, preferred}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{first, preferred}, false))
			assert.Equal(t, preferredPath, database.GetSessionFilePath(t.Context(), "openclaw:main:duplicate"))
		})
	}
}
