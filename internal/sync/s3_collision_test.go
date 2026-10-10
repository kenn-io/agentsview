package sync

import (
	"database/sql"
	"io"
	"slices"
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
		name         string
		separate     bool
		root         string
		preCollapsed bool
		restoreRoots bool
	}{
		{name: "together"},
		{name: "separate", separate: true},
		{name: "roots restored", separate: true, restoreRoots: true},
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
			otherRoot := "s3://other-bucket/host-a/raw/cursor"
			if tt.restoreRoots {
				paths[1] = otherRoot + "/agent-transcripts/" + stem + ".txt"
			}
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
						Machine: machine, Size: int64(len(contents[uri])), MtimeNS: mtime.UnixNano(), Fingerprint: "s3-meta:stable",
					},
				})
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
				if tt.restoreRoots {
					engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {otherRoot}}})
					provider.discovered = sources[1:]
				}
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
			if tt.restoreRoots {
				engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root, otherRoot}}})
				provider.discovered = sources
				starred, err := database.StarSession(t.Context(), ids[paths[1]])
				require.NoError(t, err)
				require.True(t, starred)
				messages, err := database.GetAllMessages(t.Context(), ids[paths[1]])
				require.NoError(t, err)
				_, err = database.PinMessage(t.Context(), ids[paths[1]], messages[0].ID, nil)
				require.NoError(t, err)
			}
			verify := func() {
				if tt.restoreRoots {
					stars, err := database.ListStarredSessionIDs(t.Context())
					require.NoError(t, err)
					assert.Equal(t, []string{ids[paths[1]]}, stars)
					pins, err := database.ListPinnedMessages(t.Context(), ids[paths[1]], "")
					require.NoError(t, err)
					require.Len(t, pins, 1)
				}
				for i, uri := range paths {
					session := storedSession(ids[uri])
					assert.Equal(t, uri, derefString(session.FilePath))
					assert.Equal(t, storedMachine, session.Machine)
					assert.Nil(t, session.ParentSessionID)
					sidebar, err := database.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Project: session.Project, Limit: 50})
					require.NoError(t, err)
					assert.True(t, slices.ContainsFunc(sidebar.Sessions, func(row db.SidebarSessionIndexRow) bool { return row.ID == ids[uri] }), "each project must show its conversation")
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
					_, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 128")
					return err
				}))
				require.NoError(t, database.SetSessionDataVersion(t.Context(), baseID, 128))
				engine.cacheSkip(lostPath, mtime.UnixNano(), "s3-meta:stable")
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
			if tt.name == "together" {
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
				altID := ids[paths[0]]
				if altID == baseID {
					altID = ids[paths[1]]
				}
				require.NoError(t, database.SoftDeleteSession(t.Context(), altID))
				beforeTrash := fetches.Load()
				for range 2 {
					stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
					require.Zero(t, stats.Failed)
					assert.Equal(t, beforeTrash, fetches.Load(), "unchanged trashed S3 sources must stay behind the cutoff")
					assert.True(t, database.IsSessionTrashed(t.Context(), altID))
				}
				_, err := database.RestoreSession(t.Context(), altID)
				require.NoError(t, err)
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
				stats = engine.SyncAllSince(t.Context(), mtime.Add(time.Hour), nil)
				require.Zero(t, stats.Failed)
				assert.Equal(t, "host-b", storedSession("host-b~cursor:"+stem).Machine)
				verify()
			}
			if tt.restoreRoots {
				provider.discovered = []parser.SourceRef{sources[1], sources[0]}
				stats = engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
				require.Zero(t, stats.Failed)
				verify()
				oldPath := paths[1]
				paths[1] = strings.TrimSuffix(oldPath, ".txt") + ".jsonl"
				contents[paths[1]] = strings.ReplaceAll(contents[oldPath], "Answer B", "Format B")
				ids[paths[1]] = ids[oldPath]
				for _, uri := range []string{paths[1], oldPath, paths[1]} {
					changed := sources[1]
					changed.Key, changed.DisplayPath, changed.FingerprintKey = uri, uri, uri
					remote := changed.Opaque.(parser.S3DiscoveredSource)
					remote.URI, remote.Size = uri, int64(len(contents[uri]))
					changed.Opaque = remote
					provider.discovered = []parser.SourceRef{changed}
					stats = engine.SyncAll(t.Context(), nil)
					require.Zero(t, stats.Failed)
				}
				sources[1] = provider.discovered[0]
				verify()
				messages, err := database.GetAllMessages(t.Context(), ids[paths[1]])
				require.NoError(t, err)
				assert.Equal(t, "Format B", messages[1].Content)
				stats = engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
				require.Zero(t, stats.Failed)
				verify()
			}
			if tt.restoreRoots {
				require.NoError(t, database.DeleteSession(t.Context(), ids[paths[1]]))
				provider.discovered = sources[1:]
				_, _, err := engine.processAndWriteSessionFile(t.Context(), parser.DiscoveredFile{
					Agent: parser.AgentCursor, Path: paths[1], Machine: machine, SourceSize: int64(len(contents[paths[1]])), SourceMtime: mtime.UnixNano(), ForceParse: true,
				}, ids[paths[1]])
				require.NoError(t, err)
				assert.True(t, database.IsSessionExcluded(t.Context(), ids[paths[1]]))
				missing, err := database.GetSessionFull(t.Context(), ids[paths[1]])
				require.NoError(t, err)
				assert.Nil(t, missing)
				assert.Equal(t, paths[0], derefString(storedSession(baseID).FilePath))
				messages, err := database.GetAllMessages(t.Context(), baseID)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Equal(t, "Project A", messages[0].Content)
			}
		})
	}
}

type cursorParentStatProvider struct {
	parser.S3Provider
	stat func(string) (parser.S3Object, error)
}

func (p cursorParentStatProvider) S3StatSession(uri string) (parser.S3Object, error) {
	return p.stat(uri)
}

func TestS3CursorCollidingParents(t *testing.T) {
	for _, tt := range []struct {
		name            string
		passes          [][]int
		childrenCollide bool
	}{
		{"parents first", [][]int{{0}, {1}, {2}}, false},
		{"child first separate", [][]int{{2}, {0}, {1}}, false},
		{"own parent late", [][]int{{0}, {2}, {1}}, false},
		{"own parent missing", [][]int{{0}, {2}}, false},
		{"parent root removed", [][]int{{0}, {1}, {2}}, false},
		{"same-family roots restored", [][]int{{0}, {1}, {2}}, false},
		{"new child after parent root removed", [][]int{{0}, {1}, {2}}, false},
		{"legacy wrong parent root removed", [][]int{{0}, {1}, {2}}, false},
		{"children collide", [][]int{{0}, {1}, {3}, {2}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const root = "s3://bucket/host-a/raw/cursor"
			const aliasRoot = "s3://other-bucket/host-a/raw/cursor"
			const content = "user:\nHello A\nassistant:\nReply A\n"
			paths := []string{root + "/project-a/agent-transcripts/shared.txt", root + "/project-b/agent-transcripts/shared.txt", root + "/project-b/agent-transcripts/shared/subagents/child.txt"}
			if tt.childrenCollide {
				paths = []string{root + "/project-a/agent-transcripts/parent-a.txt", root + "/project-b/agent-transcripts/parent-b.txt", root + "/project-b/agent-transcripts/parent-b/subagents/child.txt", root + "/project-a/agent-transcripts/parent-a/subagents/child.txt"}
			}
			if tt.name == "parent root removed" || tt.name == "new child after parent root removed" {
				paths[2] = strings.Replace(paths[2], root, aliasRoot, 1)
			}
			if tt.name == "same-family roots restored" {
				paths = []string{root + "/project/agent-transcripts/shared.txt", aliasRoot + "/project/agent-transcripts/shared.txt", aliasRoot + "/project/agent-transcripts/shared/subagents/child.txt"}
			}
			if tt.name == "legacy wrong parent root removed" {
				paths[1] = strings.Replace(paths[1], root, aliasRoot, 1)
				paths[2] = strings.Replace(paths[2], root, aliasRoot, 1)
			}
			oldFetch := fetchS3Object
			refreshed := false
			t.Cleanup(func() { fetchS3Object = oldFetch })
			fetchS3Object = func(uri string) (io.ReadCloser, error) {
				body := content
				if strings.Contains(uri, "/project-b/") {
					body = strings.ReplaceAll(body, " A", " B")
				}
				if refreshed {
					body = strings.ReplaceAll(body, "Hello", "Refreshed")
				}
				return io.NopCloser(strings.NewReader(body)), nil
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
			parentEvidenceRestored := false
			childIDs := make(map[string]string)
			verify := func() {
				t.Helper()
				for i, uri := range paths[2:] {
					ids, err := database.ListSessionIDsByFilePath(t.Context(), uri, "cursor")
					require.NoError(t, err)
					if len(ids) == 0 {
						assert.Empty(t, childIDs[uri], "an archived child must remain present")
						continue
					}
					require.Len(t, ids, 1)
					if childIDs[uri] == "" {
						childIDs[uri] = ids[0]
					}
					assert.Equal(t, childIDs[uri], ids[0])
					baseID := "host-a~" + parser.CursorSessionID(uri)
					if ids[0] != baseID {
						assert.Equal(t, parser.AltSessionID(baseID, uri), ids[0])
					}
					child, err := database.GetSessionFull(t.Context(), ids[0])
					require.NoError(t, err)
					require.NotNil(t, child)
					parentIndex := 1 - i
					if tt.name == "same-family roots restored" {
						parentIndex = 1
						if strings.HasPrefix(uri, root+"/") {
							parentIndex = 0
						}
					}
					parents, err := database.ListSessionIDsByFilePath(t.Context(), paths[parentIndex], "cursor")
					require.NoError(t, err)
					if len(parents) == 0 || (tt.name == "new child after parent root removed" && !parentEvidenceRestored) {
						assert.Nil(t, child.ParentSessionID, "a different project's parent cannot own this child")
					} else {
						require.Len(t, parents, 1)
						assert.Equal(t, parents[0], derefString(child.ParentSessionID))
					}
					assert.Equal(t, "subagent", child.RelationshipType)
					assert.Equal(t, "host-a~"+parser.CursorSessionID(paths[parentIndex]), derefString(child.ParserParentSessionID))
					messages, err := database.GetAllMessages(t.Context(), ids[0])
					require.NoError(t, err)
					require.Len(t, messages, 2)
					expected := []string{"Hello A", "Hello B"}[parentIndex]
					if tt.name == "same-family roots restored" {
						expected = "Hello A"
					}
					assert.Equal(t, expected, messages[0].Content)
				}
			}
			for _, pass := range tt.passes {
				provider.discovered = nil
				for _, i := range pass {
					if tt.name == "own parent late" && i == 1 {
						paths[1] = aliasRoot + "/project-b/agent-transcripts/shared.txt"
					}
					if tt.name == "new child after parent root removed" && i == 2 {
						engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {aliasRoot}}})
					}
					if tt.name == "same-family roots restored" {
						selectedRoot := root
						if i > 0 {
							selectedRoot = aliasRoot
						}
						engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {selectedRoot}}})
					}
					provider.discovered = append(provider.discovered, source(paths[i]))
				}
				stats := engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				verify()
			}
			require.Len(t, childIDs, len(paths)-2)
			if tt.name == "same-family roots restored" {
				engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {aliasRoot, root}}})
				_, _, err := engine.processAndWriteSessionFile(t.Context(), parser.DiscoveredFile{
					Agent: parser.AgentCursor, Path: paths[2], Machine: "host-a", SourceSize: int64(len(content)), SourceMtime: time.Unix(200, 0).UnixNano(), ForceParse: true,
				}, childIDs[paths[2]])
				require.NoError(t, err)
				verify()
				for i, parent := range paths[:2] {
					uri := strings.TrimSuffix(parent, ".txt") + "/subagents/fresh-" + []string{"a", "b"}[i] + ".txt"
					paths = append(paths, uri)
					provider.discovered = []parser.SourceRef{source(uri)}
					stats := engine.SyncAll(t.Context(), nil)
					require.Zero(t, stats.Failed)
					verify()
				}
			}
			provider.discovered = []parser.SourceRef{source(paths[2])}
			if tt.name == "legacy wrong parent root removed" {
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET parent_session_id = ?, data_version = 128 WHERE id = ?", "host-a~cursor:shared", childIDs[paths[2]])
					return err
				}))
				engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {aliasRoot}}})
				provider.discovered = append(provider.discovered, source(paths[1]))
			}
			if tt.childrenCollide {
				provider.discovered = append(provider.discovered, source(paths[3]))
			}
			stats := engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
			require.Zero(t, stats.Failed)
			verify()
			if tt.name == "new child after parent root removed" {
				oldPath := paths[2]
				paths[2] = strings.TrimSuffix(oldPath, ".txt") + ".jsonl"
				childIDs[paths[2]] = childIDs[oldPath]
				for _, uri := range paths[2:] {
					_, _, err := engine.processAndWriteSessionFile(t.Context(), parser.DiscoveredFile{
						Agent: parser.AgentCursor, Path: uri, Machine: "host-a", SourceSize: int64(len(content)), SourceMtime: time.Unix(100, 0).UnixNano(), ForceParse: true,
					}, childIDs[uri])
					require.NoError(t, err)
					verify()
				}
			}
			if tt.name == "new child after parent root removed" {
				engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root, aliasRoot}}})
				parentEvidenceRestored = true
				_, _, err := engine.processAndWriteSessionFile(t.Context(), parser.DiscoveredFile{
					Agent: parser.AgentCursor, Path: paths[2], Machine: "host-a", SourceSize: int64(len(content)), SourceMtime: time.Unix(200, 0).UnixNano(), ForceParse: true,
				}, childIDs[paths[2]])
				require.NoError(t, err)
				verify()
			}
			if tt.name == "parent root removed" {
				before, err := database.GetSessionFull(t.Context(), childIDs[paths[2]])
				require.NoError(t, err)
				require.NotNil(t, before)
				oldLookup := lookupS3Provider
				t.Cleanup(func() { lookupS3Provider = oldLookup })
				lookupS3Provider = func(agent parser.AgentType) (parser.S3Provider, bool) {
					p, ok := parser.S3ProviderFor(agent)
					return cursorParentStatProvider{S3Provider: p, stat: func(uri string) (parser.S3Object, error) {
						return parser.S3Object{URI: uri, Size: int64(len(content)), LastModified: time.Unix(200, 0)}, nil
					}}, ok
				}
				engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {aliasRoot}}})
				refreshed = true
				verifyRefresh := func() {
					t.Helper()
					after, err := database.GetSessionFull(t.Context(), before.ID)
					require.NoError(t, err)
					require.NotNil(t, after)
					assert.Equal(t, before.ParentSessionID, after.ParentSessionID)
					assert.Equal(t, before.ParserParentSessionID, after.ParserParentSessionID)
					assert.Equal(t, "subagent", after.RelationshipType)
					assert.Equal(t, paths[2], derefString(after.FilePath))
					messages, err := database.GetAllMessages(t.Context(), before.ID)
					require.NoError(t, err)
					require.Len(t, messages, 2)
					assert.Equal(t, "Refreshed B", messages[0].Content)
				}
				updated := source(paths[2])
				remote := updated.Opaque.(parser.S3DiscoveredSource)
				remote.MtimeNS = time.Unix(200, 0).UnixNano()
				updated.Opaque = remote
				provider.discovered = []parser.SourceRef{updated}
				stats = engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				verifyRefresh()
				oldPath := paths[2]
				paths[2] = strings.TrimSuffix(oldPath, ".txt") + ".jsonl"
				childIDs[paths[2]] = childIDs[oldPath]
				updated.Key, updated.DisplayPath, updated.FingerprintKey = paths[2], paths[2], paths[2]
				remote.URI = paths[2]
				updated.Opaque = remote
				provider.discovered = []parser.SourceRef{updated}
				stats = engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				verifyRefresh()
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), before.ID))
				verifyRefresh()
				stats = engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
				require.Zero(t, stats.Failed)
				verifyRefresh()
			}
		})
	}
}
