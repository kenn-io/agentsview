package sync

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// A result moved to a derived id keeps its retry flag, so an incomplete
// parse is not stamped current under the new id.
func TestSourceCollisionKeepsRetryFlag(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "tmp", "hash", "chats")
	require.NoError(t, os.MkdirAll(chats, 0o755))
	owner := filepath.Join(chats, "session-2026-01-01T09-00-owner.json")
	other := filepath.Join(chats, "session-2026-01-01T10-00-other.json")
	for _, path := range []string{owner, other} {
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	}
	database := openTestDB(t)
	const id = "gemini:shared"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: id, Project: "p", Machine: "local", Agent: string(parser.AgentGemini), FilePath: &owner,
	}))
	provider, ok := parser.NewProvider(parser.AgentGemini, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	e := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "local",
	})
	t.Cleanup(e.Close)

	res := processResult{
		results: []parser.ParseResult{{Session: parser.ParsedSession{
			ID: id, Agent: parser.AgentGemini, File: parser.FileInfo{Path: other},
			ParentSessionID: "gemini:spawner", RelationshipType: parser.RelSubagent,
		}}},
		retrySessionIDs: map[string]bool{id: true},
	}
	e.applyProviderFilePathPolicies(t.Context(), provider, parser.AgentGemini, other, &res, e.idPrefix)

	require.Len(t, res.results, 1)
	altID := parser.AltSessionID(id, other)
	assert.Equal(t, altID, res.results[0].Session.ID)
	assert.True(t, res.needsRetryForSession(altID))
	// The derived session links to the session it shares an id with, even
	// when its parser recorded another parent.
	assert.Equal(t, id, res.results[0].Session.ParentSessionID)
	assert.Equal(t, parser.RelContinuation, res.results[0].Session.RelationshipType)
}

// A failed ownership lookup skips the source this pass so it retries, rather
// than treating the id as unowned.
func TestSourceCollisionLookupErrorSkipsSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "hash", "chats", "session-2026-01-01T10-00-a.json")
	provider, ok := parser.NewProvider(parser.AgentGemini, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	e := NewEngine(t.Context(), openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "local",
	})
	t.Cleanup(e.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := processResult{results: []parser.ParseResult{{Session: parser.ParsedSession{
		ID: "gemini:shared", Agent: parser.AgentGemini, File: parser.FileInfo{Path: path},
	}}}}
	e.applyProviderFilePathPolicies(ctx, provider, parser.AgentGemini, path, &res, e.idPrefix)

	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "session path records")
	assert.True(t, res.noCacheSkip)
	assert.Empty(t, res.results)
}

// Every sync pass, watcher-driven ones included, starts without the previous
// pass's claims, so a claim whose write never landed can't push a file onto a
// derived id.
func TestChangedPathSyncResetsSourceClaims(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "tmp", "hash", "chats")
	require.NoError(t, os.MkdirAll(chats, 0o755))
	path := filepath.Join(chats, "session-2026-01-01T10-00-a.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"sessionId":"shared","projectHash":"hash","startTime":"2024-01-01T10:00:00Z","lastUpdated":"2024-01-01T10:00:05Z","messages":[{"id":"m1","timestamp":"2024-01-01T10:00:00Z","type":"user","content":"hi"}]}`), 0o644))
	database := openTestDB(t)
	e := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "local",
	})
	t.Cleanup(e.Close)
	stale := filepath.Join(chats, "session-2026-01-01T09-00-stale.json")
	require.NoError(t, os.WriteFile(stale, []byte("{}"), 0o644))
	e.sourceClaims = map[string]string{"gemini:shared": stale}

	require.NoError(t, e.SyncPathsContext(t.Context(), []string{path}))

	stored, err := database.GetSessionFull(t.Context(), "gemini:shared")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, path, *stored.FilePath)
}

func TestS3CursorSharedSessionProjects(t *testing.T) {
	for _, tt := range []struct {
		name        string
		order       []int
		legacy      bool
		cachedLost  bool
		batch       bool
		lookupError bool
		trashed     bool
	}{
		{name: "both in one pass", order: []int{0, 1}, batch: true},
		{name: "A then B", order: []int{0, 1}},
		{name: "B then A", order: []int{1, 0}},
		{name: "ownership lookup fails", order: []int{0, 1}, lookupError: true},
		{name: "trashed object", order: []int{0, 1}, trashed: true},
		{name: "main dropped object", order: []int{0}, legacy: true},
		{name: "main overwritten row", order: []int{1}, legacy: true},
		{name: "main overwritten cached row", order: []int{1}, legacy: true, cachedLost: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const root = "s3://bucket/host-a/raw/cursor"
			const baseID = "host-a~cursor:shared"
			paths := []string{root + "/project-a/agent-transcripts/shared.txt", root + "/project-b/agent-transcripts/shared.txt"}
			bodies := map[string]string{
				paths[0]: "user:\nProject A\nassistant:\nAnswer A\n",
				paths[1]: "user:\nProject B\nassistant:\nAnswer B\n",
			}
			mtime := time.Unix(100, 0)
			var fetches atomic.Int32
			var reopen func()
			oldFetch := fetchS3Object
			t.Cleanup(func() { fetchS3Object = oldFetch })
			fetchS3Object = func(uri string) (io.ReadCloser, error) {
				if reopen != nil {
					reopen()
				}
				body, ok := bodies[uri]
				require.True(t, ok, "unexpected object %s", uri)
				fetches.Add(1)
				return io.NopCloser(strings.NewReader(body)), nil
			}
			database := openTestDB(t)
			def, ok := parser.AgentByType(parser.AgentCursor)
			require.True(t, ok)
			provider := &processFixtureProvider{Def: def, Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{DiscoverSources: parser.CapabilitySupported, SharedSessionIDs: parser.CapabilitySupported},
			}}
			source := func(i int) parser.SourceRef {
				uri := paths[i]
				return parser.SourceRef{
					Provider: parser.AgentCursor, Key: uri, DisplayPath: uri, FingerprintKey: uri,
					ProjectHint: []string{"project-a", "project-b"}[i],
					Opaque:      parser.S3DiscoveredSource{URI: uri, Machine: "host-a", Size: int64(len(bodies[uri])), MtimeNS: mtime.UnixNano(), Fingerprint: "s3-meta:stable"},
				}
			}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}}, Machine: "local",
				DisableFilesystemProjectDiscovery: true, ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
			})
			t.Cleanup(engine.Close)
			engine.workerCountOverride = 2
			if tt.batch {
				provider.discovered = []parser.SourceRef{source(tt.order[0]), source(tt.order[1])}
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				for i, uri := range paths {
					storedIDs, err := database.ListSessionIDsByFilePath(t.Context(), uri, "cursor")
					require.NoError(t, err)
					require.Len(t, storedIDs, 1)
					messages, err := database.GetAllMessages(t.Context(), storedIDs[0])
					require.NoError(t, err)
					require.Len(t, messages, 2)
					assert.Equal(t, []string{"Project A", "Project B"}[i], messages[0].Content)
					assert.Equal(t, []string{"Answer A", "Answer B"}[i], messages[1].Content)
				}
			} else {
				for _, i := range tt.order {
					provider.discovered = []parser.SourceRef{source(i)}
					stats := engine.SyncAll(t.Context(), nil)
					require.Zero(t, stats.Failed)
				}
			}
			if tt.legacy {
				starred, err := database.StarSession(t.Context(), baseID)
				require.NoError(t, err)
				require.True(t, starred)
				require.NoError(t, database.SetSessionDataVersion(t.Context(), baseID, 128))
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 128")
					return err
				}))
				if tt.cachedLost {
					// Main cached the first object before the second overwrote its row.
					engine.cacheSkip(paths[0], mtime.UnixNano(), "s3-meta:stable")
				}
			}
			provider.discovered = []parser.SourceRef{source(0), source(1)}
			if tt.lookupError {
				oldLookup := lookupS3Provider
				t.Cleanup(func() { lookupS3Provider = oldLookup })
				lookupS3Provider = func(agent parser.AgentType) (parser.S3Provider, bool) {
					// A maintenance handoff temporarily closes the archive's connections.
					lookupS3Provider = oldLookup
					require.NoError(t, database.CloseConnections(t.Context()))
					_, err := database.ListSessionIDsByFilePath(t.Context(), paths[0], "cursor")
					require.Error(t, err)
					return oldLookup(agent)
				}
				var once sync.Once
				reopen = func() { once.Do(func() { require.NoError(t, database.Reopen()) }) }
				t.Cleanup(reopen)
			}
			stats := engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
			require.Zero(t, stats.Failed)
			if tt.lookupError {
				assert.EqualValues(t, 4, fetches.Load(), "lookup failure must keep both objects in the work list")
			}
			if tt.cachedLost {
				missing, err := database.ListSessionIDsByFilePath(t.Context(), paths[0], "cursor")
				require.NoError(t, err)
				require.Empty(t, missing, "main's cached overwritten source remains missing after ordinary sync")
				needsResync, err := db.ArchiveNeedsResync(t.Context(), database.Path())
				require.NoError(t, err)
				require.True(t, needsResync, "the data-version bump must schedule recovery")
			}
			if tt.legacy {
				stats = engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
				require.Zero(t, stats.Failed)
			}
			ids := make([]string, 2)
			verify := func() {
				t.Helper()
				for i, uri := range paths {
					storedIDs, err := database.ListSessionIDsByFilePath(t.Context(), uri, "cursor")
					require.NoError(t, err)
					require.Len(t, storedIDs, 1, "both projects must survive")
					if ids[i] != "" {
						assert.Equal(t, ids[i], storedIDs[0])
					}
					ids[i] = storedIDs[0]
					session, err := database.GetSessionFull(t.Context(), ids[i])
					require.NoError(t, err)
					require.NotNil(t, session)
					assert.Equal(t, "host-a", session.Machine)
					assert.Equal(t, uri, derefString(session.FilePath))
					if ids[i] != baseID {
						assert.Equal(t, baseID, derefString(session.ParentSessionID))
						assert.Equal(t, "continuation", session.RelationshipType)
					}
					messages, err := database.GetAllMessages(t.Context(), ids[i])
					require.NoError(t, err)
					require.Len(t, messages, 2)
					assert.Equal(t, []string{"Project A", "Project B"}[i], messages[0].Content)
					assert.Equal(t, []string{"Answer A", "Answer B"}[i], messages[1].Content)
				}
			}
			verify()
			if tt.legacy {
				stars, err := database.ListStarredSessionIDs(t.Context())
				require.NoError(t, err)
				assert.Equal(t, []string{baseID}, stars)
			}
			if tt.name == "A then B" || tt.lookupError || tt.trashed {
				before := fetches.Load()
				if tt.trashed {
					require.NoError(t, database.SoftDeleteSession(t.Context(), ids[1]))
				}
				stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
				require.Zero(t, stats.Failed)
				assert.Equal(t, before, fetches.Load(), "unchanged objects must stay behind the cutoff")
				if tt.trashed {
					stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
					require.Zero(t, stats.Failed)
					assert.Equal(t, before, fetches.Load(), "trashed objects must stay behind the cutoff on the second pass")
					assert.True(t, database.IsSessionTrashed(t.Context(), ids[1]))
					return
				}
				stats = engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				assert.Equal(t, before, fetches.Load(), "unchanged objects must not download without a cutoff")
			}
			if tt.name != "A then B" && !tt.legacy {
				return
			}
			if !tt.legacy || ids[1] != baseID {
				starred, err := database.StarSession(t.Context(), ids[1])
				require.NoError(t, err)
				require.True(t, starred)
			}
			paths[1] = root + "/project-b/agent-transcripts/shared/shared.jsonl"
			bodies[paths[1]] = `{"role":"user","message":{"content":"Project B"}}` + "\n" + `{"role":"assistant","message":{"content":"Answer B"}}` + "\n"
			provider.discovered = []parser.SourceRef{source(0), source(1)}
			stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
			require.Zero(t, stats.Failed)
			verify()
			stars, err := database.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			wantStars := []string{ids[1]}
			if tt.legacy && baseID != ids[1] {
				wantStars = append(wantStars, baseID)
			}
			assert.ElementsMatch(t, wantStars, stars)
		})
	}
}
