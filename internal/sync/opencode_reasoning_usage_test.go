package sync_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestOpenCodeFamilyReasoningTokensArePricedOnce syncs OpenCode-family
// stores whose assistant messages report one call with 1000 input tokens
// (200 cached) and 300 output tokens, 120 of them reasoning. One message
// uses the current writers' convention (output 180 plus reasoning 120), the
// other the convention of builds before the change (output 300 already
// holding the reasoning). Both must archive output 300 with reasoning as its
// subset and cost the same: reasoning billed once at the output rate.
func TestOpenCodeFamilyReasoningTokensArePricedOnce(t *testing.T) {
	const model = "synthetic-reasoning-model"
	messages := []struct{ id, tokens string }{
		{"msg_a1", `{"total":1300,"input":800,"output":180,"reasoning":120,"cache":{"read":200,"write":0}}`},
		{"msg_a2", `{"total":1300,"input":800,"output":300,"reasoning":120,"cache":{"read":200,"write":0}}`},
	}
	for _, tc := range []struct {
		agent  parser.AgentType
		dbName string
	}{
		{parser.AgentOpenCode, "opencode.db"},
		{parser.AgentKilo, "kilo.db"},
		{parser.AgentMiMoCode, "mimocode.db"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			oc := createOpenCodeLikeDB(t, filepath.Join(root, tc.dbName), string(tc.agent))
			oc.addProject(t, "prj_1", "/synthetic/project")
			oc.addSession(t, "ses_r", "prj_1", 1767000000000, 1767000060000)
			oc.mustExec(t, "insert user message",
				`INSERT INTO message (id, session_id, data, time_created, time_updated)
				 VALUES ('msg_u', 'ses_r', '{"role":"user"}', 1767000000000, 1767000000000)`)
			oc.mustExec(t, "insert user part",
				`INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
				 VALUES ('prt_u', 'ses_r', 'msg_u', '{"type":"text","text":"Explain the plan"}',
				 1767000000000, 1767000000000)`)
			for i, m := range messages {
				created := int64(1767000010000 + i*10000)
				oc.mustExec(t, "insert assistant message",
					`INSERT INTO message (id, session_id, data, time_created, time_updated)
					 VALUES (?, 'ses_r', ?, ?, ?)`,
					m.id, `{"role":"assistant","modelID":"`+model+`","providerID":"openai","tokens":`+m.tokens+`}`,
					created, created)
				oc.mustExec(t, "insert assistant part",
					`INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
					 VALUES (?, 'ses_r', ?, '{"type":"text","text":"Answer"}', ?, ?)`,
					"prt_"+m.id, m.id, created, created)
			}

			database, err := db.OpenIsolated(ctx, filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, database.Close()) }()
			require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
				ModelPattern:     model,
				InputPerMTok:     money.MustParseDollars("1"),
				OutputPerMTok:    money.MustParseDollars("10"),
				CacheReadPerMTok: money.MustParseDollars("0.1"),
			}}))
			engine := sync.NewEngine(ctx, database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{tc.agent: {root}},
				Machine:   "local",
			})
			defer engine.Close()
			require.Zero(t, engine.SyncAll(ctx, nil).Failed)

			def, ok := parser.AgentByType(tc.agent)
			require.True(t, ok)
			sessionID := def.IDPrefix + "ses_r"
			stored, err := database.GetAllMessages(ctx, sessionID)
			require.NoError(t, err)
			require.Len(t, stored, 3)
			for _, m := range stored[1:] {
				assert.JSONEq(t,
					`{"input_tokens":800,"output_tokens":300,"reasoning_tokens":120,"cache_read_input_tokens":200,"cache_creation_input_tokens":0}`,
					string(m.TokenUsage))
				assert.Equal(t, 300, m.OutputTokens)
			}

			usage, err := database.GetSessionUsage(ctx, sessionID, false)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Equal(t, 600, usage.TotalOutputTokens)
			// Per message: 800*$1 + 300*$10 + 200*$0.1 per million = $0.00382.
			assert.Equal(t, money.MustParseDollars("0.00764"), usage.Cost)
		})
	}
}

// Archives parsed before version 179 stored the current writers' output
// without reasoning. The scoped upgrade reparses unchanged OpenCode-family
// sources in place so output counts reasoning once.
func TestOpenCodeReasoningUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		sessionID       = "opencode:ses_up"
		previousVersion = 178
	)
	ctx := t.Context()
	root := t.TempDir()
	oc := createOpenCodeDB(t, root)
	oc.addProject(t, "prj_1", "/synthetic/project")
	oc.addSession(t, "ses_up", "prj_1", 1767000000000, 1767000060000)
	oc.mustExec(t, "insert user message",
		`INSERT INTO message (id, session_id, data, time_created, time_updated)
		 VALUES ('msg_u', 'ses_up', '{"role":"user"}', 1767000000000, 1767000000000)`)
	oc.mustExec(t, "insert user part",
		`INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
		 VALUES ('prt_u', 'ses_up', 'msg_u', '{"type":"text","text":"Explain the plan"}',
		 1767000000000, 1767000000000)`)
	oc.mustExec(t, "insert assistant message",
		`INSERT INTO message (id, session_id, data, time_created, time_updated)
		 VALUES ('msg_a', 'ses_up', ?, 1767000010000, 1767000010000)`,
		`{"role":"assistant","modelID":"gpt-5.4","providerID":"openai",`+
			`"tokens":{"total":1300,"input":800,"output":180,"reasoning":120,"cache":{"read":200,"write":0}}}`)
	oc.mustExec(t, "insert assistant part",
		`INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
		 VALUES ('prt_a', 'ses_up', 'msg_a', '{"type":"text","text":"Answer"}', 1767000010000, 1767000010000)`)

	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(ctx, archivePath)
	require.NoError(t, err)
	config := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {root}},
		Machine:   "local",
	}
	engine := sync.NewEngine(ctx, database, config)
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	before, err := database.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	hashBefore, ok := database.GetSessionFileHash(ctx, sessionID)
	require.True(t, ok)
	engine.Close()
	require.NoError(t, database.Close())

	// Model the archive version 178 wrote, without touching the source.
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE messages
		SET token_usage = '{"cache_creation_input_tokens":0,"cache_read_input_tokens":200,"input_tokens":800,"output_tokens":180}',
		    output_tokens = 180
		WHERE session_id = ? AND role = 'assistant'`, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE sessions SET data_version = ? WHERE id = ?", previousVersion, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "PRAGMA user_version = 178")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	assert.ElementsMatch(t,
		[]parser.AgentType{parser.AgentOpenCode, parser.AgentKilo, parser.AgentMiMoCode, parser.AgentIcodemate},
		db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
		"every provider sharing the OpenCode parser reparses in place")
	reopened, err := db.Open(ctx, archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	upgraded := sync.NewEngine(ctx, reopened, config)
	defer upgraded.Close()
	require.Equal(t, 1, upgraded.SyncAll(ctx, nil).Synced,
		"the version bump must bypass freshness checks on unchanged sources")

	after, err := reopened.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.FilePath, after.FilePath)
	hashAfter, ok := reopened.GetSessionFileHash(ctx, sessionID)
	require.True(t, ok)
	assert.Equal(t, hashBefore, hashAfter)
	messages, err := reopened.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, 300, messages[1].OutputTokens)
	assert.JSONEq(t,
		`{"input_tokens":800,"output_tokens":300,"reasoning_tokens":120,"cache_read_input_tokens":200,"cache_creation_input_tokens":0}`,
		string(messages[1].TokenUsage))
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(ctx, sessionID))
	page, err := reopened.ListSessions(ctx, db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 1, "the upgrade must not create a duplicate session")
}
