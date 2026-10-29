package db_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestLegacyUpgradePreservesIncompleteRetry(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	body := testjsonl.CodexForkedSessionMetaJSON(id, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", root, "codex_cli_rs", "2026-10-08T12:00:00Z") + "\n" + testjsonl.CodexMsgJSON("user", "child without parent turns", "2026-10-08T12:00:01Z") + "\n"
	dbtest.WriteTestFile(t, filepath.Join(root, "rollout-"+id+".jsonl"), []byte(body))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "PRAGMA user_version=129")
		return err
	}))
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	require.Equal(t, db.CurrentDataVersion()-1, database.GetSessionDataVersion(t.Context(), "codex:"+id), "missing parent output must remain retryable")
	// Reproduce the retry stamp written by the legacy parser before reopening.
	require.NoError(t, database.SetSessionDataVersion(t.Context(), "codex:"+id, 129))
	path := database.Path()
	require.NoError(t, database.Close())
	reopened, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, reopened.NeedsResync())
	assert.Equal(t, 129, reopened.GetSessionDataVersion(t.Context(), "codex:"+id))
	require.NoError(t, reopened.Close())
}

func TestSimulatedScopedUpgradeReparsesUnchangedFreebuffSource(t *testing.T) {
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: {parser.AgentCodebuff}}))
	database := dbtest.OpenTestDB(t)
	root, unaffected := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "project", "chats", "2026-09-15T10-00-00.000Z", "chat-messages.json")
	dbtest.WriteTestFile(t, source, []byte(`[{"id":"user-1","variant":"user","content":"hello","timestamp":"10:00 AM"}]`))
	dbtest.WriteTestFile(t, filepath.Join(filepath.Dir(source), "run-state.json"), []byte(`{"sessionState":{"mainAgentState":{"agentType":"base2-free-minimax-m3"}}}`))
	dbtest.WriteTestFile(t, filepath.Join(unaffected, "project", "unchanged.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-10-08T12:00:00Z", "unaffected").String()))
	cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodebuff: {root}, parser.AgentClaude: {unaffected}}}
	engine := sync.NewEngine(t.Context(), database, cfg)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	var id string
	require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT id FROM sessions WHERE agent='freebuff'").Scan(&id))
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	messages[0].Content = "obsolete parser output"
	messages[0].ContentLength = len(messages[0].Content)
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), id, messages))
	_, found, err := database.GetProviderStatHash(t.Context(), parser.AgentCodebuff, source)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("UPDATE sessions SET data_version=%d; PRAGMA user_version=%d", version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err = db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	needsSync, _, err := db.ArchiveResyncState(t.Context(), path)
	require.NoError(t, err)
	require.True(t, needsSync)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	cfg.AgentDirs[parser.AgentCodebuff] = []string{root, unaffected}
	dbtest.WriteTestFile(t, filepath.Join(unaffected, "project", "chats", "broken", "chat-messages.json"), []byte("{"))
	engine = sync.NewEngine(t.Context(), database, cfg)
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, stats.Failed)
	needsSync, _, err = db.ArchiveResyncState(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, needsSync)
	assert.Equal(t, source, database.GetSessionFilePath(t.Context(), id))
	messages, err = database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hello", messages[0].Content)
}

func TestClaudeFormatPRLinkUpgradePreservesSessionIdentity(t *testing.T) {
	providers := []parser.AgentType{parser.AgentClaude, parser.AgentCowork, parser.AgentQoder, parser.AgentIcodemate}
	scope := db.ScopedDataVersionProviders(131, 132)
	assert.ElementsMatch(t, append(providers, parser.AgentGrok), scope)
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: scope}))
	for _, provider := range providers {
		t.Run(string(provider), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			sessionID := "c0000000-0000-4000-8000-000000000101"
			source := filepath.Join(root, "project", sessionID+".jsonl")
			if provider == parser.AgentCowork {
				workspace := filepath.Join(root, "org", "workspace")
				localID := "local_50000000-0000-4000-8000-000000000101"
				dbtest.WriteTestFile(t, filepath.Join(workspace, localID+".json"), []byte(`{"sessionId":"`+localID+`","cliSessionId":"`+sessionID+`"}`))
				source = filepath.Join(workspace, localID, ".claude", "projects", "project", sessionID+".jsonl")
			}
			body := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-10-08T12:00:00Z", "hello", sessionID).String() + "\n" + `{"type":"pr-link","prNumber":7,"prUrl":"https://github.com/owner/repo/pull/7","prRepository":"owner/repo"}` + "\n"
			dbtest.WriteTestFile(t, source, []byte(body))
			cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{provider: {root}}}
			engine := sync.NewEngine(t.Context(), database, cfg)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			engine.Close()
			var id string
			require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT id FROM sessions").Scan(&id))
			require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), fmt.Sprintf("UPDATE sessions SET data_version=%d, pr_links=''; PRAGMA user_version=%d", version-1, version-1))
				return err
			}))
			path := database.Path()
			require.NoError(t, database.Close())
			database, err := db.OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			assert.False(t, database.NeedsResync(), "Claude upgrade must reparse in place")
			engine = sync.NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			session, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, source, database.GetSessionFilePath(t.Context(), id))
			require.Len(t, session.PRLinks, 1)
			assert.Equal(t, 7, session.PRLinks[0].Number)
			var count int
			require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&count))
			assert.Equal(t, 1, count)
		})
	}
}

func TestGrokUpgradePreservesParserRelationship(t *testing.T) {
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: db.ScopedDataVersionProviders(131, 132)}))
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	id := "grok:019f6000-0000-7000-8000-000000000011"
	source := filepath.Join(root, "cwd-key", "019f6000-0000-7000-8000-000000000011", "summary.json")
	dbtest.WriteTestFile(t, source, []byte(`{"session_kind":"subagent","session_summary":"Review this change","created_at":"2026-07-18T10:01:00Z","num_messages":1}`))
	cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}}
	engine := sync.NewEngine(t.Context(), database, cfg)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("UPDATE sessions SET data_version=%d, parser_relationship_type=NULL; PRAGMA user_version=%d", version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine = sync.NewEngine(t.Context(), database, cfg)
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Empty(t, session.SessionKind)
	assert.Nil(t, session.ParentSessionID)
	assert.Equal(t, "subagent", session.RelationshipType)
	assert.Equal(t, source, database.GetSessionFilePath(t.Context(), id))
	var relationship string
	require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT parser_relationship_type FROM sessions WHERE id=?", id).Scan(&relationship))
	assert.Equal(t, "subagent", relationship)
}

func TestScopedUpgradeWaitsForAuthoritativeFullPass(t *testing.T) {
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(
		map[int][]parser.AgentType{version: {parser.AgentCodebuff}},
	))
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.False(t, database.NeedsResync(), "a scoped upgrade must not force a rebuild")

	waiting := func() (startup, coldRead bool) {
		t.Helper()
		startup, _, err := db.ArchiveResyncState(t.Context(), path)
		require.NoError(t, err)
		reader, err := db.OpenReadOnly(t.Context(), path)
		require.NoError(t, err)
		defer func() { require.NoError(t, reader.Close()) }()
		return startup, reader.NeedsResync()
	}
	startup, coldRead := waiting()
	assert.True(t, startup)
	assert.True(t, coldRead, "cold reads must wait for affected sources")

	clean := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodebuff: {t.TempDir()},
	}}
	broken := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(broken, "opencode.db"), []byte("not a database"))
	failing := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{
		parser.AgentOpenCode: {broken},
	}}
	passes := []struct {
		name string
		cfg  sync.EngineConfig
		run  func(*sync.Engine) sync.SyncStats
		done bool
	}{
		{
			name: "incremental pass",
			cfg:  clean,
			run: func(e *sync.Engine) sync.SyncStats {
				return e.SyncAllSince(t.Context(), time.Now(), nil)
			},
		},
		{
			name: "full pass with provider failure",
			cfg:  failing,
			run: func(e *sync.Engine) sync.SyncStats {
				return e.SyncAll(t.Context(), nil)
			},
		},
		{
			name: "authoritative full pass",
			cfg:  clean,
			run: func(e *sync.Engine) sync.SyncStats {
				return e.SyncAll(t.Context(), nil)
			},
			done: true,
		},
	}
	for _, pass := range passes {
		engine := sync.NewEngine(t.Context(), database, pass.cfg)
		pass.run(engine)
		engine.Close()
		startup, coldRead := waiting()
		assert.Equal(t, !pass.done, startup, pass.name)
		assert.Equal(t, !pass.done, coldRead, pass.name)
	}
}

func TestCodexGitBranchUpgradeReparsesCodexInPlace(t *testing.T) {
	scope := db.ScopedDataVersionProviders(132, 133)
	assert.ElementsMatch(t,
		[]parser.AgentType{parser.AgentCodex, parser.AgentTraeX, parser.AgentAugureCode}, scope)
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: scope}))
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	codexRoot, claudeRoot := t.TempDir(), t.TempDir()
	codexID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	codexSource := filepath.Join(codexRoot, "rollout-"+codexID+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(
		codexID, codexRoot, "codex_cli_rs", "2026-10-08T12:00:00Z",
		map[string]any{"git": map[string]any{"branch": "feat/rate-limit"}},
	)
	body := meta + "\n" +
		testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z") + "\n"
	dbtest.WriteTestFile(t, codexSource, []byte(body))
	dbtest.WriteTestFile(t, filepath.Join(claudeRoot, "project", "unchanged.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-10-08T12:00:00Z", "unaffected").String()))
	cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodex: {codexRoot}, parser.AgentClaude: {claudeRoot},
	}}
	engine := sync.NewEngine(ctx, database, cfg)
	require.Equal(t, 2, engine.SyncAll(ctx, nil).Synced)
	engine.Close()
	var claudeID string
	require.NoError(t, database.Reader().QueryRow(ctx,
		"SELECT id FROM sessions WHERE agent='claude'").Scan(&claudeID))

	// Reproduce an archive from the version before the Codex branch upgrade.
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, fmt.Sprintf(
			"UPDATE sessions SET git_branch='', data_version=%d; PRAGMA user_version=%d",
			version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err := db.OpenIsolated(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	needsSync, fullRebuild, err := db.ArchiveResyncState(ctx, path)
	require.NoError(t, err)
	assert.True(t, needsSync)
	assert.False(t, fullRebuild, "the Codex branch upgrade must not rebuild the archive")
	assert.Equal(t, version, database.GetSessionDataVersion(ctx, claudeID))
	assert.Equal(t, version-1, database.GetSessionDataVersion(ctx, "codex:"+codexID))

	engine = sync.NewEngine(ctx, database, cfg)
	t.Cleanup(engine.Close)
	assert.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)
	needsSync, _, err = db.ArchiveResyncState(ctx, path)
	require.NoError(t, err)
	assert.False(t, needsSync)
	codex, err := database.GetSessionFull(ctx, "codex:"+codexID)
	require.NoError(t, err)
	require.NotNil(t, codex)
	require.NotNil(t, codex.FilePath)
	assert.Equal(t, codexSource, *codex.FilePath)
	assert.Equal(t, "feat/rate-limit", codex.GitBranch)
	assert.Equal(t, version, codex.DataVersion)
	var sessions int
	require.NoError(t, database.Reader().QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
	assert.Equal(t, 2, sessions, "the reparse must keep the existing session identities")
}

func TestCodexRepositoryUpgradeRepairsExistingIdentity(t *testing.T) {
	assert.ElementsMatch(t,
		[]parser.AgentType{parser.AgentCodex, parser.AgentTraeX, parser.AgentAugureCode},
		db.ScopedDataVersionProviders(137, 138))
	const recorded = "example.com/team/api"
	for _, tc := range []struct {
		name                         string
		resolution                   export.ProjectResolution
		remote, remoteName           string
		wantNormalized               string
		fullRebuild                  bool
		otherUnresolved              bool
		mapped, nested               bool
		otherAssigned, otherRootOnly bool
		additionalAmbiguous          bool
	}{
		{name: "reassigned sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherAssigned: true},
		{name: "full rebuild reassigned sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherAssigned: true, fullRebuild: true},
		{name: "root-only sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherRootOnly: true},
		{name: "full rebuild root-only sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherRootOnly: true, fullRebuild: true},
		{name: "mapped project reassigned sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherAssigned: true, mapped: true},
		{name: "full rebuild nested reassigned sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherAssigned: true, nested: true, fullRebuild: true},
		{name: "nested mixed sibling evidence", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherRootOnly: true, additionalAmbiguous: true, nested: true},
		{name: "full rebuild nested mixed sibling evidence", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherRootOnly: true, additionalAmbiguous: true, nested: true, fullRebuild: true},
		{name: "nested root-only sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherRootOnly: true, nested: true},
		{name: "full rebuild nested root-only sibling", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true, otherRootOnly: true, nested: true, fullRebuild: true},
		{name: "mapped nested checkout", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, mapped: true, nested: true},
		{name: "full rebuild mapped nested checkout", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, fullRebuild: true, mapped: true, nested: true},
		{name: "full rebuild retains mapped nested ambiguity", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, fullRebuild: true, mapped: true, nested: true, otherUnresolved: true},
		{name: "mapped project retains another ambiguous session", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, mapped: true, otherUnresolved: true},
		{name: "scoped removed nested checkout", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, nested: true},
		{name: "scoped removed root-only checkout", resolution: export.ProjectResolutionResolved, wantNormalized: recorded, nested: true},
		{name: "full rebuild removed root-only checkout", resolution: export.ProjectResolutionResolved, wantNormalized: recorded, fullRebuild: true, nested: true},
		{name: "full rebuild retains another nested ambiguity", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, fullRebuild: true, nested: true, otherUnresolved: true},
		{name: "scoped repair retains another nested root", resolution: export.ProjectResolutionResolved, wantNormalized: recorded, nested: true, otherUnresolved: true},
		{name: "full rebuild retains another nested root", resolution: export.ProjectResolutionResolved, wantNormalized: recorded, fullRebuild: true, nested: true, otherUnresolved: true},
		{name: "mapped project", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, mapped: true},
		{name: "full rebuild removed nested checkout", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, fullRebuild: true, nested: true},
		{name: "unknown root only", resolution: export.ProjectResolutionUnknown, wantNormalized: recorded},
		{name: "resolved root only", resolution: export.ProjectResolutionResolved, wantNormalized: recorded},
		{name: "ambiguous", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded},
		{name: "full rebuild root only", resolution: export.ProjectResolutionResolved, wantNormalized: recorded, fullRebuild: true},
		{name: "full rebuild ambiguous", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, fullRebuild: true},
		{name: "another session remains ambiguous", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, otherUnresolved: true},
		{name: "full rebuild retains another ambiguous session", resolution: export.ProjectResolutionAmbiguous, wantNormalized: recorded, fullRebuild: true, otherUnresolved: true},
		{
			name: "full rebuild retains checkout remote", resolution: export.ProjectResolutionResolved,
			remote: "https://example.com/team/other.git", remoteName: "origin",
			wantNormalized: "example.com/team/other", fullRebuild: true,
		},
		{
			name: "resolved checkout remote", resolution: export.ProjectResolutionResolved,
			remote: "https://example.com/team/other.git", remoteName: "origin",
			wantNormalized: "example.com/team/other",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			cwd := filepath.Join(root, "removed-checkout")
			checkout := filepath.Join(root, ".codex", "worktrees", "abcd", "api")
			if tc.nested {
				cwd = filepath.Join(checkout, "internal", "parser")
				require.NoError(t, os.MkdirAll(cwd, 0o755))
				require.NoError(t, os.Mkdir(filepath.Join(checkout, ".git"), 0o755))
			}
			if tc.mapped {
				_, err := database.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
					Machine: "local", PathPrefix: cwd, Project: "mapped-api", Enabled: true,
				})
				require.NoError(t, err)
			}
			id := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
			source := filepath.Join(root, "rollout-"+id+".jsonl")
			meta := testjsonl.CodexSessionMetaWithFieldsJSON(
				id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z",
				map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}},
			)
			dbtest.WriteTestFile(t, source, []byte(meta+"\n"+
				testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z")+"\n"))
			cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}}
			engine := sync.NewEngine(ctx, database, cfg)
			require.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)
			engine.Close()
			before, err := database.GetSessionFull(ctx, "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, before)
			priorSnapshots, err := database.ListSessionProjectIdentitySnapshots(ctx)
			require.NoError(t, err)
			require.Len(t, priorSnapshots, 1)
			oldRoot := priorSnapshots[0].RootPath
			// Older parsers discarded the URL and kept checkout-derived evidence.
			version := 137
			if tc.fullRebuild {
				version = 135
			}
			oldIdentity := export.BuildStoredProjectIdentity(export.ProjectIdentityInput{
				RootPath: oldRoot, GitRemote: tc.remote, GitRemoteName: tc.remoteName,
			})
			if tc.resolution == export.ProjectResolutionAmbiguous {
				oldIdentity = export.StoredProjectIdentity{}
			}
			require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
				for _, table := range []string{"session_project_identity_snapshots", "project_identity_observations"} {
					_, err := tx.ExecContext(ctx, `
					UPDATE `+table+` SET git_remote=?, git_remote_name=?,
					    normalized_remote=?, key=?, key_source=?, remote_resolution=?,
					    remote_candidate_count=0`,
						tc.remote, tc.remoteName, oldIdentity.NormalizedRemote,
						oldIdentity.Key, oldIdentity.KeySource, tc.resolution)
					if err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(ctx, fmt.Sprintf(
					"UPDATE sessions SET data_version=%d; PRAGMA user_version=%d", version, version))
				return err
			}))
			assignedProject := ""
			if tc.otherUnresolved {
				other := priorSnapshots[0]
				other.SessionID = "unavailable-session"
				other.Project = before.Project
				other.GitRemote, other.GitRemoteName = "", ""
				other.RemoteResolution = tc.resolution
				if tc.otherRootOnly {
					other.RemoteResolution = export.ProjectResolutionResolved
					other.WorktreeRelationship = export.WorktreeMain
				}
				other.ObservedAt = time.Now()
				if tc.additionalAmbiguous {
					ambiguous := other
					ambiguous.SessionID = "ambiguous-sibling"
					ambiguous.RemoteResolution = export.ProjectResolutionAmbiguous
					ambiguous.ObservedAt = other.ObservedAt.Add(-time.Hour)
					require.NoError(t, database.UpsertSessionWithProjectIdentity(ctx, db.Session{
						ID: ambiguous.SessionID, Project: before.Project, Machine: "local", Agent: "codex",
					}, ambiguous, priorSnapshots[0].Project))
				}
				require.NoError(t, database.UpsertSessionWithProjectIdentity(ctx, db.Session{
					ID: "unavailable-session", Project: before.Project, Machine: "local", Agent: "codex",
				}, other, priorSnapshots[0].Project))
				if tc.otherAssigned {
					assignment, err := database.AssignSessionProject(ctx, other.SessionID, "separate-project")
					require.NoError(t, err)
					assignedProject = assignment.Project
				}
			}
			require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), "ALTER TABLE session_project_identity_snapshots DROP COLUMN recorded_git_remote")
				return err
			}))
			if tc.nested {
				require.NoError(t, os.RemoveAll(checkout))
			}
			path := database.Path()
			require.NoError(t, database.Close())
			database, err = db.OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			needsSync, fullRebuild, err := db.ArchiveResyncState(ctx, path)
			require.NoError(t, err)
			assert.True(t, needsSync)
			assert.Equal(t, tc.fullRebuild, fullRebuild)
			engine = sync.NewEngine(ctx, database, cfg)
			t.Cleanup(engine.Close)
			stats, err := engine.SyncThenRun(ctx, false, nil, func(bool) error { return nil })
			require.NoError(t, err)
			require.Equal(t, 1, stats.Synced)
			require.Zero(t, stats.Failed)
			require.False(t, stats.Aborted)
			after, err := database.GetSessionFull(ctx, "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, after)
			assert.Equal(t, before.ID, after.ID)
			assert.Equal(t, before.FilePath, after.FilePath)
			assert.Equal(t, before.FileHash, after.FileHash)
			assert.Equal(t, before.SourceSessionID, after.SourceSessionID)
			assert.Equal(t, db.CurrentDataVersion(), after.DataVersion)
			snapshots, err := database.ListSessionProjectIdentitySnapshots(ctx)
			require.NoError(t, err)
			found := false
			for _, snapshot := range snapshots {
				if snapshot.SessionID == after.ID {
					found = true
					assert.Equal(t, tc.wantNormalized, snapshot.NormalizedRemote)
					assert.Equal(t, export.ProjectResolutionResolved, snapshot.RemoteResolution)
				}
			}
			require.True(t, found, "the reparsed session must retain its snapshot")
			projects, err := database.BuildProjectIdentityMap(ctx, []string{after.Project})
			require.NoError(t, err)
			wantResolution := export.ProjectResolutionResolved
			if (tc.otherUnresolved && !tc.otherAssigned && (!tc.otherRootOnly || tc.nested)) || tc.remote != "" {
				wantResolution = export.ProjectResolutionAmbiguous
			}
			assert.Equal(t, wantResolution, projects[after.Project].Resolution)
			if tc.otherAssigned {
				assigned, err := database.BuildProjectIdentityMap(ctx, []string{assignedProject})
				require.NoError(t, err)
				assert.Equal(t, export.ProjectResolutionAmbiguous, assigned[assignedProject].Resolution)
			}
			if tc.otherRootOnly {
				observations, err := database.ListProjectIdentityObservations(ctx, []string{after.Project})
				require.NoError(t, err)
				var fallbacks []export.ProjectIdentityObservation
				for _, observation := range observations {
					if observation.RootPath == oldRoot && observation.GitRemote == "" {
						fallbacks = append(fallbacks, observation)
					}
				}
				if tc.nested {
					require.Len(t, fallbacks, 1, "the sibling still supports the previous root")
					wantFallback := export.ProjectResolutionResolved
					if tc.additionalAmbiguous {
						wantFallback = export.ProjectResolutionAmbiguous
					}
					assert.Equal(t, wantFallback, fallbacks[0].RemoteResolution)
				} else {
					assert.Empty(t, fallbacks, "the recorded remote supersedes same-root fallbacks")
				}
			}
			var sessions int
			require.NoError(t, database.Reader().QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
			wantSessions := 1
			if tc.otherUnresolved {
				wantSessions++
			}
			if tc.additionalAmbiguous {
				wantSessions++
			}
			assert.Len(t, snapshots, wantSessions)
			assert.Equal(t, wantSessions, sessions)
			assert.Zero(t, engine.SyncAll(ctx, nil).Synced, "unchanged sources must stop reparsing after the upgrade")
		})
	}
}

func TestCodexCloudToolTextUpgradeReparsesInPlace(t *testing.T) {
	scope := db.ScopedDataVersionProviders(151, 152)
	assert.Equal(t, []parser.AgentType{parser.AgentCodexCloud}, scope)
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: scope}))
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	source := filepath.Join(root, "account-key", "thread-a.jsonl.zstd")
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := mirror.Append(source, 0, []mirror.Record{
		{Kind: mirror.KindThread, FetchedAt: fetched, Data: jsontext.Value(`{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`)},
		{Kind: mirror.KindItem, FetchedAt: fetched, Data: jsontext.Value(`{"turnId":"turn-1","startedAtMs":1759320000000,"item":` +
			`{"type":"commandExecution","id":"c1","command":"make","cwd":"/workspace/project-a","status":"completed"}}`)},
	})
	require.NoError(t, err)
	cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}}}
	engine := sync.NewEngine(ctx, database, cfg)
	require.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)
	engine.Close()
	const id = "codex-cloud:thread-a"
	messages, err := database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	// Reproduce the previous parser, which wrote the call as plain prose.
	messages[0].Content = "$ make"
	messages[0].ContentLength = len(messages[0].Content)
	require.NoError(t, database.ReplaceSessionMessages(ctx, id, messages))
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE sessions SET data_version=%d; PRAGMA user_version=%d", version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err = db.OpenIsolated(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, fullRebuild, err := db.ArchiveResyncState(ctx, path)
	require.NoError(t, err)
	assert.False(t, fullRebuild, "the Codex cloud upgrade must reparse in place")

	engine = sync.NewEngine(ctx, database, cfg)
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)
	messages, err = database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Equal(t, "[Bash]\n$ make", messages[0].Content)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, source, database.GetSessionFilePath(ctx, id))
	var sessions int
	require.NoError(t, database.Reader().QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
	assert.Equal(t, 1, sessions)
}
