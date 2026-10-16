package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	identityThreadA = "11111111-1111-7111-8111-111111111111"
	identityThreadB = "22222222-2222-7222-8222-222222222222"
)

func writeCodexIdentityRollouts(t *testing.T) (root, pathA, pathB string) {
	t.Helper()
	root = t.TempDir()
	const ts = "2025-10-20T19:00:00Z"
	metaA := testjsonl.CodexSessionMetaJSON(identityThreadA, "/work/project-a", "codex_cli_rs", ts)
	pathA = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-00-00-"+identityThreadA+".jsonl", testjsonl.JoinJSONL(
		metaA,
		testjsonl.CodexMsgJSON("user", "first question", ts),
		testjsonl.CodexMsgJSON("assistant", "first answer", ts),
		testjsonl.CodexMsgJSON("user", "second question", ts),
		testjsonl.CodexMsgJSON("assistant", "second answer", ts),
	))
	pathB = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-09-52-"+identityThreadB+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(identityThreadB, "/work/project-b", "codex_cli_rs", ts),
		metaA,
		testjsonl.CodexMsgJSON("user", "other question", ts),
	))
	return root, pathA, pathB
}

func TestCodexForeignMetadataKeepsSessionsSeparate(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, staged := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%t/staged=%t", reverse, staged), func(t *testing.T) {
				root, pathA, pathB := writeCodexIdentityRollouts(t)
				database := openTestDB(t)
				cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
				if staged {
					cfg.StagedCodexParseMinBytes = 1
				}
				engine := NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
				paths := []string{pathA, pathB}
				if reverse {
					paths = []string{pathB, pathA}
				}
				for _, path := range paths {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				}
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, database, "codex:"+identityThreadA))
				assert.Equal(t, []string{"other question"}, messageContents(t, database, "codex:"+identityThreadB))

				// A later foreign header forces a full parse even after a checkpoint.
				f, err := os.OpenFile(pathB, os.O_APPEND|os.O_WRONLY, 0o600)
				require.NoError(t, err)
				_, err = f.WriteString(testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON(identityThreadA, "/work/project-a", "codex_cli_rs", "2025-10-20T19:10:00Z"),
					testjsonl.CodexMsgJSON("assistant", "other answer", "2025-10-20T19:10:01Z"),
				))
				require.NoError(t, err)
				require.NoError(t, f.Close())
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{pathB}))
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, database, "codex:"+identityThreadA))
				assert.Equal(t, []string{"other question", "other answer"}, messageContents(t, database, "codex:"+identityThreadB))
			})
		}
	}
}

func TestCodexForeignMetadataUpgradeRepairsMisfiledSession(t *testing.T) {
	for _, tc := range []struct {
		name            string
		originalPresent bool
		renamedFile     bool
		version         int
		missing         bool
		trash           bool
		snapshot        bool
		matchingRoot    bool
		page            bool
		occupied        bool
		compression     string
	}{
		{name: "plain_to_compressed", originalPresent: true, compression: "compress", version: 135},
		{name: "compressed_to_plain", originalPresent: true, compression: "decompress", version: 135},
		{name: "trash_occupied_destination", originalPresent: true, trash: true, missing: true, occupied: true, version: 135},
		{name: "original_present", originalPresent: true, version: 134},
		{name: "original_absent", version: 134},
		{name: "noncanonical_filename", originalPresent: true, renamedFile: true, version: 134},
		{name: "version_135", version: 135},
		{name: "corrected_source_missing", originalPresent: true, missing: true, version: 135},
		{name: "missing_noncanonical_source", originalPresent: true, renamedFile: true, missing: true, version: 135},
		{name: "missing_misfiled_page", originalPresent: true, page: true, missing: true, version: 135},
		{name: "missing_page_destination_owned", originalPresent: true, page: true, missing: true, occupied: true, version: 135},
		{name: "both_sources_missing", missing: true, version: 135},
		{name: "trash", originalPresent: true, trash: true, version: 135},
		{name: "trash_noncanonical", originalPresent: true, renamedFile: true, trash: true, version: 135},
		{name: "trash_missing_source", originalPresent: true, missing: true, trash: true, version: 135},
		{name: "foreign_project_snapshot", originalPresent: true, snapshot: true, version: 135},
		{name: "matching_project_snapshot", originalPresent: true, snapshot: true, matchingRoot: true, version: 135},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, pathA, pathB := writeCodexIdentityRollouts(t)
			correctedID := "codex:" + identityThreadB
			if tc.page {
				const suffix = "_33333333-3333-7333-8333-333333333333"
				pagePath := strings.TrimSuffix(pathB, ".jsonl") + suffix + ".jsonl"
				require.NoError(t, os.Rename(pathB, pagePath))
				pathB = pagePath
				correctedID += suffix
			}
			if tc.renamedFile {
				restored := filepath.Join(filepath.Dir(pathB), "rollout-restored.jsonl")
				require.NoError(t, os.Rename(pathB, restored))
				pathB = restored
			}
			if !tc.originalPresent {
				require.NoError(t, os.Remove(pathA))
			}
			var correctedRoot string
			if tc.snapshot {
				correctedRoot = t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(correctedRoot, ".git"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(correctedRoot, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/project-b.git\n"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(correctedRoot, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
				content, err := os.ReadFile(pathB)
				require.NoError(t, err)
				// JSON-escape the fixture cwd on Windows too.
				cwdJSON := fmt.Sprintf("%q", filepath.ToSlash(correctedRoot))
				require.NoError(t, os.WriteFile(pathB, []byte(strings.ReplaceAll(string(content), `"/work/project-b"`, cwdJSON)), 0o600))
			}
			contentB, err := os.ReadFile(pathB)
			require.NoError(t, err)
			if tc.missing {
				require.NoError(t, os.Remove(pathB))
			}
			if tc.missing && !tc.originalPresent {
				writeCodexRollout(t, root, "02", "rollout-unrelated.jsonl", testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON("unrelated", "/work/other", "codex_cli_rs", "2025-10-20T19:00:00Z"),
					testjsonl.CodexMsgJSON("user", "unrelated", "2025-10-20T19:00:01Z")))
			}
			if tc.compression == "decompress" {
				pathB += ".zst"
			}
			database := openTestDB(t)
			// Earlier versions could store B's transcript as A and lose A's full history.
			const legacyID = "codex:" + identityThreadA
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: legacyID, Agent: "codex", Project: "project-a", Machine: "local", FilePath: &pathB,
				DataVersion: tc.version, MessageCount: 1, UserMessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: legacyID, Role: "user", Content: "other question"}}))
			name, note := "Saved conversation", "Keep this question"
			require.NoError(t, database.RenameSession(t.Context(), legacyID, &name))
			_, err = database.StarSession(t.Context(), legacyID)
			require.NoError(t, err)
			messages, err := database.GetAllMessages(t.Context(), legacyID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			_, err = database.PinMessage(t.Context(), legacyID, messages[0].ID, &note)
			require.NoError(t, err)
			if tc.trash {
				require.NoError(t, database.SoftDeleteSession(t.Context(), legacyID))
			}
			if tc.snapshot {
				historicalRoot := "/work/project-a"
				if tc.matchingRoot {
					historicalRoot = correctedRoot
				}
				require.NoError(t, database.UpsertProjectIdentityObservationWithSnapshotProject(t.Context(), export.ProjectIdentityObservation{
					SessionID: legacyID, Project: "project-a", Machine: "local", RootPath: historicalRoot,
					GitRemote: "https://example.com/team/project-a.git", RemoteResolution: export.ProjectResolutionResolved,
					ObservedAt: time.Date(2025, 10, 20, 19, 0, 0, 0, time.UTC),
				}, "project-a"))
			}
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "retained", Agent: "gemini", Project: "sample", Machine: "local", MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: "retained", Role: "user", Content: "Only stored in the archive"}}))
			if tc.occupied {
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: correctedID, Agent: "codex", Project: "sample", Machine: "local", MessageCount: 1}))
				require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: correctedID, Role: "user", Content: "independent archive"}}))
			}
			if tc.compression == "compress" {
				require.NoError(t, os.Remove(pathB))
				require.NoError(t, os.WriteFile(pathB+".zst", zstdCompressString(t, string(contentB)), 0o600))
			}
			reopened, engine := rebuildCodexIdentityArchive(t, database, root, tc.version)
			if tc.compression != "" {
				oldIDs, err := reopened.ListSessionIDsByFilePath(t.Context(), pathB, "codex")
				require.NoError(t, err)
				assert.Empty(t, oldIDs, "one source must not leave a duplicate under its old representation")
				if tc.compression == "compress" {
					pathB += ".zst"
				} else {
					pathB = strings.TrimSuffix(pathB, ".zst")
				}
			}
			if tc.occupied {
				assert.Equal(t, []string{"independent archive"}, messageContents(t, reopened, correctedID))
			}
			if tc.missing && tc.occupied && tc.trash {
				correctedID = parser.AltSessionID(legacyID, pathB)
			} else if tc.missing && (tc.renamedFile || tc.occupied) {
				ids, err := reopened.ListSessionIDsByFilePath(t.Context(), pathB, "codex")
				require.NoError(t, err)
				require.Len(t, ids, 1, "retain the archived transcript even without its canonical identity")
				correctedID = ids[0]
				assert.NotEqual(t, legacyID, correctedID)
			}
			if tc.trash {
				require.True(t, reopened.IsSessionTrashed(t.Context(), correctedID))
			}
			if tc.trash && !tc.occupied {
				_, err = reopened.RestoreSession(t.Context(), correctedID)
				require.NoError(t, err)
				if tc.missing {
					require.NoError(t, os.WriteFile(pathB, contentB, 0o600))
				}
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{pathB}))
			}
			if tc.originalPresent {
				assert.False(t, reopened.IsSessionTrashed(t.Context(), legacyID))
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, reopened, legacyID))
				assert.Equal(t, pathA, reopened.GetSessionFilePath(t.Context(), legacyID))
				if tc.trash {
					original, err := reopened.GetSession(t.Context(), legacyID)
					require.NoError(t, err)
					require.NotNil(t, original)
					assert.Nil(t, original.DisplayName)
				}
			} else {
				stale, err := reopened.GetSession(t.Context(), legacyID)
				require.NoError(t, err)
				assert.Nil(t, stale, "B's misfiled transcript must not also survive under A")
			}
			assert.Equal(t, []string{"other question"}, messageContents(t, reopened, correctedID))
			assert.Equal(t, []string{"Only stored in the archive"}, messageContents(t, reopened, "retained"))
			assert.Equal(t, pathB, reopened.GetSessionFilePath(t.Context(), correctedID))
			if tc.trash && tc.occupied {
				_, err := reopened.RestoreSession(t.Context(), correctedID)
				require.NoError(t, err)
			}
			pins, err := reopened.ListPinnedMessages(t.Context(), "", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, correctedID, pins[0].SessionID)
			assert.Equal(t, &note, pins[0].Note)
			corrected, err := reopened.GetAllMessages(t.Context(), correctedID)
			require.NoError(t, err)
			require.Len(t, corrected, 1)
			assert.Equal(t, corrected[0].ID, pins[0].MessageID)
			curatedID := correctedID
			if tc.originalPresent && !tc.trash {
				curatedID = legacyID
			}
			curated, err := reopened.GetSession(t.Context(), curatedID)
			require.NoError(t, err)
			require.NotNil(t, curated)
			assert.Equal(t, &name, curated.DisplayName)
			stars, err := reopened.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{curatedID}, stars)

			if tc.missing && tc.renamedFile {
				engine.Close()
				again, againEngine := rebuildCodexIdentityArchive(t, reopened, root, tc.version)
				assert.Equal(t, []string{"other question"}, messageContents(t, again, correctedID))
				retainedPins, err := again.ListPinnedMessages(t.Context(), "", "")
				require.NoError(t, err)
				require.Len(t, retainedPins, 1)
				assert.Equal(t, correctedID, retainedPins[0].SessionID)
				// Curation made on the retained alternative belongs to this
				// source even after its canonical identity becomes readable.
				recoveredName := "Recovered conversation"
				require.NoError(t, again.RenameSession(t.Context(), correctedID, &recoveredName))
				_, err = again.StarSession(t.Context(), correctedID)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(pathB, contentB, 0o600))
				require.NoError(t, againEngine.SyncPathsContext(t.Context(), []string{pathB}))
				retainedPins, err = again.ListPinnedMessages(t.Context(), "", "")
				require.NoError(t, err)
				require.Len(t, retainedPins, 1)
				assert.Equal(t, "codex:"+identityThreadB, retainedPins[0].SessionID)
				assert.Equal(t, &note, retainedPins[0].Note)
				recovered, err := again.GetSession(t.Context(), "codex:"+identityThreadB)
				require.NoError(t, err)
				require.NotNil(t, recovered)
				assert.Equal(t, &recoveredName, recovered.DisplayName)
				stars, err := again.ListStarredSessionIDs(t.Context())
				require.NoError(t, err)
				assert.Contains(t, stars, "codex:"+identityThreadB)
				alternative, err := again.GetSession(t.Context(), correctedID)
				require.NoError(t, err)
				assert.Nil(t, alternative)
			}
			if tc.snapshot {
				snapshots, err := reopened.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{correctedID})
				require.NoError(t, err)
				require.Contains(t, snapshots, correctedID)
				assert.Equal(t, correctedRoot, snapshots[correctedID].RootPath)
				expectedRemote := "https://example.com/team/project-b.git"
				if tc.matchingRoot {
					expectedRemote = "https://example.com/team/project-a.git"
				}
				assert.Equal(t, expectedRemote, snapshots[correctedID].GitRemote)
			}
		})
	}
}

func TestCodexForeignMetadataUpgradePreservesPermanentDeletion(t *testing.T) {
	for _, tc := range []struct {
		missing bool
		version int
		renamed bool
	}{
		{false, 134, false},
		{true, 134, false},
		{false, 135, false},
		{true, 135, false},
		{false, 135, true},
		{true, 135, true},
	} {
		t.Run(fmt.Sprintf("version=%d/missing=%t/renamed=%t", tc.version, tc.missing, tc.renamed), func(t *testing.T) {
			root, _, pathB := writeCodexIdentityRollouts(t)
			if tc.renamed {
				renamed := filepath.Join(filepath.Dir(pathB), "rollout-restored.jsonl")
				require.NoError(t, os.Rename(pathB, renamed))
				pathB = renamed
			}
			content, err := os.ReadFile(pathB)
			require.NoError(t, err)
			if tc.missing {
				require.NoError(t, os.Remove(pathB))
			}
			database := openTestDB(t)
			const legacyID = "codex:" + identityThreadA
			const correctedID = "codex:" + identityThreadB
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: legacyID, Agent: "codex", Project: "sample", Machine: "local", FilePath: &pathB,
				DataVersion: tc.version, MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
				{SessionID: legacyID, Role: "user", Content: "other question"},
			}))
			require.NoError(t, database.DeleteSession(t.Context(), legacyID))
			reopened, engine := rebuildCodexIdentityArchive(t, database, root, tc.version)
			assert.True(t, reopened.IsSessionExcluded(t.Context(), legacyID), "the original exclusion remains")
			excludedPath, err := reopened.HasExcludedCodexSource(t.Context(), pathB, "codex")
			require.NoError(t, err)
			assert.True(t, excludedPath, "the deleted source stays excluded")
			deleted, err := reopened.GetSession(t.Context(), correctedID)
			require.NoError(t, err)
			assert.Nil(t, deleted)
			if tc.missing {
				require.NoError(t, os.WriteFile(pathB, content, 0o600))
			}
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{pathB}))
			deleted, err = reopened.GetSession(t.Context(), correctedID)
			require.NoError(t, err)
			assert.Nil(t, deleted, "a later sync must not resurrect the deleted transcript")
		})
	}
}

func rebuildCodexIdentityArchive(t *testing.T, database *db.DB, root string, version int) (*db.DB, *Engine) {
	t.Helper()
	path := database.Path()
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", version))
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	reopened, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	require.True(t, reopened.NeedsResync())
	engine := NewEngine(t.Context(), reopened, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	stats, err := engine.SyncThenRun(t.Context(), false, nil, func(full bool) error { assert.True(t, full); return nil })
	require.NoError(t, err)
	require.Zero(t, stats.Failed)
	require.False(t, stats.Aborted)
	require.True(t, stats.ArchiveRebuilt)
	assert.False(t, reopened.NeedsResync())
	return reopened, engine
}

func TestCodexIdentityDeletionDoesNotTrustFilename(t *testing.T) {
	for _, tc := range []struct{ missing, corrected bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		missing := tc.missing
		t.Run(fmt.Sprintf("deleted_source_missing=%t/corrected=%t", missing, tc.corrected), func(t *testing.T) {
			root, pathA, pathB := writeCodexIdentityRollouts(t)
			// A renamed export can name B while its first metadata still owns A.
			contentPath := pathA
			if tc.corrected {
				contentPath = pathB
			}
			content, err := os.ReadFile(contentPath)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(pathB, content, 0o600))
			if missing {
				require.NoError(t, os.Remove(pathB))
			}
			database := openTestDB(t)
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "codex:" + identityThreadA, Agent: "codex", Project: "sample", Machine: "local", FilePath: &pathB,
			}))
			require.NoError(t, database.DeleteSession(t.Context(), "codex:"+identityThreadA))
			writeCodexRollout(t, root, "02", "rollout-unrelated.jsonl", testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON("unrelated", "/work/other", "codex_cli_rs", "2025-10-20T19:00:00Z"),
				testjsonl.CodexMsgJSON("user", "unrelated", "2025-10-20T19:00:01Z")))
			// B's independent transcript exists only in the archive.
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "codex:" + identityThreadB, Agent: "codex", Project: "sample", Machine: "local", MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: "codex:" + identityThreadB, Role: "user", Content: "Unrelated archived history"}}))
			reopened, engine := rebuildCodexIdentityArchive(t, database, root, 135)
			assert.False(t, reopened.IsSessionExcluded(t.Context(), "codex:"+identityThreadB))
			assert.Equal(t, []string{"Unrelated archived history"}, messageContents(t, reopened, "codex:"+identityThreadB))
			if missing {
				require.NoError(t, os.WriteFile(pathB, content, 0o600))
			}
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{pathB}))
			assert.Equal(t, []string{"Unrelated archived history"}, messageContents(t, reopened, "codex:"+identityThreadB))
		})
	}
}

func TestCodexDeletedSourceChangesCompressionDuringUpgrade(t *testing.T) {
	for _, compressedFirst := range []bool{false, true} {
		for _, returnsLater := range []bool{false, true} {
			t.Run(fmt.Sprintf("compressed_first=%t/returns_later=%t", compressedFirst, returnsLater), func(t *testing.T) {
				root, _, plain := writeCodexIdentityRollouts(t)
				raw, err := os.ReadFile(plain)
				require.NoError(t, err)
				compressed := plain + ".zst"
				oldPath, newPath := plain, compressed
				newContent := zstdCompressString(t, string(raw))
				if compressedFirst {
					oldPath, newPath, newContent = compressed, plain, raw
				}
				database := openTestDB(t)
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "codex:" + identityThreadA, Agent: "codex", Project: "sample", Machine: "local", FilePath: &oldPath}))
				require.NoError(t, database.DeleteSession(t.Context(), "codex:"+identityThreadA))
				require.NoError(t, os.Remove(plain))
				if !returnsLater {
					require.NoError(t, os.WriteFile(newPath, newContent, 0o600))
				}
				rebuilt, engine := rebuildCodexIdentityArchive(t, database, root, 135)
				if returnsLater {
					require.NoError(t, os.WriteFile(newPath, newContent, 0o600))
				}
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{newPath}))
				restored, err := rebuilt.GetSessionFull(t.Context(), "codex:"+identityThreadB)
				require.NoError(t, err)
				assert.Nil(t, restored)
				assert.True(t, rebuilt.IsSessionExcluded(t.Context(), "codex:"+identityThreadA))
			})
		}
	}
}

func TestCodexCanonicalTrashedLegacyPageSurvivesUpgrade(t *testing.T) {
	root := t.TempDir()
	const rollout = "33333333-3333-7333-8333-333333333333"
	const pageID = "codex:" + identityThreadB + "_" + rollout
	path := writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-00-00-"+identityThreadB+"_"+rollout+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaWithFieldsJSON(rollout, "/work/sample", "codex_cli_rs", "2025-10-20T19:00:00Z", map[string]any{"session_id": identityThreadB}),
		testjsonl.CodexMsgJSON("user", "archived page", "2025-10-20T19:00:00Z")))
	database := openTestDB(t)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: pageID, Agent: "codex", Project: "sample", Machine: "local", FilePath: &path, DataVersion: 135, MessageCount: 1}))
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: pageID, Role: "user", Content: "archived page"}}))
	messages, err := database.GetAllMessages(t.Context(), pageID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	_, err = database.PinMessage(t.Context(), pageID, messages[0].ID, new("saved note"))
	require.NoError(t, err)
	require.NoError(t, database.SoftDeleteSession(t.Context(), pageID))
	// Keep discovery nonempty while the page is suppressed as trash.
	writeCodexRollout(t, root, "02", "rollout-other.jsonl", testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON("other", "/work/sample", "codex_cli_rs", "2025-10-20T19:00:00Z"), testjsonl.CodexMsgJSON("user", "other", "2025-10-20T19:00:00Z")))
	rebuilt, engine := rebuildCodexIdentityArchive(t, database, root, 135)
	assert.True(t, rebuilt.IsSessionTrashed(t.Context(), pageID), "already-canonical trash must keep its page identity")
	_, err = rebuilt.RestoreSession(t.Context(), pageID)
	require.NoError(t, err)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	pins, err := rebuilt.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, pageID, pins[0].SessionID)
	assert.Equal(t, new("saved note"), pins[0].Note)
	assert.Equal(t, []string{"archived page"}, messageContents(t, rebuilt, pageID))
}
