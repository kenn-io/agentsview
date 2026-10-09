package sync

import (
	"database/sql"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestS3CursorSharedSessionProjects(t *testing.T) {
	for _, tt := range []struct {
		name              string
		reverse, separate bool
		root              string
		preCollapsed      bool
	}{
		{name: "together"},
		{name: "together reversed", reverse: true},
		{name: "separate", separate: true},
		{name: "no machine boundary", root: "s3://bucket/archive"},
		{name: "pre-collapsed cached source", preCollapsed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, machine, storedMachine := tt.root, "", "local"
			if root == "" {
				root, machine, storedMachine = "s3://bucket/host-a/raw/cursor", "host-a", "host-a"
			}
			const stem = "11111111-1111-4111-8111-111111111111"
			baseID := s3SessionIDPrefix(machine) + "cursor:" + stem
			paths := []string{root + "/agent-transcripts/" + stem + ".txt", root + "/cursor/" + stem + ".txt"}
			contents := map[string]string{
				paths[0]: "user:\nProject A\nassistant:\nAnswer A\n",
				paths[1]: "user:\nProject B\nassistant:\nAnswer B\nuser:\nFollow up B\n",
			}
			mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
			oldFetch := fetchS3Object
			t.Cleanup(func() { fetchS3Object = oldFetch })
			var fetches atomic.Int32
			fetchS3Object = func(uri string) (io.ReadCloser, error) {
				content, ok := contents[uri]
				if !ok {
					return nil, missingS3ObjectError()
				}
				fetches.Add(1)
				return io.NopCloser(strings.NewReader(content)), nil
			}
			database := openTestDB(t)
			storedSession := func(id string) *db.Session {
				session, err := database.GetSessionFull(t.Context(), id)
				require.NoError(t, err)
				require.NotNil(t, session)
				return session
			}
			def, ok := parser.AgentByType(parser.AgentCursor)
			require.True(t, ok)
			provider := &processFixtureProvider{Def: def, Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{DiscoverSources: parser.CapabilitySupported, SharedSessionIDs: parser.CapabilitySupported},
			}}
			sources := make([]parser.SourceRef, 0, 2)
			for i, uri := range paths {
				project := []string{"agent-transcripts", "cursor"}[i]
				sources = append(sources, parser.SourceRef{
					Provider: parser.AgentCursor, Key: uri, DisplayPath: uri, FingerprintKey: uri, ProjectHint: project,
					Opaque: parser.S3DiscoveredSource{
						URI: uri, Project: project,
						Machine: machine, Size: int64(len(contents[uri])), MtimeNS: mtime.UnixNano(),
					},
				})
			}
			if tt.reverse {
				sources[0], sources[1] = sources[1], sources[0]
			}
			provider.discovered = sources
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}}, Machine: "local",
				DisableFilesystemProjectDiscovery: true, ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
			})
			t.Cleanup(engine.Close)
			if tt.separate {
				provider.discovered = sources[:1]
			}
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			if tt.separate {
				provider.discovered = sources
				stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
				require.Zero(t, stats.Failed)
				require.Equal(t, 1, stats.Synced, "an unstored project must import despite the cutoff")
			}
			ids := make(map[string]string)
			for _, uri := range paths {
				stored, err := database.ListSessionIDsByFilePath(t.Context(), uri, "cursor")
				require.NoError(t, err)
				require.Len(t, stored, 1, "each project must retain its conversation")
				ids[uri] = stored[0]
				if stored[0] != baseID {
					assert.Equal(t, parser.AltSessionID(baseID, uri), stored[0])
				}
			}
			verify := func() {
				for i, uri := range paths {
					session := storedSession(ids[uri])
					assert.Equal(t, uri, derefString(session.FilePath))
					assert.Equal(t, storedMachine, session.Machine)
					if ids[uri] != baseID {
						assert.Equal(t, baseID, derefString(session.ParentSessionID))
					}
					messages, err := database.GetAllMessages(t.Context(), ids[uri])
					require.NoError(t, err)
					require.Len(t, messages, i+2)
					assert.Equal(t, []string{"Project A", "Project B"}[i], messages[0].Content)
				}
			}
			verify()
			if tt.preCollapsed {
				lostPath := paths[0]
				if ids[lostPath] == baseID {
					lostPath = paths[1]
				}
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(t.Context(), "DELETE FROM sessions WHERE id = ?", ids[lostPath]); err != nil {
						return err
					}
					_, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 127")
					return err
				}))
				require.NoError(t, database.SetSessionDataVersion(t.Context(), baseID, 127))
				engine.cacheSkip(lostPath, mtime.UnixNano())
				stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
				require.Zero(t, stats.Failed)
				missing, err := database.GetSessionFull(t.Context(), ids[lostPath])
				require.NoError(t, err)
				require.Nil(t, missing, "a pre-upgrade cached skip bypasses the missing-row freshness gate")
				needsResync, err := db.ArchiveNeedsResync(t.Context(), database.Path())
				require.NoError(t, err)
				require.True(t, needsResync, "the data-version increase must schedule archive recovery")
				stats = engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
				require.Zero(t, stats.Failed)
				verify()
			}
			before := fetches.Load()
			stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
			require.Zero(t, stats.Failed)
			assert.Equal(t, before, fetches.Load(), "unchanged derived sources must not download")
			// A stale row bypasses the cutoff without giving its ID to the other project.
			require.NoError(t, database.SetSessionDataVersion(t.Context(), ids[paths[1]], db.CurrentDataVersion()-1))
			stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
			require.Zero(t, stats.Failed)
			assert.Equal(t, before+1, fetches.Load())
			verify()
			// Changed object metadata must refresh the same saved row.
			contents[paths[1]] = strings.ReplaceAll(contents[paths[1]], "Answer B", "Updated B")
			for i := range sources {
				if sources[i].DisplayPath == paths[1] {
					source := sources[i].Opaque.(parser.S3DiscoveredSource)
					source.Size = int64(len(contents[paths[1]]))
					sources[i].Opaque = source
				}
			}
			stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
			require.Zero(t, stats.Failed)
			messages, err := database.GetAllMessages(t.Context(), ids[paths[1]])
			require.NoError(t, err)
			require.Len(t, messages, 3)
			assert.Equal(t, "Updated B", messages[1].Content)
			// Rebuild discovers the alternate first and must keep both saved IDs.
			provider.discovered = sources
			if ids[sources[0].DisplayPath] == baseID {
				provider.discovered = []parser.SourceRef{sources[1], sources[0]}
			}
			stats = engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
			require.Zero(t, stats.Failed)
			verify()
			// Omission is not proof of remote deletion, even when another object grows.
			ownerPath := paths[0]
			otherPath := paths[1]
			if ids[ownerPath] != baseID {
				ownerPath, otherPath = otherPath, ownerPath
			}
			for _, source := range sources {
				if source.DisplayPath == otherPath {
					provider.discovered = []parser.SourceRef{source}
				}
			}
			stats = engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
			require.Zero(t, stats.Failed)
			verify()
			assert.Equal(t, ownerPath, derefString(storedSession(baseID).FilePath))
			// A second machine owns its own base ID despite the same project and stem.
			otherMachine := "s3://bucket/host-b/raw/cursor/agent-transcripts/" + stem + ".txt"
			contents[otherMachine] = contents[paths[0]]
			second := sources[0]
			second.Key, second.DisplayPath, second.FingerprintKey = otherMachine, otherMachine, otherMachine
			second.ProjectHint = "agent-transcripts"
			second.Opaque = parser.S3DiscoveredSource{
				URI: otherMachine, Project: "agent-transcripts", Machine: "host-b",
				Size: int64(len(contents[otherMachine])), MtimeNS: mtime.UnixNano(),
			}
			provider.discovered = append(provider.discovered, second)
			if machine == "" {
				unrelated := root + "/agent-transcripts/unrelated.txt"
				contents[unrelated] = "user:\nUnrelated conversation\n"
				source := second
				source.Key, source.DisplayPath, source.FingerprintKey = unrelated, unrelated, unrelated
				source.Opaque = parser.S3DiscoveredSource{URI: unrelated, Project: "agent-transcripts", Size: int64(len(contents[unrelated])), MtimeNS: mtime.UnixNano()}
				provider.discovered = append(provider.discovered, source)
			}
			stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
			require.Zero(t, stats.Failed)
			assert.Equal(t, "host-b", storedSession("host-b~cursor:"+stem).Machine)
			if machine == "" {
				assert.Equal(t, "local", storedSession("cursor:unrelated").Machine)
			}
			verify()
			if tt.name == "together" {
				newPath := strings.TrimSuffix(ownerPath, stem+".txt") + "agent-transcripts/" + stem + "/" + stem + ".jsonl"
				contents[newPath] = contents[ownerPath]
				source := sources[0]
				if source.DisplayPath != ownerPath {
					source = sources[1]
				}
				source.Key, source.DisplayPath, source.FingerprintKey = newPath, newPath, newPath
				remote := source.Opaque.(parser.S3DiscoveredSource)
				remote.URI = newPath
				source.Opaque = remote
				provider.discovered = []parser.SourceRef{source}
				stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
				require.Zero(t, stats.Failed)
				for pass := range 3 {
					if pass == 1 {
						before := fetches.Load()
						stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
						require.Zero(t, stats.Failed)
						assert.Equal(t, before, fetches.Load(), "the saved alias must skip unchanged content")
					}
					if pass == 2 {
						stats = engine.ResyncAll(t.Context(), nil)
						require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
						require.Zero(t, stats.Failed)
					}
					assert.Equal(t, newPath, derefString(storedSession(baseID).FilePath))
					ids, err := database.ListSessionIDsByFilePath(t.Context(), newPath, "cursor")
					require.NoError(t, err)
					assert.Equal(t, []string{baseID}, ids)
					ids, err = database.ListSessionIDsByFilePath(t.Context(), ownerPath, "cursor")
					require.NoError(t, err)
					assert.Empty(t, ids)

				}
			}
		})
	}
}

func TestS3CursorCollidingParents(t *testing.T) {
	for _, tt := range []struct {
		name   string
		passes [][]int
	}{
		{"parents first", [][]int{{0}, {1}, {2}}},
		{"parents reversed", [][]int{{1}, {0}, {2}}},
		{"child first separate", [][]int{{2}, {0}, {1}}},
		{"child first together", [][]int{{2, 1, 0}}},
		{"own parent late", [][]int{{0}, {2}, {1}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const root = "s3://bucket/host-a/raw/cursor"
			const aliasRoot = "s3://other-bucket/host-a/raw/cursor"
			const content = "user:\nHello\nassistant:\nReply\n"
			paths := []string{root + "/project-a/agent-transcripts/shared.txt", root + "/project-b/agent-transcripts/shared.txt", root + "/project-b/agent-transcripts/shared/subagents/child.txt"}
			oldFetch := fetchS3Object
			t.Cleanup(func() { fetchS3Object = oldFetch })
			fetchS3Object = func(string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(content)), nil
			}
			database := openTestDB(t)
			def, ok := parser.AgentByType(parser.AgentCursor)
			require.True(t, ok)
			provider := &processFixtureProvider{Def: def, Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{DiscoverSources: parser.CapabilitySupported, SharedSessionIDs: parser.CapabilitySupported},
			}}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root, aliasRoot}}, Machine: "local",
				DisableFilesystemProjectDiscovery: true, ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
			})
			t.Cleanup(engine.Close)
			source := func(uri string) parser.SourceRef {
				return parser.SourceRef{
					Provider: parser.AgentCursor, Key: uri, DisplayPath: uri, FingerprintKey: uri,
					Opaque: parser.S3DiscoveredSource{URI: uri, Machine: "host-a", Size: int64(len(content)), MtimeNS: time.Unix(100, 0).UnixNano()},
				}
			}
			verify := func() {
				t.Helper()
				child, err := database.GetSessionFull(t.Context(), "host-a~cursor:child")
				require.NoError(t, err)
				if child == nil {
					return
				}
				parents, err := database.ListSessionIDsByFilePath(t.Context(), paths[1], "cursor")
				require.NoError(t, err)
				if len(parents) == 0 {
					assert.Nil(t, child.ParentSessionID, "a different project's parent cannot own this child")
				} else {
					require.Len(t, parents, 1)
					assert.Equal(t, parents[0], derefString(child.ParentSessionID))
				}
				assert.Equal(t, "host-a~cursor:shared", derefString(child.ParserParentSessionID))
			}
			for _, pass := range tt.passes {
				provider.discovered = nil
				for _, i := range pass {
					if tt.name == "own parent late" && i == 1 {
						paths[1] = aliasRoot + "/project-b/agent-transcripts/shared.txt"
					}
					provider.discovered = append(provider.discovered, source(paths[i]))
				}
				stats := engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				verify()
			}
			if tt.name == "own parent late" {
				parents, err := database.ListSessionIDsByFilePath(t.Context(), paths[1], "cursor")
				require.NoError(t, err)
				require.Len(t, parents, 1)
				paths[1] = aliasRoot + "/project-b/agent-transcripts/shared/shared.jsonl"
				provider.discovered = []parser.SourceRef{source(paths[1])}
				stats := engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				moved, err := database.ListSessionIDsByFilePath(t.Context(), paths[1], "cursor")
				require.NoError(t, err)
				assert.Equal(t, parents, moved)
				verify()
			}
			provider.discovered = []parser.SourceRef{source(paths[2])}
			stats := engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
			require.Zero(t, stats.Failed)
			verify()
			_, _, err := engine.processAndWriteSessionFile(t.Context(), parser.DiscoveredFile{
				Agent: parser.AgentCursor, Path: paths[2], Machine: "host-a", SourceSize: int64(len(content)), SourceMtime: time.Unix(100, 0).UnixNano(), ForceParse: true,
			}, "host-a~cursor:child")
			require.NoError(t, err)
			verify()
		})
	}
}
