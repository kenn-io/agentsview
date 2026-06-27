package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestEnsureOriginPersists(t *testing.T) {
	database := testDB(t)

	first, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.NotEqual(t, "local", first)

	second, err := EnsureOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestIsFolderTargetAcceptsWindowsDrivePaths(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{"windows backslash path", `C:\Users\runner\artifacts`, true},
		{"windows slash path", `C:/Users/runner/artifacts`, true},
		{"posix path", "/tmp/agentsview-artifacts", true},
		{"relative path", "artifacts", true},
		{"http peer", "https://peer.example.test/artifacts", false},
		{"s3 target", "s3://bucket/artifacts", false},
		{"host port", "localhost:8080", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsFolderTarget(tt.target))
		})
	}
}

func TestAdoptOriginPersistsConfigOrigin(t *testing.T) {
	database := testDB(t)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)

	// EnsureOrigin and its callers now agree with the adopted origin instead
	// of generating a divergent DB-only value.
	ensured, err := EnsureOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", ensured)
}

func TestAdoptOriginIsIdempotent(t *testing.T) {
	database := testDB(t)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))
	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginOverwritesDivergentDBOrigin(t *testing.T) {
	database := testDB(t)

	// Simulate the pre-fix state: the recorder generated a DB-only origin
	// before the authoritative config origin existed.
	stale, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEqual(t, "desk-a1b2c3", stale)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginRejectsInvalidOrigin(t *testing.T) {
	database := testDB(t)

	err := AdoptOrigin(database, "../outside")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adopting artifact origin")

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

func TestEnsureOriginRejectsInvalidPersistedOrigin(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.SetSyncState(originStateKey, "../outside"))

	origin, err := EnsureOrigin(database)
	require.Error(t, err)
	assert.Empty(t, origin)
	assert.Contains(t, err.Error(), "stored artifact origin")
	assert.Contains(t, err.Error(), "invalid artifact origin")
}

func TestSyncFolderRoundTripImportsForeignSession(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	aRes, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	assert.Equal(t, "laptop-a1b2c3", aRes.Origin)
	assert.Equal(t, 1, aRes.ExportedSessions)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Equal(t, 1, bRes.ImportedSessions)
	assert.Equal(t, 2, bRes.ImportedMessages)

	bRes, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, bRes.ImportedSessions)
	assert.Zero(t, bRes.ImportedMessages)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "laptop-a1b2c3", got.Machine)
	assert.Equal(t, "alpha", got.Project)

	msgs, err := bDB.GetAllMessages(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "world", msgs[1].Content)
	assert.Equal(t, filepath.Join(bData, "artifacts", "laptop-a1b2c3"), filepath.Join(bData, "artifacts", got.Machine))
}

func TestSyncFolderRoundTripPreservesSessionName(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	sessionName := "Parser Provided Title"
	seedSession(t, aDB, "sess-1", "alpha", func(s *db.Session) {
		s.SessionName = &sessionName
	})

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.SessionName)
	assert.Equal(t, sessionName, *got.SessionName)
}

func TestSyncFolderImportClearsSourceFileState(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)
	sourcePath := filepath.Join(t.TempDir(), "shared-session.jsonl")
	peerHash := strings64("a")
	localHash := strings64("b")
	lastEntryUUID := "entry-99"

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha", func(s *db.Session) {
		s.FilePath = &sourcePath
		s.FileSize = new(int64)
		*s.FileSize = 4096
		s.FileMtime = new(int64)
		*s.FileMtime = 200
		s.NextOrdinal = 99
		s.LastEntryUUID = &lastEntryUUID
		s.FileInode = new(int64)
		*s.FileInode = 12345
		s.FileDevice = new(int64)
		*s.FileDevice = 67890
		s.FileHash = &peerHash
	})
	seedSession(t, bDB, "local-sess", "alpha", func(s *db.Session) {
		s.FilePath = &sourcePath
		s.FileMtime = new(int64)
		*s.FileMtime = 100
		s.FileHash = &localHash
	})

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	res, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, res.ImportedSessions)

	imported, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, imported)
	assert.Nil(t, imported.FilePath)
	assert.Nil(t, imported.FileSize)
	assert.Nil(t, imported.FileMtime)
	assert.Zero(t, imported.NextOrdinal)
	assert.Nil(t, imported.LastEntryUUID)
	assert.Nil(t, imported.FileInode)
	assert.Nil(t, imported.FileDevice)
	assert.Nil(t, imported.FileHash)

	ids, err := bDB.ListSessionIDsByFilePath(sourcePath, "claude")
	require.NoError(t, err)
	assert.Equal(t, []string{"local-sess"}, ids)
	gotHash, ok := bDB.GetFileHashByPath(sourcePath)
	require.True(t, ok)
	assert.Equal(t, localHash, gotHash)
}

func TestSyncFolderRoundTripPreservesSessionSignals(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	// Signal columns are written outside UpsertSession, so seed them through
	// the same writer paths the live app uses. These include fields the Session
	// JSON drops (json:"-"): has_tool_calls, has_context_data, and the quality
	// scalars. Secret-scan state is seeded too, but unlike the others it is
	// deliberately not carried across import (asserted below).
	require.NoError(t, aDB.UpdateSessionSignals("sess-1", db.SessionSignalUpdate{
		HasToolCalls:   true,
		HasContextData: true,
		Outcome:        "success",
		QualitySignals: db.QualitySignals{
			Version:              3,
			ShortPromptCount:     2,
			UnstructuredStart:    true,
			RunawayToolLoopCount: 1,
		},
	}))
	require.NoError(t, aDB.ReplaceSessionSecretFindings("sess-1", nil, 0, "rules-v7"))

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.HasToolCalls, "has_tool_calls should survive the round trip")
	assert.True(t, got.HasContextData, "has_context_data should survive the round trip")
	assert.Equal(t, "success", got.Outcome)
	// Secret findings are not carried in the manifest, so the imported session
	// is treated as unscanned: the source rules version is dropped so
	// `secrets scan --backfill` rescans it with local rules.
	assert.Empty(t, got.SecretsRulesVersion,
		"secret-scan state must not be restored without findings")

	qs := got.StoredQualitySignals()
	require.NotNil(t, qs, "quality signals should survive the round trip")
	assert.Equal(t, 3, qs.Version)
	assert.Equal(t, 2, qs.ShortPromptCount)
	assert.True(t, qs.UnstructuredStart)
	assert.Equal(t, 1, qs.RunawayToolLoopCount)
}

func TestSyncFolderRoundTripRewritesForeignRelationshipIDs(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "source-1", "alpha")
	seedSession(t, aDB, "parent-1", "alpha")
	seedSession(t, aDB, "child-1", "alpha")
	parentID := "parent-1"
	seedSession(t, aDB, "sess-1", "alpha", func(s *db.Session) {
		s.SourceSessionID = "source-1"
		s.ParentSessionID = &parentID
	})
	require.NoError(t, aDB.ReplaceSessionMessages("sess-1", []db.Message{
		{
			SessionID:     "sess-1",
			Ordinal:       0,
			Role:          "assistant",
			Content:       "delegating",
			ContentLength: 10,
			ToolCalls: []db.ToolCall{{
				ToolName:          "Task",
				Category:          "Task",
				ToolUseID:         "toolu_1",
				SubagentSessionID: "child-1",
				ResultEvents: []db.ToolResultEvent{{
					ToolUseID:         "toolu_1",
					AgentID:           "agent-1",
					SubagentSessionID: "child-1",
					Source:            "tool_result",
					Status:            "success",
					Content:           "done",
					ContentLength:     4,
					EventIndex:        0,
				}},
			}},
		},
	}))

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	_, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "laptop-a1b2c3~source-1", got.SourceSessionID)
	require.NotNil(t, got.ParentSessionID)
	assert.Equal(t, "laptop-a1b2c3~parent-1", *got.ParentSessionID)

	msgs, err := bDB.GetAllMessages(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "laptop-a1b2c3~child-1", msgs[0].ToolCalls[0].SubagentSessionID)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "laptop-a1b2c3~child-1",
		msgs[0].ToolCalls[0].ResultEvents[0].SubagentSessionID)
}

// TestSyncFolderImportLeavesScannedSessionBackfillable verifies that a session
// scanned for secrets at the source (rules version, a finding row, a nonzero
// leak count) is imported as unscanned, because the manifest carries no finding
// rows. The imported session must have no leak count and no rules version, so it
// stays a `secrets scan --backfill` candidate even when the source rules version
// is current on the importing machine. Stamping it scanned-at-source-version
// would skip a secret-bearing session, leaving a leak count with no revealable
// findings.
func TestSyncFolderImportLeavesScannedSessionBackfillable(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	// The source session is fully scanned: a finding row, a nonzero leak count,
	// and a rules version that is also current on the importing machine below.
	const rulesVersion = "rules-current"
	findings := []db.SecretFinding{{
		SessionID:      "sess-1",
		RuleName:       "aws-access-key",
		Confidence:     "definite",
		LocationKind:   "message",
		MessageOrdinal: 1,
		MatchStart:     4,
		MatchEnd:       24,
		RedactedMatch:  "AKIA…MPLE",
		RulesVersion:   rulesVersion,
	}}
	require.NoError(t, aDB.ReplaceSessionSecretFindings("sess-1", findings, 1, rulesVersion))

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	const importedID = "laptop-a1b2c3~sess-1"
	got, err := bDB.GetSessionFull(ctx, importedID)
	require.NoError(t, err)
	require.NotNil(t, got)
	// Imported session must be unscanned so its state is consistent with the zero
	// findings carried in the manifest.
	assert.Empty(t, got.SecretsRulesVersion, "imported session must not be stamped scanned")
	assert.Zero(t, got.SecretLeakCount, "imported session must not claim leaks without findings")

	// With the source rules version current on the importing machine, backfill
	// must still treat the imported session as a candidate (secrets_rules_version
	// "" != current) instead of skipping it.
	cands, err := bDB.SecretScanCandidates(ctx, db.SecretScanCandidateFilter{
		CurrentVersion: rulesVersion,
		OnlyStale:      true,
	})
	require.NoError(t, err)
	assert.Contains(t, cands, importedID,
		"secret-bearing imported session must be a backfill candidate, not skipped")
}

// TestSyncFolderSourceLeakCountChangeKeepsLocalFindings verifies that a
// source-side secret rescan that changes only secret_leak_count (not message
// content) does not alter the artifact manifest hash, so the importer neither
// re-imports the session nor clears the findings it scanned locally.
// secret_leak_count is the only secret field carried in the Session JSON, and
// import discards secret-scan state, so it must not influence the
// content-addressed manifest.
func TestSyncFolderSourceLeakCountChangeKeepsLocalFindings(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	// First round trip: A exports sess-1 (no secrets yet), B imports it.
	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	const importedID = "laptop-a1b2c3~sess-1"

	// B scans the imported session locally and records a finding.
	bFinding := []db.SecretFinding{{
		SessionID: importedID, RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		RedactedMatch: "AKIA…MPLE", RulesVersion: "rules-b",
	}}
	require.NoError(t, bDB.ReplaceSessionSecretFindings(importedID, bFinding, 1, "rules-b"))

	// A rescans sess-1: only secret_leak_count changes (0 -> 1); the message
	// content A exports is untouched.
	aFinding := []db.SecretFinding{{
		SessionID: "sess-1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		RedactedMatch: "AKIA…MPLE", RulesVersion: "rules-a",
	}}
	require.NoError(t, aDB.ReplaceSessionSecretFindings("sess-1", aFinding, 1, "rules-a"))

	// Second round trip after the source-only rescan.
	_, err = SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	bRes, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, bRes.ImportedSessions,
		"a source-only leak-count change must not re-import the session")

	// B's locally scanned findings and scan state survive.
	got, err := bDB.SessionSecretFindings(ctx, importedID)
	require.NoError(t, err)
	assert.Len(t, got, 1, "importer's local secret findings must not be cleared")

	sess, err := bDB.GetSessionFull(ctx, importedID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 1, sess.SecretLeakCount, "importer's leak count preserved")
	assert.Equal(t, "rules-b", sess.SecretsRulesVersion, "importer's scan version preserved")
}

func TestExportSessionReusesPreNormalizationManifestHashForIgnoredLocalFields(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	originRoot := filepath.Join(root, origin)
	seedSession(t, database, "sess-1", "alpha")

	finding := []db.SecretFinding{{
		SessionID: "sess-1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5,
		RedactedMatch: "hello", RulesVersion: "rules-a",
	}}
	require.NoError(t, database.ReplaceSessionSecretFindings("sess-1", finding, 1, "rules-a"))

	prevManifestHash := writePreNormalizationManifest(t, ctx, database, originRoot, origin, "sess-1")
	require.NoError(t, database.SetSyncState(exportStateKey(origin, "sess-1"), prevManifestHash))

	gotHash, changed, err := exportSession(ctx, database, originRoot, origin, "sess-1", prevManifestHash)
	require.NoError(t, err)
	assert.Equal(t, prevManifestHash, gotHash)
	assert.False(t, changed,
		"an old manifest that only differs by ignored local fields should remain the export watermark")

	manifests := globArtifacts(t, root, origin, "manifests", "*"+manifestExtension)
	require.Len(t, manifests, 1)
	assert.Equal(t, prevManifestHash+manifestExtension, filepath.Base(manifests[0]))
}

func TestSyncFolderNotifiesWhenImportWritesData(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	changes := 0
	_, err = SyncFolder(ctx, bDB, SyncOptions{
		DataDir:       bData,
		Target:        share,
		OnDataChanged: func() { changes++ },
	})
	require.NoError(t, err)
	assert.Equal(t, 1, changes)

	_, err = SyncFolder(ctx, bDB, SyncOptions{
		DataDir:       bData,
		Target:        share,
		OnDataChanged: func() { changes++ },
	})
	require.NoError(t, err)
	assert.Equal(t, 1, changes)
}

func TestSyncFolderUsesProvidedOrigin(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	dataDir := t.TempDir()
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	res, err := SyncFolder(ctx, database, SyncOptions{
		DataDir: dataDir,
		Target:  share,
		Origin:  "configured-a1b2c3",
	})
	require.NoError(t, err)

	assert.Equal(t, "configured-a1b2c3", res.Origin)
	persisted, err := database.GetSyncState(originStateKey)
	require.NoError(t, err)
	assert.Empty(t, persisted)
	manifests := globArtifacts(t, share, "configured-a1b2c3", "manifests", "*"+manifestExtension)
	assert.Len(t, manifests, 1)
}

func TestImportMaintainsFTS(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	exportDB := testDB(t)
	importDB := testDB(t)
	if !importDB.HasFTS() {
		t.Skip("FTS unavailable")
	}
	seedSession(t, exportDB, "sess-1", "alpha")

	_, err := Export(ctx, exportDB, root, origin)
	require.NoError(t, err)
	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.NoError(t, err)
	require.Equal(t, 1, imported)
	require.Equal(t, 2, messages)

	page, err := importDB.Search(ctx, db.SearchFilter{Query: "world", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, origin+"~sess-1", page.Results[0].SessionID)
}

func TestImportPreservesPinsAndStatsOnRewrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")

	_, err := Export(ctx, exportDB, root, origin)
	require.NoError(t, err)
	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.NoError(t, err)
	require.Equal(t, 1, imported)
	require.Equal(t, 2, messages)

	gid := origin + "~sess-1"
	importedMsgs, err := importDB.GetAllMessages(ctx, gid)
	require.NoError(t, err)
	require.Len(t, importedMsgs, 2)
	note := "keep this pin"
	_, err = importDB.PinMessage(gid, importedMsgs[1].ID, &note)
	require.NoError(t, err)

	require.NoError(t, exportDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "planet", ContentLength: 6},
	}))
	_, err = Export(ctx, exportDB, root, origin)
	require.NoError(t, err)
	imported, messages, err = Import(ctx, importDB, root, "desktop-d4e5f6")
	require.NoError(t, err)
	require.Equal(t, 1, imported)
	require.Equal(t, 2, messages)

	pins, err := importDB.ListPinnedMessages(ctx, gid, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 1, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)

	allPins, err := importDB.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err)
	require.Len(t, allPins, 1)
	assert.Equal(t, gid, allPins[0].SessionID)
	require.NotNil(t, allPins[0].Content)
	assert.Equal(t, "planet", *allPins[0].Content)

	stats, err := importDB.GetStats(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionCount)
	assert.Equal(t, 2, stats.MessageCount)
	assert.Equal(t, 1, stats.ProjectCount)
	assert.Equal(t, 1, stats.MachineCount)
}

func TestImportDoesNotAdvanceStateForExcludedOrTrashedSessions(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	exportDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := Export(ctx, exportDB, root, origin)
	require.NoError(t, err)

	gid := origin + "~sess-1"
	tests := []struct {
		name string
		seed func(*testing.T, *db.DB)
	}{
		{
			name: "excluded",
			seed: func(t *testing.T, database *db.DB) {
				t.Helper()
				seedSession(t, database, gid, "alpha", func(s *db.Session) {
					s.Machine = origin
				})
				require.NoError(t, database.DeleteSession(gid))
			},
		},
		{
			name: "trashed",
			seed: func(t *testing.T, database *db.DB) {
				t.Helper()
				seedSession(t, database, gid, "alpha", func(s *db.Session) {
					s.Machine = origin
				})
				require.NoError(t, database.SoftDeleteSession(gid))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			importDB := testDB(t)
			tt.seed(t, importDB)

			imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
			require.NoError(t, err)
			assert.Zero(t, imported)
			assert.Zero(t, messages)

			state, err := importDB.GetSyncState(importStateKey(origin, gid))
			require.NoError(t, err)
			assert.Empty(t, state)
		})
	}
}

func TestSyncFolderRetriesIncompleteForeignArtifacts(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	segments := globArtifacts(t, share, "laptop-a1b2c3", "segments", "*"+segmentExtension)
	require.Len(t, segments, 1)
	require.NoError(t, os.Remove(segments[0]))

	res, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, res.ImportedSessions)
	assert.Zero(t, res.ImportedMessages)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	assert.Nil(t, got)

	require.NoError(t, CopyUnion(filepath.Join(aData, "artifacts"), share))
	res, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Equal(t, 1, res.ImportedSessions)
	assert.Equal(t, 2, res.ImportedMessages)
}

func TestSyncFolderSkipsCheckpointWithMissingManifest(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	manifests := globArtifacts(t, share, "laptop-a1b2c3", "manifests", "*"+manifestExtension)
	require.Len(t, manifests, 1)
	require.NoError(t, os.Remove(manifests[0]))

	res, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, res.ImportedSessions)
	assert.Zero(t, res.ImportedMessages)
}

func TestImportRejectsMismatchedCheckpointOrigin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)

	originRoot := filepath.Join(root, origin)
	cp, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, cp)
	cp.Origin = "spoofed-origin"
	writeCheckpoint(t, originRoot, *cp)

	importDB := testDB(t)
	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoint origin mismatch")
	assert.Zero(t, imported)
	assert.Zero(t, messages)

	got, err := importDB.GetSessionFull(ctx, origin+"~sess-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestImportRejectsMismatchedManifestOrigin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)

	originRoot := filepath.Join(root, origin)
	cp, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, cp)
	gid := origin + "~sess-1"
	manifestHash := cp.Sessions[gid]
	require.NotEmpty(t, manifestHash)

	m, err := readManifest(originRoot, manifestHash)
	require.NoError(t, err)
	m.Origin = "spoofed-origin"
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	spoofedHash := hashHex(manifestData)
	require.NoError(t, writeCompressed(filepath.Join(originRoot, "manifests", spoofedHash+manifestExtension), manifestData))
	cp.Sessions[gid] = spoofedHash
	writeCheckpoint(t, originRoot, *cp)

	importDB := testDB(t)
	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest origin mismatch")
	assert.Zero(t, imported)
	assert.Zero(t, messages)

	got, err := importDB.GetSessionFull(ctx, gid)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestImportRejectsCorruptManifestHash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)

	originRoot := filepath.Join(root, origin)
	cp, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, cp)
	manifestHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, manifestHash)
	m, err := readManifest(originRoot, manifestHash)
	require.NoError(t, err)
	m.Session.Project = "tampered"
	data, err := canonicalJSON(m)
	require.NoError(t, err)
	path := filepath.Join(originRoot, "manifests", manifestHash+manifestExtension)
	require.NoError(t, os.Remove(path))
	require.NoError(t, writeCompressed(path, data))

	importDB := testDB(t)
	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest")
	assert.Contains(t, err.Error(), "hash mismatch")
	assert.Zero(t, imported)
	assert.Zero(t, messages)
}

func TestImportRejectsInvalidCheckpointManifestReference(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	importDB := testDB(t)
	originRoot := filepath.Join(root, origin)
	writeCheckpoint(t, originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{
			origin + "~sess-1": "../outside",
		},
	})

	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Contains(t, err.Error(), "invalid artifact hash")
	assert.Zero(t, imported)
	assert.Zero(t, messages)
}

func TestImportRejectsCorruptSegmentHash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)

	originRoot := filepath.Join(root, origin)
	cp, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, cp)
	manifestHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, manifestHash)
	m, err := readManifest(originRoot, manifestHash)
	require.NoError(t, err)
	require.Len(t, m.Segments, 1)
	segmentPath := filepath.Join(originRoot, "segments", m.Segments[0]+segmentExtension)
	require.NoError(t, os.Remove(segmentPath))
	require.NoError(t, writeCompressed(segmentPath, []byte("{\"v\":1,\"ordinal\":0,\"role\":\"user\",\"content\":\"tampered\"}\n")))

	importDB := testDB(t)
	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "segment")
	assert.Contains(t, err.Error(), "hash mismatch")
	assert.Zero(t, imported)
	assert.Zero(t, messages)
}

func TestImportRejectsInvalidManifestSegmentReference(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	importDB := testDB(t)
	originRoot := filepath.Join(root, origin)
	m := manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: db.Session{
			ID:        "sess-1",
			Machine:   origin,
			Agent:     "claude",
			Project:   "alpha",
			CreatedAt: "2026-06-14T01:02:03Z",
		},
		Segments: []string{"../outside"},
	}
	data, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(data)
	require.NoError(t, writeCompressed(filepath.Join(originRoot, "manifests", manifestHash+manifestExtension), data))
	writeCheckpoint(t, originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{
			origin + "~sess-1": manifestHash,
		},
	})

	imported, messages, err := Import(ctx, importDB, root, "desktop-d4e5f6")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Contains(t, err.Error(), "invalid artifact hash")
	assert.Zero(t, imported)
	assert.Zero(t, messages)
}

func TestSyncFolderRejectsOverlappingRoots(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		target func(string) string
	}{
		{
			name: "target is data dir",
			target: func(dataDir string) string {
				return dataDir
			},
		},
		{
			name: "target is artifact store",
			target: func(dataDir string) string {
				return filepath.Join(dataDir, "artifacts")
			},
		},
		{
			name: "target inside artifact store",
			target: func(dataDir string) string {
				return filepath.Join(dataDir, "artifacts", "share")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testDB(t)
			dataDir := t.TempDir()

			_, err := SyncFolder(ctx, database, SyncOptions{
				DataDir: dataDir,
				Target:  tt.target(dataDir),
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must not overlap")
		})
	}
}

func TestExportAppendsCheckpoints(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")

	count, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	count, err = Export(ctx, database, root, origin)
	require.NoError(t, err)
	assert.Zero(t, count)

	checkpoints, err := filepath.Glob(filepath.Join(root, origin, "checkpoints", "cp-*.json"))
	require.NoError(t, err)
	assert.Equal(t, []string{
		filepath.Join(root, origin, "checkpoints", "cp-0000000001.json"),
		filepath.Join(root, origin, "checkpoints", "cp-0000000002.json"),
	}, checkpoints)

	manifests := globArtifacts(t, root, origin, "manifests", "*"+manifestExtension)
	assert.Len(t, manifests, 1)
}

func TestExportEmitsNewManifestAfterDataVersionChange(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")

	count, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	cp, err := readLatestCheckpoint(filepath.Join(root, origin))
	require.NoError(t, err)
	require.NotNil(t, cp)
	firstHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, firstHash)

	require.NoError(t, database.SetSessionDataVersion("sess-1", 42))
	count, err = Export(ctx, database, root, origin)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	cp, err = readLatestCheckpoint(filepath.Join(root, origin))
	require.NoError(t, err)
	require.NotNil(t, cp)
	nextHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, nextHash)
	assert.NotEqual(t, firstHash, nextHash)

	m, err := readManifest(filepath.Join(root, origin), nextHash)
	require.NoError(t, err)
	assert.Equal(t, 42, m.DataVersion)
}

func TestExportIncludesLocalOwnedSessionClasses(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "file-sess", "alpha")
	seedSession(t, database, "claude-ai-sess", "bravo", func(s *db.Session) {
		s.Agent = "claude-ai"
	})
	seedSession(t, database, "upload-sess", "charlie", func(s *db.Session) {
		s.Agent = "upload"
	})
	seedSession(t, database, "orphan-sess", "delta", func(s *db.Session) {
		s.SourceSessionID = "missing-source"
	})

	count, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	assert.Equal(t, 4, count)

	cp, err := readLatestCheckpoint(filepath.Join(root, origin))
	require.NoError(t, err)
	require.NotNil(t, cp)
	assert.Contains(t, cp.Sessions, origin+"~file-sess")
	assert.Contains(t, cp.Sessions, origin+"~claude-ai-sess")
	assert.Contains(t, cp.Sessions, origin+"~upload-sess")
	assert.Contains(t, cp.Sessions, origin+"~orphan-sess")
}

func TestImportDefersFutureVersionCheckpoint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	importDB := testDB(t)
	cp := checkpoint{
		Version:  formatVersion + 1,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{
			origin + "~sess-1": "future-manifest",
		},
	}
	originRoot := filepath.Join(root, origin)
	require.NoError(t, os.MkdirAll(filepath.Join(originRoot, "checkpoints"), 0o755))
	data, err := canonicalJSON(cp)
	require.NoError(t, err)
	require.NoError(t, writeFileAtomic(
		filepath.Join(originRoot, "checkpoints", "cp-0000000001.json"),
		data,
		0o644,
	))

	res, err := ImportDetailed(ctx, importDB, root, "desktop-d4e5f6")
	require.NoError(t, err)
	assert.False(t, res.Changed())
}

func TestImportUsesLatestCompatibleCheckpointBeforeFutureCheckpoint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")

	_, err := Export(ctx, exportDB, root, origin)
	require.NoError(t, err)
	originRoot := filepath.Join(root, origin)
	writeCheckpoint(t, originRoot, checkpoint{
		Version:  formatVersion + 1,
		Origin:   origin,
		Sequence: 2,
		Sessions: map[string]string{
			origin + "~future": "future-manifest",
		},
	})

	res, err := ImportDetailed(ctx, importDB, root, localOrigin)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sessions)
	assert.Equal(t, 2, res.Messages)
}

func TestImportDefersFutureVersionManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	importDB := testDB(t)
	originRoot := filepath.Join(root, origin)
	m := manifest{
		Version:         formatVersion + 1,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: db.Session{
			ID:      "sess-1",
			Machine: origin,
		},
		Segments: []string{"future-segment"},
	}
	data, err := canonicalJSON(m)
	require.NoError(t, err)
	hash := hashHex(data)
	require.NoError(t, writeCompressed(filepath.Join(originRoot, "manifests", hash+manifestExtension), data))
	writeCheckpoint(t, originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{
			origin + "~sess-1": hash,
		},
	})

	res, err := ImportDetailed(ctx, importDB, root, "desktop-d4e5f6")
	require.NoError(t, err)
	assert.False(t, res.Changed())
	state, err := importDB.GetSyncState(importStateKey(origin, origin+"~sess-1"))
	require.NoError(t, err)
	assert.Empty(t, state)
}

func TestImportDefersFutureVersionSegment(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	importDB := testDB(t)
	originRoot := filepath.Join(root, origin)
	segmentData, err := canonicalJSON(segmentMessage{
		Version:       formatVersion + 1,
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
	})
	require.NoError(t, err)
	segmentHash := hashHex(segmentData)
	require.NoError(t, writeCompressed(filepath.Join(originRoot, "segments", segmentHash+segmentExtension), segmentData))
	m := manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: db.Session{
			ID:      "sess-1",
			Machine: origin,
		},
		Segments: []string{segmentHash},
	}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	require.NoError(t, writeCompressed(filepath.Join(originRoot, "manifests", manifestHash+manifestExtension), manifestData))
	writeCheckpoint(t, originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{
			origin + "~sess-1": manifestHash,
		},
	})

	res, err := ImportDetailed(ctx, importDB, root, "desktop-d4e5f6")
	require.NoError(t, err)
	assert.False(t, res.Changed())
	state, err := importDB.GetSyncState(importStateKey(origin, origin+"~sess-1"))
	require.NoError(t, err)
	assert.Empty(t, state)
}

func TestExportScrubsUnstableArtifactIDs(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	require.NoError(t, database.ReplaceSessionUsageEvents("sess-1", []db.UsageEvent{
		{
			SessionID:   "sess-1",
			Source:      "fixture",
			Model:       "claude-test",
			InputTokens: 1,
			OccurredAt:  "2026-06-14T01:02:04Z",
			DedupKey:    "usage-1",
		},
	}))

	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	cp, err := readLatestCheckpoint(filepath.Join(root, origin))
	require.NoError(t, err)
	require.NotNil(t, cp)
	manifestHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, manifestHash)

	m, err := readManifest(filepath.Join(root, origin), manifestHash)
	require.NoError(t, err)
	require.Len(t, m.UsageEvents, 1)
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	assert.NotContains(t, string(manifestData), `"ID"`)
	assert.NotContains(t, string(manifestData), `"SessionID"`)

	msgs, err := readManifestMessages(filepath.Join(root, origin), m)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	for _, msg := range msgs {
		assert.Zero(t, msg.ID)
		assert.Empty(t, msg.SessionID)
	}
}

func TestExportRejectsInvalidOriginBeforeCreatingPaths(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "artifacts")
	seedSession(t, database, "sess-1", "alpha")

	count, err := Export(ctx, database, root, "../outside")
	require.Error(t, err)
	assert.Zero(t, count)
	assert.Contains(t, err.Error(), "invalid artifact origin")

	_, statErr := os.Stat(filepath.Join(parent, "outside"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestExportSkipsForeignSessions(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	dataDir := t.TempDir()
	share := t.TempDir()
	seedSession(t, database, "other~sess-1", "alpha", func(s *db.Session) {
		s.Machine = "other"
	})
	seedSession(t, database, "legacy-remote~sess-2", "bravo", func(s *db.Session) {
		s.Machine = "remote"
	})

	res, err := SyncFolder(ctx, database, SyncOptions{DataDir: dataDir, Target: share})
	require.NoError(t, err)
	assert.Zero(t, res.ExportedSessions)

	origin := res.Origin
	manifests := globArtifacts(t, filepath.Join(dataDir, "artifacts"), origin, "manifests", "*"+manifestExtension)
	assert.Empty(t, manifests)
}

func TestCopyUnionRejectsPathConflicts(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	rel := filepath.Join("origin", "checkpoints", "cp-0000000001.json")
	require.NoError(t, os.MkdirAll(filepath.Join(src, filepath.Dir(rel)), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dst, filepath.Dir(rel)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, rel), []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dst, rel), []byte("bravo"), 0o644))

	err := CopyUnion(src, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact path conflict")
}

func TestCopyUnionDetectsCheckpointSequenceConflict(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	rel := filepath.Join("origin", "checkpoints", "cp-0000000001.json")
	require.NoError(t, os.MkdirAll(filepath.Join(src, filepath.Dir(rel)), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dst, filepath.Dir(rel)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, rel), []byte("{\"seq\":1,\"sessions\":{\"a\":\"1\"}}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dst, rel), []byte("{\"seq\":1,\"sessions\":{\"b\":\"2\"}}\n"), 0o644))

	err := CopyUnion(src, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact path conflict")
}

func TestWriteFileAtomicIsWriteOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "origin", "segments", "artifact.ndjson.zst")
	first := []byte("first")
	second := []byte("second")

	require.NoError(t, writeFileAtomic(path, first, 0o644))
	require.NoError(t, writeFileAtomic(path, first, 0o644))
	err := writeFileAtomic(path, second, 0o644)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact path conflict")
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, first, got)
}

func TestWriteFileAtomicDoesNotReplaceFileCreatedDuringCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "origin", "segments", "artifact.ndjson.zst")
	first := []byte("first")
	second := []byte("second")

	writeFileAtomicBeforeCommit = func(commitPath string) {
		require.Equal(t, path, commitPath)
		require.NoError(t, os.WriteFile(path, first, 0o644))
	}
	t.Cleanup(func() { writeFileAtomicBeforeCommit = nil })

	err := writeFileAtomic(path, second, 0o644)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact path conflict")
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, first, got)
}

func TestWriteFileAtomicFallsBackWhenHardLinksUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "origin", "segments", "artifact.ndjson.zst")
	data := []byte("payload")

	origLink := writeFileAtomicLink
	writeFileAtomicLink = func(_, _ string) error {
		return &os.LinkError{Op: "link", Err: syscall.ENOTSUP}
	}
	t.Cleanup(func() { writeFileAtomicLink = origLink })

	require.NoError(t, writeFileAtomic(path, data, 0o644))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestCopyUnionRejectsNonRegularSources(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o644))
	link := filepath.Join(src, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	err := CopyUnion(src, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}

func TestCopyUnionRejectsNonRegularDestinations(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "artifact.txt"), []byte("secret"), 0o644))
	target := filepath.Join(t.TempDir(), "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o644))
	link := filepath.Join(dst, "artifact.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	err := CopyUnion(src, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}

func TestCopyUnionSkipsAtomicTempFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	rel := filepath.Join("origin", "segments", "artifact.ndjson.zst")
	tmpRel := filepath.Join("origin", "segments", tempFilePrefix+"leftover")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "origin", "segments"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, rel), []byte("artifact"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, tmpRel), []byte("partial"), 0o644))

	require.NoError(t, CopyUnion(src, dst))

	got, err := os.ReadFile(filepath.Join(dst, rel))
	require.NoError(t, err)
	assert.Equal(t, []byte("artifact"), got)
	_, err = os.Stat(filepath.Join(dst, tmpRel))
	assert.True(t, os.IsNotExist(err), "temp artifact should not be copied")
}

func TestCopyUnionRepeatedExchangeLeavesExistingArtifactsAndCopiesMissing(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	existingRel := filepath.Join("origin", "manifests", "existing.json.zst")
	missingRel := filepath.Join("origin", "checkpoints", "cp-0000000002.json")
	require.NoError(t, os.MkdirAll(filepath.Join(src, filepath.Dir(existingRel)), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(src, filepath.Dir(missingRel)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, existingRel), []byte("manifest"), 0o644))

	require.NoError(t, CopyUnion(src, dst))
	dstExisting := filepath.Join(dst, existingRel)
	pinnedMtime := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(dstExisting, pinnedMtime, pinnedMtime))
	require.NoError(t, os.WriteFile(filepath.Join(src, missingRel), []byte("checkpoint"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "origin", tempFilePrefix+"leftover"), []byte("partial"), 0o644))

	require.NoError(t, CopyUnion(src, dst))

	info, err := os.Stat(dstExisting)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(pinnedMtime), "existing artifact should not be rewritten")
	got, err := os.ReadFile(filepath.Join(dst, missingRel))
	require.NoError(t, err)
	assert.Equal(t, []byte("checkpoint"), got)
	_, err = os.Stat(filepath.Join(dst, "origin", tempFilePrefix+"leftover"))
	assert.True(t, os.IsNotExist(err), "stale temp file should not be copied while resuming")
}

func globArtifacts(t *testing.T, root, origin, kind, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, origin, kind, pattern))
	require.NoError(t, err)
	return paths
}

func writeCheckpoint(t *testing.T, originRoot string, cp checkpoint) {
	t.Helper()
	data, err := canonicalJSON(cp)
	require.NoError(t, err)
	path := filepath.Join(originRoot, "checkpoints", fmt.Sprintf("cp-%010d.json", cp.Sequence))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
	}
	require.NoError(t, writeFileAtomic(path, data, 0o644))
}

func writePreNormalizationManifest(
	t *testing.T,
	ctx context.Context,
	database *db.DB,
	originRoot, origin, sessionID string,
) string {
	t.Helper()
	sess, err := database.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := database.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	usageEvents, err := database.GetUsageEvents(ctx, sessionID)
	require.NoError(t, err)
	segmentData, err := encodeSegment(canonicalMessages(msgs))
	require.NoError(t, err)
	segmentHash := hashHex(segmentData)
	require.NoError(t, writeCompressed(
		filepath.Join(originRoot, "segments", segmentHash+segmentExtension),
		segmentData,
	))

	sess.Machine = origin
	require.NotZero(t, sess.SecretLeakCount, "precondition: old manifest carries source scan count")
	require.NotNil(t, sess.LocalModifiedAt, "precondition: old manifest carries local watermark")
	m := manifest{
		Version:               formatVersion,
		Origin:                origin,
		NativeSessionID:       sessionID,
		Session:               *sess,
		SessionName:           sess.SessionName,
		Segments:              []string{segmentHash},
		UsageEvents:           canonicalUsageEvents(usageEvents),
		DataVersion:           sess.DataVersion,
		Generation:            1,
		SessionHasToolCalls:   sess.HasToolCalls,
		SessionHasContextData: sess.HasContextData,
		SessionQualitySignals: sess.StoredQualitySignals(),
	}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	require.NoError(t, writeCompressed(
		filepath.Join(originRoot, "manifests", manifestHash+manifestExtension),
		manifestData,
	))
	return manifestHash
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	return database
}

func seedSession(t *testing.T, database *db.DB, id, project string, opts ...func(*db.Session)) {
	t.Helper()
	sess := db.Session{
		ID:               id,
		Project:          project,
		Machine:          "local",
		Agent:            "claude",
		MessageCount:     2,
		UserMessageCount: 1,
		FirstMessage:     new("hello"),
		StartedAt:        new("2026-06-14T01:02:03Z"),
		EndedAt:          new("2026-06-14T01:03:03Z"),
		SessionName:      new("Test Session"),
		CreatedAt:        "2026-06-14T01:02:03Z",
	}
	for _, opt := range opts {
		opt(&sess)
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
}
