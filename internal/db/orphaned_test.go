package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecWithoutCancelDropsTempTableWithCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open sqlite")
	defer pool.Close()

	baseCtx := context.Background()
	conn, err := pool.Conn(baseCtx)
	require.NoError(t, err, "pin sqlite connection")
	defer conn.Close()

	_, err = conn.ExecContext(baseCtx, `
		CREATE TEMP TABLE _test_cleanup (
			id TEXT PRIMARY KEY
		)`)
	require.NoError(t, err, "create temp table")

	ctx, cancel := context.WithCancel(baseCtx)
	cancel()

	_, err = execWithoutCancel(ctx, conn,
		"DROP TABLE IF EXISTS _test_cleanup")
	require.NoError(t, err, "drop with canceled context")

	_, err = conn.ExecContext(baseCtx, `
		CREATE TEMP TABLE _test_cleanup (
			id TEXT PRIMARY KEY
		)`)
	require.NoError(t, err, "recreate temp table after cleanup")
}

func TestCopyOrphanedDataRedactsCodexEncryptedPayloads(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")

	codexContent := "[Task: spawn_agent]\n" + fernet
	codexInput := `{"task_name":"multiply","message":"` + fernet + `"}`
	codexPreview := fernet
	multiTokenPreview := fernet + " " + fernet
	truncatedPreview := fernet[:60] + "..."
	// The parser's truncate() appends "..." when a preview is clipped at
	// 300 runes, so a token cut mid-way ends with an ellipsis.
	ellipsisPreview := "clipped " + fernet[:60] + "..."
	// Truncation can land on the first base64 padding byte; the partial
	// padding must not hide the clipped token from the scrub.
	paddedTailPreview := "clipped " + fernet[:len(fernet)-1] + "..."
	// A token starting near the 300-rune boundary leaves a short tail;
	// only the exact truncated length (300 runes plus the ellipsis)
	// marks it as clipped ciphertext.
	shortTailPreview := strings.Repeat("p", 290) + fernet[:10] + "..."
	// A short Fernet-looking suffix in a preview the parser did not
	// truncate is literal plaintext and must survive, ellipsis or not.
	lookalikePreview := "mentions gAAAAAabc"
	lookalikeEllipsisPreview := "trailing lookalike gAAAAAabc..."
	literalTokenPreview := "quoted " + fernet
	// Old builds truncated tool previews inside message content at 220
	// chars, so copied content can end in a clipped token.
	clippedContent := "[Task: spawn_agent]\n" +
		"gAAAAA" + strings.Repeat("B", 214) + "..."
	// A long argument prefix can leave fewer than 40 encrypted chars on
	// the 220-rune preview line; the exact truncated length still marks
	// it as clipped ciphertext.
	shortTailContent := "[Task: send_message]\n" +
		strings.Repeat("j", 200) + "gAAAAA" +
		strings.Repeat("C", 14) + "..."
	// The same short suffix on a line the old builds did not truncate is
	// plaintext and must survive.
	lookalikeContent := "[Task: send_message]\nmentions gAAAAAabc..."
	// A long Fernet-looking suffix without the parser's truncation ellipsis is
	// also literal plaintext and must survive.
	longLookalikeContent := "[Task: send_message]\nliteral gAAAAA" +
		strings.Repeat("D", 48)
	// The minimal clipped tail is shorter than its replacement, so
	// content_length must grow to stay consistent with stored bytes.
	minimalTailContent := "[Task: send_message]\n" +
		strings.Repeat("j", 214) + "gAAAAA..."
	// codexSubagent marks a session as an encrypted-capable subagent
	// child; the preview scrub is scoped to these.
	codexSubagent := func(preview *string) func(*Session) {
		return func(s *Session) {
			s.Agent = "codex"
			s.RelationshipType = "subagent"
			s.FirstMessage = preview
		}
	}
	// toolMsg builds a formatted collab tool-call transcript row; the
	// content scrub is scoped to rows carrying a collab tool call.
	toolMsg := func(sid string, ordinal int, content string) Message {
		m := userMsg(sid, ordinal, content)
		m.Role = "assistant"
		m.HasToolUse = true
		m.ToolCalls = []ToolCall{{
			SessionID: sid,
			ToolName:  "spawn_agent",
			Category:  "Task",
			ToolUseID: sid + "-call",
		}}
		return m
	}
	insertSession(t, srcDB, "enc-orphan", "proj",
		codexSubagent(&codexPreview))
	insertSession(t, srcDB, "enc-orphan-multi-preview", "proj",
		codexSubagent(&multiTokenPreview))
	insertSession(t, srcDB, "enc-orphan-clipped", "proj",
		codexSubagent(&truncatedPreview))
	insertSession(t, srcDB, "enc-orphan-ellipsis", "proj",
		codexSubagent(&ellipsisPreview))
	insertSession(t, srcDB, "enc-orphan-padded-tail", "proj",
		codexSubagent(&paddedTailPreview))
	insertSession(t, srcDB, "enc-orphan-short-tail", "proj",
		codexSubagent(&shortTailPreview))
	insertSession(t, srcDB, "enc-orphan-lookalike", "proj",
		codexSubagent(&lookalikePreview))
	insertSession(t, srcDB, "enc-orphan-lookalike-ellipsis", "proj",
		codexSubagent(&lookalikeEllipsisPreview))
	insertSession(t, srcDB, "enc-orphan-literal-token", "proj",
		codexSubagent(&literalTokenPreview))
	insertSession(t, srcDB, "enc-orphan-clipped-content", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB,
		toolMsg("enc-orphan-clipped-content", 0, clippedContent))
	insertSession(t, srcDB, "enc-orphan-short-content", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB,
		toolMsg("enc-orphan-short-content", 0, shortTailContent))
	insertSession(t, srcDB, "enc-orphan-lookalike-content", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB,
		toolMsg("enc-orphan-lookalike-content", 0, lookalikeContent))
	insertSession(t, srcDB, "enc-orphan-long-lookalike-content", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB,
		toolMsg("enc-orphan-long-lookalike-content", 0, longLookalikeContent))
	insertSession(t, srcDB, "enc-orphan-minimal-tail", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB,
		toolMsg("enc-orphan-minimal-tail", 0, minimalTailContent))
	// A user-authored codex message quoting a valid token is content,
	// not a parser artifact, and must survive the copy scrub.
	insertSession(t, srcDB, "enc-orphan-user-quote", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB,
		userMsg("enc-orphan-user-quote", 0, "look at this: "+fernet))
	// Builds before dataVersion 63 stored an encrypted inbound agent task as
	// a user turn. A subagent user turn is parser-owned inter-agent content,
	// unlike the root user quote above, so the preservation copy must scrub it.
	insertSession(t, srcDB, "enc-orphan-child-user", "proj",
		codexSubagent(nil))
	insertMessages(t, srcDB,
		userMsg("enc-orphan-child-user", 0, fernet))
	// A subagent can still receive user-authored literal text. Matching a
	// Fernet token somewhere in that text is not enough to classify the whole
	// turn as an encrypted delivery.
	literalChildContent := "Inspect this token: " + fernet
	insertSession(t, srcDB, "enc-orphan-child-literal", "proj",
		codexSubagent(nil))
	insertMessages(t, srcDB,
		userMsg("enc-orphan-child-literal", 0, literalChildContent))
	_, err := srcDB.getWriter().ExecContext(ctx, `
		UPDATE sessions
		SET quality_signal_version = ?, short_prompt_count = 3,
		    health_score = 17, health_grade = 'D',
		    unstructured_start = 1, missing_success_criteria_count = 4,
		    missing_verification_count = 5, duplicate_prompt_count = 6,
		    no_code_context_count = 7, runaway_tool_loop_count = 8,
		    secret_leak_count = 1, secrets_rules_version = 'stale-rules',
		    transcript_revision = '7'
		WHERE id = ?`, CurrentQualitySignalVersion, "enc-orphan-child-user")
	require.NoError(t, err, "seed stale derived data")
	_, err = srcDB.getWriter().ExecContext(ctx, `
		UPDATE sessions
		SET quality_signal_version = ?, short_prompt_count = 9,
		    health_score = 91, health_grade = 'A',
		    secret_leak_count = 1, secrets_rules_version = 'current-rules',
		    transcript_revision = '11'
		WHERE id = ?`, CurrentQualitySignalVersion, "enc-orphan-child-literal")
	require.NoError(t, err, "seed preserved literal derived data")
	_, err = srcDB.getWriter().ExecContext(ctx, `
		INSERT INTO secret_findings (
			session_id, rule_name, confidence, location_kind,
			message_ordinal, match_start, match_end, match_index,
			redacted_match, rules_version
		) VALUES (?, 'test-secret', 'definite', 'message', 0, 0, 8, 0,
		          '[redacted]', 'stale-rules')`, "enc-orphan-child-user")
	require.NoError(t, err, "seed stale secret finding")
	_, err = srcDB.getWriter().ExecContext(ctx, `
		INSERT INTO secret_findings (
			session_id, rule_name, confidence, location_kind,
			message_ordinal, match_start, match_end, match_index,
			redacted_match, rules_version
		) VALUES (?, 'literal-secret', 'definite', 'message', 0, 0, 8, 0,
		          '[redacted]', 'current-rules')`, "enc-orphan-child-literal")
	require.NoError(t, err, "seed preserved literal secret finding")
	// A root codex session's preview is the user's own prompt; a quoted
	// token there must survive because only subagent previews are
	// parser-derived task deliveries.
	rootPreview := "debugging " + fernet
	insertSession(t, srcDB, "enc-orphan-root-preview", "proj",
		func(s *Session) {
			s.Agent = "codex"
			s.FirstMessage = &rootPreview
		})
	insertMessages(t, srcDB, toolMsg("enc-orphan", 0, codexContent))
	_, err = srcDB.getWriter().ExecContext(ctx,
		`UPDATE messages SET has_tool_use = 0 WHERE session_id = ?`,
		"enc-orphan",
	)
	require.NoError(t, err, "plant stale codex tool-use flag")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`UPDATE tool_calls SET input_json = ? WHERE session_id = ?`,
		codexInput, "enc-orphan")
	require.NoError(t, err, "plant encrypted codex tool input")

	// A non-collab tool legitimately carrying a valid Fernet token in
	// its arguments or transcript line is content and must survive.
	shellContent := "[Bash: decrypt]\n$ decrypt " + fernet
	shellMsg := userMsg("enc-orphan-shell", 0, shellContent)
	shellMsg.Role = "assistant"
	shellMsg.HasToolUse = true
	shellMsg.ToolCalls = []ToolCall{{
		SessionID: "enc-orphan-shell",
		ToolName:  "shell",
		Category:  "Bash",
		ToolUseID: "shell-call",
		InputJSON: `{"command":"decrypt ` + fernet + `"}`,
	}}
	insertSession(t, srcDB, "enc-orphan-shell", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB, shellMsg)

	// A non-Codex session with the same token must be left alone: only
	// Codex ingest redacts these payloads, so the copy pass mirrors that
	// scope.
	insertSession(t, srcDB, "other-orphan", "proj",
		func(s *Session) { s.Agent = "hermes" })
	insertMessages(t, srcDB, userMsg("other-orphan", 0, fernet))

	// Ciphertext only exists in archives written before
	// redactedCodexSourceDataVersion; newer sources already redacted at
	// ingest.
	_, err = srcDB.getWriter().ExecContext(ctx, fmt.Sprintf(
		"PRAGMA user_version = %d", redactedCodexSourceDataVersion-1,
	))
	require.NoError(t, err, "downgrade source data version")
	require.NoError(t, srcDB.Close(), "close source")

	dstPath := filepath.Join(dir, "new.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 20, count, "expected twenty orphans")
	for _, sessionID := range []string{
		"enc-orphan-child-user", "enc-orphan-child-literal",
	} {
		var dataVersion int
		require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
			`SELECT data_version FROM sessions WHERE id = ?`, sessionID,
		).Scan(&dataVersion))
		assert.Less(t, dataVersion, redactedCodexSourceDataVersion,
			"copy-time payload repair must not claim parser migrations for %s",
			sessionID)
	}
	unverified, err := dstDB.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.NotContains(t, unverified, "enc-orphan-child-user",
		"the repaired transcript revision must be certified")
	assert.NotContains(t, unverified, "enc-orphan-child-literal",
		"the verified literal transcript revision must be certified")

	var gotMultiPreview string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-multi-preview",
	).Scan(&gotMultiPreview), "query copied multi-token codex preview")
	assert.Equal(t, "[encrypted] [encrypted]", gotMultiPreview)

	var gotPaddedTail string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-padded-tail",
	).Scan(&gotPaddedTail), "query copied padded-tail codex preview")
	assert.Equal(t, "clipped [encrypted]", gotPaddedTail,
		"a token clipped at its padding byte must still be scrubbed")

	var gotShellContent, gotShellInput string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT m.content, tc.input_json
		 FROM messages m JOIN tool_calls tc ON tc.message_id = m.id
		 WHERE m.session_id = ? AND m.ordinal = 0`,
		"enc-orphan-shell",
	).Scan(&gotShellContent, &gotShellInput),
		"query copied shell codex row")
	assert.Equal(t, "[Bash: decrypt]\n$ decrypt "+fernet, gotShellContent,
		"non-collab tool transcript must survive")
	assert.Equal(t, `{"command":"decrypt `+fernet+`"}`, gotShellInput,
		"non-collab tool input must survive")

	var gotMinimalTail string
	var gotMinimalTailLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-minimal-tail",
	).Scan(&gotMinimalTail, &gotMinimalTailLength),
		"query copied minimal-tail codex content")
	wantMinimalTail := "[Task: send_message]\n" +
		strings.Repeat("j", 214) + "[encrypted]"
	assert.Equal(t, wantMinimalTail, gotMinimalTail)
	assert.Equal(t, len(wantMinimalTail), gotMinimalTailLength,
		"content_length must grow with a longer replacement")

	var gotUserQuote string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-user-quote",
	).Scan(&gotUserQuote), "query copied user-quote codex content")
	assert.Equal(t, "look at this: "+fernet, gotUserQuote,
		"user-authored token quote must survive")

	var gotChildUser string
	var gotChildUserLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-child-user",
	).Scan(&gotChildUser, &gotChildUserLength),
		"query copied encrypted child user turn")
	assert.Equal(t, "[encrypted]", gotChildUser)
	assert.Equal(t, len("[encrypted]"), gotChildUserLength)

	var gotLiteralChild string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-child-literal",
	).Scan(&gotLiteralChild), "query copied literal child user turn")
	assert.Equal(t, literalChildContent, gotLiteralChild,
		"literal subagent text containing a token must survive")
	var literalQualityVersion, literalShortPrompts, literalSecretLeakCount int
	var literalHealthScore sql.NullInt64
	var literalHealthGrade sql.NullString
	var literalRulesVersion, literalTranscriptRevision string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx, `
		SELECT quality_signal_version, short_prompt_count,
		       health_score, health_grade,
		       secret_leak_count, secrets_rules_version, transcript_revision
		FROM sessions WHERE id = ?`, "enc-orphan-child-literal").Scan(
		&literalQualityVersion, &literalShortPrompts,
		&literalHealthScore, &literalHealthGrade,
		&literalSecretLeakCount, &literalRulesVersion,
		&literalTranscriptRevision,
	), "query preserved literal derived data")
	assert.Equal(t, CurrentQualitySignalVersion, literalQualityVersion)
	assert.Equal(t, 9, literalShortPrompts)
	assert.Equal(t, int64(91), literalHealthScore.Int64)
	assert.Equal(t, "A", literalHealthGrade.String)
	assert.Equal(t, 1, literalSecretLeakCount)
	assert.Equal(t, "current-rules", literalRulesVersion)
	assert.Equal(t, "11", literalTranscriptRevision,
		"an unchanged literal transcript must retain its revision")
	var literalFindingCount int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secret_findings WHERE session_id = ?`,
		"enc-orphan-child-literal",
	).Scan(&literalFindingCount), "count preserved literal secret findings")
	assert.Equal(t, 1, literalFindingCount)

	var qualityVersion, shortPrompts, unstructuredStart int
	var missingSuccess, missingVerification, duplicatePrompts int
	var noCodeContext, runawayLoops, secretLeakCount int
	var healthScore sql.NullInt64
	var healthGrade sql.NullString
	var secretsRulesVersion, transcriptRevision string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx, `
		SELECT quality_signal_version, health_score, health_grade,
		       short_prompt_count, unstructured_start,
		       missing_success_criteria_count, missing_verification_count,
		       duplicate_prompt_count, no_code_context_count,
		       runaway_tool_loop_count, secret_leak_count,
		       secrets_rules_version, transcript_revision
		FROM sessions WHERE id = ?`, "enc-orphan-child-user").Scan(
		&qualityVersion, &healthScore, &healthGrade,
		&shortPrompts, &unstructuredStart,
		&missingSuccess, &missingVerification, &duplicatePrompts,
		&noCodeContext, &runawayLoops, &secretLeakCount,
		&secretsRulesVersion, &transcriptRevision,
	), "query invalidated copied derived data")
	assert.Equal(t, []int{0, 0, 0, 0, 0, 0, 0, 0, 0}, []int{
		qualityVersion, shortPrompts, unstructuredStart, missingSuccess,
		missingVerification, duplicatePrompts, noCodeContext, runawayLoops,
		secretLeakCount,
	})
	assert.False(t, healthScore.Valid)
	assert.False(t, healthGrade.Valid)
	assert.Empty(t, secretsRulesVersion)
	assert.Equal(t, "8", transcriptRevision,
		"the repaired transcript must advance exactly once")
	var findingCount int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secret_findings WHERE session_id = ?`,
		"enc-orphan-child-user",
	).Scan(&findingCount), "count invalidated copied secret findings")
	assert.Zero(t, findingCount)

	var gotRootPreview string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-root-preview",
	).Scan(&gotRootPreview), "query copied root codex preview")
	assert.Equal(t, "debugging "+fernet, gotRootPreview,
		"root session preview must survive")

	var gotShortContent string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-short-content",
	).Scan(&gotShortContent), "query copied short-tail codex content")
	assert.Equal(t,
		"[Task: send_message]\n"+strings.Repeat("j", 200)+"[encrypted]",
		gotShortContent)

	var gotLookalikeContent string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-lookalike-content",
	).Scan(&gotLookalikeContent), "query copied lookalike codex content")
	assert.Equal(t,
		"[Task: send_message]\nmentions gAAAAAabc...", gotLookalikeContent)

	var gotLongLookalikeContent string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-long-lookalike-content",
	).Scan(&gotLongLookalikeContent),
		"query copied long lookalike codex content")
	assert.Equal(t, longLookalikeContent, gotLongLookalikeContent,
		"non-truncated Fernet-looking tool text must survive")

	var gotEllipsis string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-ellipsis",
	).Scan(&gotEllipsis), "query copied ellipsis codex preview")
	assert.Equal(t, "clipped [encrypted]", gotEllipsis)

	var gotShortTail string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-short-tail",
	).Scan(&gotShortTail), "query copied short-tail codex preview")
	assert.Equal(t, strings.Repeat("p", 290)+"[encrypted]", gotShortTail)

	var gotLookalike string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-lookalike",
	).Scan(&gotLookalike), "query copied lookalike codex preview")
	assert.Equal(t, "mentions gAAAAAabc", gotLookalike)

	var gotLookalikeEllipsis string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-lookalike-ellipsis",
	).Scan(&gotLookalikeEllipsis),
		"query copied ellipsis lookalike codex preview")
	assert.Equal(t,
		"trailing lookalike gAAAAAabc...", gotLookalikeEllipsis)

	var gotLiteralToken string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-literal-token",
	).Scan(&gotLiteralToken),
		"query copied literal-token codex preview")
	assert.Equal(t, "quoted "+fernet, gotLiteralToken)

	var gotClippedContent string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan-clipped-content",
	).Scan(&gotClippedContent), "query copied clipped codex content")
	assert.Equal(t,
		"[Task: spawn_agent]\n[encrypted]", gotClippedContent)

	var gotPreview string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan",
	).Scan(&gotPreview), "query copied codex preview")
	assert.Equal(t, "[encrypted]", gotPreview)

	var gotClipped string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		"enc-orphan-clipped",
	).Scan(&gotClipped), "query copied clipped codex preview")
	assert.Equal(t, "[encrypted]", gotClipped)

	var gotContent string
	var gotLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length
		 FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"enc-orphan",
	).Scan(&gotContent, &gotLength), "query copied codex message")
	assert.Equal(t, "[Task: spawn_agent]\n[encrypted]", gotContent)
	assert.Equal(t, len("[Task: spawn_agent]\n[encrypted]"), gotLength)

	var gotInput string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT input_json
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 0`,
		"enc-orphan",
	).Scan(&gotInput), "query copied codex tool input")
	assert.Equal(t,
		`{"task_name":"multiply","message":"[encrypted]"}`, gotInput)

	var gotOther string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"other-orphan",
	).Scan(&gotOther), "query copied non-codex message")
	assert.Equal(t, fernet, gotOther)
}

func TestCopyPreservedDataRedactsLegacyCodexEncryptedHeader(t *testing.T) {
	const (
		fernet            = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
		legacyDataVersion = 67
	)
	copies := []struct {
		name  string
		trash bool
		copy  func(*DB, string) (int, error)
	}{
		{
			name: "orphaned",
			copy: func(dst *DB, sourcePath string) (int, error) {
				return dst.CopyOrphanedDataFrom(sourcePath)
			},
		},
		{
			name:  "trashed",
			trash: true,
			copy: func(dst *DB, sourcePath string) (int, error) {
				return dst.CopyTrashedDataFrom(sourcePath)
			},
		},
	}
	for _, cp := range copies {
		t.Run(cp.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "old.db")
			source := testDBAtPath(t, sourcePath, "src")
			insertSession(t, source, "encrypted-header", "proj",
				func(sess *Session) {
					sess.Agent = "codex"
				},
			)
			require.NoError(t,
				source.SetSessionDataVersion("encrypted-header", legacyDataVersion),
				"stamp the legacy parser row")
			content := "[Task: " + fernet + "]\nRun the task"
			message := userMsg("encrypted-header", 0, content)
			message.Role = "assistant"
			message.HasToolUse = true
			message.ToolCalls = []ToolCall{{
				SessionID: "encrypted-header",
				ToolName:  "spawn_agent",
				Category:  "Task",
				ToolUseID: "spawn-call",
				InputJSON: `{"task_name":"[encrypted]","message":"Run the task"}`,
			}}
			insertMessages(t, source, message)
			if cp.trash {
				require.NoError(t, source.SoftDeleteSession("encrypted-header"),
					"soft-delete source session")
			}
			_, err := source.getWriter().ExecContext(ctx,
				fmt.Sprintf("PRAGMA user_version = %d", legacyDataVersion))
			require.NoError(t, err, "mark source as a legacy version")
			require.NoError(t, source.Close(), "close source")

			destination := testDBAtPath(
				t, filepath.Join(dir, "new.db"), "dst",
			)
			defer destination.Close()
			count, err := cp.copy(destination, sourcePath)
			require.NoError(t, err, "copy preserved data")
			require.Equal(t, 1, count, "copy one preserved session")

			var gotContent string
			var gotLength, gotVersion int
			require.NoError(t, destination.getReader().QueryRowContext(ctx, `
				SELECT m.content, m.content_length, s.data_version
				  FROM messages m
				  JOIN sessions s ON s.id = m.session_id
				 WHERE m.session_id = ? AND m.ordinal = 0`,
				"encrypted-header",
			).Scan(&gotContent, &gotLength, &gotVersion))
			want := "[Task: [encrypted]]\nRun the task"
			assert.Equal(t, want, gotContent)
			assert.Equal(t, len(want), gotLength)
			assert.Equal(t, legacyDataVersion, gotVersion,
				"copy-time repair must preserve the parser version")
			unverified, err := destination.UnverifiedCodexSessionIDs(ctx)
			require.NoError(t, err)
			assert.NotContains(t, unverified, "encrypted-header",
				"the repaired transcript revision must be certified")
		})
	}
}

// The copy-time certification must not approve rows the scoped scrub never
// examined: ciphertext can survive in shapes the scrub skips (a child that
// was never linked to its parent, a collab message whose tool_calls row was
// lost), and stamping those sessions with the watermark would let every
// downstream gate treat them as verified.
func TestCopyOrphanedDataWithholdsCertificationFromUnverifiedCodexRows(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")

	// A pre-lineage child that current parsers never re-linked: its inbound
	// encrypted delivery is stored as a plain user turn, but the session is
	// not marked subagent, so the scoped scrub skips it.
	insertSession(t, srcDB, "unlinked-child", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB, userMsg("unlinked-child", 0, fernet))

	// A formatted collab tool message whose tool_calls row was lost: the
	// scrub's EXISTS filter cannot see it.
	lostToolCall := userMsg("lost-tool-call", 0, "[Task: spawn_agent]\n"+fernet)
	lostToolCall.Role = "assistant"
	lostToolCall.HasToolUse = true
	insertSession(t, srcDB, "lost-tool-call", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB, lostToolCall)

	// Both legacy metadata signals are absent: only the formatted Task block
	// identifies the encrypted collaboration payload.
	staleMissingToolCall := userMsg(
		"stale-missing-tool-call", 0, "[Task: spawn_agent]\n"+fernet,
	)
	staleMissingToolCall.Role = "assistant"
	staleMissingToolCall.HasToolUse = false
	insertSession(t, srcDB, "stale-missing-tool-call", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB, staleMissingToolCall)

	// A token-free codex session is certified outright.
	insertSession(t, srcDB, "clean-codex", "proj",
		func(s *Session) { s.Agent = "codex" })
	insertMessages(t, srcDB, userMsg("clean-codex", 0, "no payloads here"))

	_, err := srcDB.getWriter().ExecContext(ctx, fmt.Sprintf(
		"PRAGMA user_version = %d", redactedCodexSourceDataVersion-1,
	))
	require.NoError(t, err, "downgrade source data version")
	require.NoError(t, srcDB.Close(), "close source")

	dstPath := filepath.Join(dir, "new.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 4, count, "expected four orphans")

	dataVersions := map[string]int{}
	for _, sessionID := range []string{
		"unlinked-child", "lost-tool-call", "stale-missing-tool-call",
		"clean-codex",
	} {
		var dataVersion int
		require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
			`SELECT data_version FROM sessions WHERE id = ?`, sessionID,
		).Scan(&dataVersion), "query copied data version for %s", sessionID)
		dataVersions[sessionID] = dataVersion
	}
	assert.Less(t, dataVersions["unlinked-child"],
		redactedCodexSourceDataVersion,
		"an unexamined encrypted user turn must not be certified")
	assert.Less(t, dataVersions["lost-tool-call"],
		redactedCodexSourceDataVersion,
		"a collab message without its tool_calls row must not be certified")
	assert.Less(t, dataVersions["stale-missing-tool-call"],
		redactedCodexSourceDataVersion,
		"a formatted collab block must not be certified when both metadata signals are missing")
	assert.Less(t, dataVersions["clean-codex"],
		redactedCodexSourceDataVersion,
		"certification must not advance the clean session's parser version")
	unverified, err := dstDB.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"unlinked-child", "lost-tool-call", "stale-missing-tool-call",
	}, unverified,
		"only transcript revisions the exhaustive verifier rejects stay withheld")

	// The withheld rows keep their content: withholding the watermark is
	// about certification, not additional scrubbing.
	var gotChild string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		"unlinked-child",
	).Scan(&gotChild), "query copied unlinked child message")
	assert.Equal(t, fernet, gotChild)
}

// A source archive already at the watermark skips the scrub, so its copied
// rows must not be certified by a pass that never ran: per-row versions can
// lag the archive version when an older daemon wrote rows after the resync.
func TestCopyOrphanedDataSkipsCodexCertificationForCurrentSources(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")

	insertSession(t, srcDB, "stale-row", "proj", codexSubagentSession)
	insertMessages(t, srcDB, userMsg("stale-row", 0, fernet))
	require.NoError(t, srcDB.Close(), "close source")

	dstPath := filepath.Join(dir, "new.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected one orphan")

	var dataVersion int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT data_version FROM sessions WHERE id = ?`, "stale-row",
	).Scan(&dataVersion), "query copied data version")
	assert.Less(t, dataVersion, redactedCodexSourceDataVersion,
		"a skipped scrub must not certify lagging rows")
	unverified, err := dstDB.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"stale-row"}, unverified,
		"the copied row must lack a certification for its transcript revision")
}

func codexSubagentSession(s *Session) {
	s.Agent = "codex"
	s.RelationshipType = "subagent"
}

func TestCopyOrphanedDataSanitizesCopiedContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "poison-orphan", "proj")
	insertMessages(t, srcDB, userMsg("poison-orphan", 0, "clean"))
	var messageID int64
	require.NoError(t, srcDB.getWriter().QueryRowContext(ctx,
		`SELECT id FROM messages WHERE session_id = ? AND ordinal = 0`,
		"poison-orphan",
	).Scan(&messageID), "query source message id")

	messageContent := "message\x00body\x01\nkept"
	toolInput := "{\"cmd\":\"tool\x00input\x04\"}"
	emptyToolInput := "\x00\x04"
	toolResult := "tool\x00result\x02"
	emptyToolResult := "\x00\x04"
	eventContent := "event\x00content\x03"
	const (
		messageLengthExcess = 7
		toolLengthExcess    = 11
		eventLengthExcess   = 5
		emptyResultLength   = 7
	)
	_, err := srcDB.getWriter().ExecContext(ctx,
		`UPDATE messages
		 SET content = ?, content_length = ?
		 WHERE id = ?`,
		messageContent, len(messageContent)+messageLengthExcess, messageID,
	)
	require.NoError(t, err, "plant poisoned message")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, input_json, result_content_length,
			result_content, call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-1",
		toolInput, len(toolResult)+toolLengthExcess, toolResult, 0,
	)
	require.NoError(t, err, "plant poisoned tool call")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, input_json, call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-empty",
		emptyToolInput, 1,
	)
	require.NoError(t, err, "plant empty-sanitized tool input")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, result_content_length, result_content,
			call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-empty-result",
		emptyResultLength, emptyToolResult, 2,
	)
	require.NoError(t, err, "plant empty-sanitized tool result")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_result_events (
			session_id, tool_call_message_ordinal, call_index,
			tool_use_id, source, status, content, content_length,
			event_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"poison-orphan", 0, 0, "tool-1", "tool_result", "ok",
		eventContent, len(eventContent)+eventLengthExcess, 0,
	)
	require.NoError(t, err, "plant poisoned tool result event")
	// Dirty content only exists in archives written before
	// sanitizedSourceDataVersion; sources at or above it skip the
	// sanitize pass entirely.
	_, err = srcDB.getWriter().ExecContext(ctx, fmt.Sprintf(
		"PRAGMA user_version = %d", sanitizedSourceDataVersion-1,
	))
	require.NoError(t, err, "downgrade source data version")
	require.NoError(t, srcDB.Close(), "close source")

	dstPath := filepath.Join(dir, "new.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected one orphan")

	var gotMessage string
	var gotMessageLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length
		 FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"poison-orphan",
	).Scan(&gotMessage, &gotMessageLength), "query copied message")
	wantMessage := SanitizeUTF8(messageContent)
	assert.Equal(t, wantMessage, gotMessage)
	assert.Equal(t, len(wantMessage)+messageLengthExcess, gotMessageLength)

	var gotToolInput string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT input_json
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 0`,
		"poison-orphan",
	).Scan(&gotToolInput), "query copied tool input")
	assert.Equal(t, SanitizeUTF8(toolInput), gotToolInput)

	var gotEmptyToolInput sql.NullString
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT input_json
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 1`,
		"poison-orphan",
	).Scan(&gotEmptyToolInput), "query empty copied tool input")
	assert.False(t, gotEmptyToolInput.Valid)

	var gotToolResult string
	var gotToolResultLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT result_content, result_content_length
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 0`,
		"poison-orphan",
	).Scan(&gotToolResult, &gotToolResultLength), "query copied tool call")
	wantToolResult := SanitizeUTF8(toolResult)
	assert.Equal(t, wantToolResult, gotToolResult)
	assert.Equal(t, len(wantToolResult)+toolLengthExcess, gotToolResultLength)

	var gotEmptyToolResult sql.NullString
	var gotEmptyToolResultLength sql.NullInt64
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT result_content, result_content_length
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 2`,
		"poison-orphan",
	).Scan(
		&gotEmptyToolResult,
		&gotEmptyToolResultLength,
	), "query empty copied tool call result")
	assert.False(t, gotEmptyToolResult.Valid)
	require.True(t, gotEmptyToolResultLength.Valid)
	assert.Equal(t,
		int64(emptyResultLength-len(emptyToolResult)),
		gotEmptyToolResultLength.Int64,
	)

	var gotEventContent string
	var gotEventLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length
		 FROM tool_result_events
		 WHERE session_id = ? AND event_index = 0`,
		"poison-orphan",
	).Scan(&gotEventContent, &gotEventLength), "query copied tool result event")
	wantEventContent := SanitizeUTF8(eventContent)
	assert.Equal(t, wantEventContent, gotEventContent)
	assert.Equal(t, len(wantEventContent)+eventLengthExcess, gotEventLength)
}

// TestCopySkipsSanitizeForSanitizedSource guards the resync fast
// path: each sanitize pass is skipped once the source data version
// proves ingest already sanitized that field — content/results at
// sanitizedSourceDataVersion, input_json at the later
// sanitizedInputSourceDataVersion — and skipped rows survive the
// copy verbatim.
func TestCopySkipsSanitizeForSanitizedSource(t *testing.T) {
	const rawContent = "nul\x00byte\x01kept"
	const rawToolInput = "{\"cmd\":\"a\x00b\x01\"}"
	const rawToolResult = "result\x00kept\x01"
	copies := []struct {
		name  string
		trash bool
		copy  func(dst *DB, srcPath string) (int, error)
	}{
		{
			name: "orphaned",
			copy: func(dst *DB, srcPath string) (int, error) {
				return dst.CopyOrphanedDataFrom(srcPath)
			},
		},
		{
			name:  "trashed",
			trash: true,
			copy: func(dst *DB, srcPath string) (int, error) {
				return dst.CopyTrashedDataFrom(srcPath)
			},
		},
	}
	versions := []struct {
		name          string
		sourceVersion int
		wantInput     string
	}{
		{
			// Ingest at v58 sanitized content and results but not
			// input_json, so only the input pass runs for it.
			name:          "content-sanitized source pays input pass",
			sourceVersion: sanitizedSourceDataVersion,
			wantInput:     SanitizeUTF8(rawToolInput),
		},
		{
			// A source at the input watermark is fully clean; every
			// pass is skipped and rows copy verbatim.
			name:          "fully sanitized source copies verbatim",
			sourceVersion: sanitizedInputSourceDataVersion,
			wantInput:     rawToolInput,
		},
	}
	for _, cp := range copies {
		for _, ver := range versions {
			t.Run(cp.name+"/"+ver.name, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				srcPath := filepath.Join(dir, "old.db")
				srcDB := testDBAtPath(t, srcPath, "src")
				insertSession(t, srcDB, "sess", "proj")
				insertMessages(t, srcDB, userMsg("sess", 0, "clean"))
				_, err := srcDB.getWriter().ExecContext(ctx,
					`UPDATE messages SET content = ? WHERE session_id = ?`,
					rawContent, "sess",
				)
				require.NoError(t, err, "plant raw content")
				var messageID int64
				require.NoError(t, srcDB.getWriter().QueryRowContext(ctx,
					`SELECT id FROM messages WHERE session_id = ?`, "sess",
				).Scan(&messageID), "read message id")
				_, err = srcDB.getWriter().ExecContext(ctx,
					`INSERT INTO tool_calls (
						message_id, session_id, tool_name, category,
						tool_use_id, input_json, result_content_length,
						result_content, call_index
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					messageID, "sess", "Bash", "execution", "tool-1",
					rawToolInput, len(rawToolResult), rawToolResult, 0,
				)
				require.NoError(t, err, "plant raw tool call")
				_, err = srcDB.getWriter().ExecContext(ctx, fmt.Sprintf(
					"PRAGMA user_version = %d", ver.sourceVersion,
				))
				require.NoError(t, err, "set source data version")
				if cp.trash {
					require.NoError(t, srcDB.SoftDeleteSession("sess"),
						"soft delete source session")
				}
				require.NoError(t, srcDB.Close(), "close source")

				dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
				defer dstDB.Close()
				count, err := cp.copy(dstDB, srcPath)
				require.NoError(t, err, "copy from source")
				require.Equal(t, 1, count, "copied sessions")

				var got string
				require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
					`SELECT content FROM messages WHERE session_id = ?`,
					"sess",
				).Scan(&got), "query copied message")
				assert.Equal(t, rawContent, got,
					"content-sanitized source must copy content verbatim")

				var gotInput, gotResult string
				require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
					`SELECT input_json, result_content
					 FROM tool_calls WHERE session_id = ?`,
					"sess",
				).Scan(&gotInput, &gotResult), "query copied tool call")
				assert.Equal(t, ver.wantInput, gotInput)
				assert.Equal(t, rawToolResult, gotResult,
					"tool result must copy verbatim for sanitized sources")
			})
		}
	}
}
