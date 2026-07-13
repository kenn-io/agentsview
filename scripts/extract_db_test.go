package scripts

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	avdb "go.kenn.io/agentsview/internal/db"
)

// TestExtractDBBlocksTermsAcrossCoveredColumns exercises the screenshot
// blocked-terms filter end to end: it seeds a source database with sessions
// in a screenshot-whitelisted project, runs extract-db.sh with a blocked
// term, and asserts that every session referencing the term is dropped while
// clean sessions survive. Each blocked session hides the term in a different
// covered column so the test pins the full coverage surface, including the
// git_branch, tool_calls.file_path, and tool_calls.skill_name columns.
func TestExtractDBBlocksTermsAcrossCoveredColumns(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "source.db")

	// Build the source database with the real schema so the test never
	// drifts from the production DDL (FTS table, triggers, all tables).
	d, err := avdb.Open(srcPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err)
	defer conn.Close()

	// All sessions share one timestamp so every row sits inside the
	// extract window (which spans 60 days back from the newest session).
	const ts = "2026-06-01T12:00:00.000Z"
	insertSession := func(id, branch, firstMsg string) {
		_, err := conn.Exec(
			`INSERT INTO sessions
			   (id, project, created_at, started_at, message_count,
			    user_message_count, git_branch, first_message)
			 VALUES (?, 'agentsview', ?, '', 1, 1, ?, ?)`,
			id, ts, branch, firstMsg,
		)
		require.NoError(t, err)
	}
	insertMessage := func(id int, sessionID, content string) {
		_, err := conn.Exec(
			`INSERT INTO messages (id, session_id, ordinal, role, content)
			 VALUES (?, ?, 0, 'user', ?)`,
			id, sessionID, content,
		)
		require.NoError(t, err)
	}
	insertToolCall := func(sessionID, filePath, skillName, inputJSON string) {
		_, err := conn.Exec(
			`INSERT INTO tool_calls
			   (message_id, session_id, tool_name, category,
			    file_path, skill_name, input_json)
			 VALUES (0, ?, 'Edit', 'edit', ?, ?, ?)`,
			sessionID, filePath, skillName, inputJSON,
		)
		require.NoError(t, err)
	}

	// Clean session: nothing references the blocked term -> survives.
	insertSession("s_keep", "main", "ordinary refactoring work")
	insertMessage(1, "s_keep", "nothing sensitive in this transcript")

	// Term hidden in message content.
	insertSession("s_msg", "main", "ordinary prompt")
	insertMessage(2, "s_msg", "today I integrated with blocklist-demo-service")

	// Term hidden in git_branch (newly covered column).
	insertSession("s_branch", "feat/blocklist-demo-service-sync", "branch work")
	insertMessage(3, "s_branch", "clean message body")

	// Term hidden in a tool call's file_path (newly covered column).
	insertSession("s_tcpath", "main", "file edits")
	insertMessage(4, "s_tcpath", "clean message body")
	insertToolCall("s_tcpath", "/Users/dev/code/blocklist-demo-service/main.go", "", `{"a":1}`)

	// Term hidden in a tool call's skill_name (newly covered column).
	insertSession("s_skill", "main", "skill run")
	insertMessage(5, "s_skill", "clean message body")
	insertToolCall("s_skill", "/tmp/clean.go", "blocklist-demo-service-deploy", `{"b":2}`)

	// Flush the WAL so the sqlite3 CLI sees every committed row.
	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	outPath := filepath.Join(tempDir, "out.db")
	scriptPath := filepath.Join("..", "docs", "screenshots", "extract-db.sh")
	cmd := exec.Command("bash", scriptPath, srcPath, outPath)
	cmd.Env = append(
		os.Environ(),
		"SCREENSHOT_BLOCKED_TERMS=blocklist-demo-service",
		// Pin the file source to a path that does not exist so the test is
		// hermetic and ignores any real blocked-terms file on the host.
		"SCREENSHOT_BLOCKED_TERMS_FILE="+filepath.Join(tempDir, "absent.txt"),
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "extract-db.sh failed: %s", out)

	outConn, err := sql.Open("sqlite3", outPath)
	require.NoError(t, err)
	defer outConn.Close()

	rows, err := outConn.Query("SELECT id FROM sessions ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{"s_keep"}, ids,
		"only the clean session should survive; sessions hiding the term in "+
			"message content, git_branch, tool file_path, and skill_name must "+
			"all be dropped")
}

// TestExtractDBUsesPrivateTermsFileByDefault protects the default screenshot
// scrub path. The screenshot runner should pick up the canonical private terms
// file without requiring SCREENSHOT_BLOCKED_TERMS_FILE for every local run.
func TestExtractDBUsesPrivateTermsFileByDefault(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "source.db")
	d, err := avdb.Open(srcPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err)
	defer conn.Close()

	const ts = "2026-06-01T12:00:00.000Z"
	seed := func(id, content string) {
		_, err := conn.Exec(
			`INSERT INTO sessions
			   (id, project, created_at, started_at, message_count,
			    user_message_count)
			 VALUES (?, 'agentsview', ?, '', 1, 1)`,
			id, ts,
		)
		require.NoError(t, err)
		_, err = conn.Exec(
			`INSERT INTO messages (session_id, ordinal, role, content)
			 VALUES (?, 0, 'user', ?)`,
			id, content,
		)
		require.NoError(t, err)
	}
	seed("s_keep", "ordinary screenshot-safe notes")
	seed("s_drop", "mentions blocklist-demo-service in the transcript")

	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	privateTermsDir := filepath.Join(tempDir, ".config", "kenn")
	require.NoError(t, os.MkdirAll(privateTermsDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(privateTermsDir, "private-terms.txt"),
		[]byte("blocklist-demo-service\n"),
		0o600,
	))

	outPath := filepath.Join(tempDir, "out.db")
	scriptPath := filepath.Join("..", "docs", "screenshots", "extract-db.sh")
	cmd := exec.Command("bash", scriptPath, srcPath, outPath)
	cmd.Env = filteredEnv(
		"SCREENSHOT_BLOCKED_TERMS_FILE",
		"KENN_PRIVATE_TERMS_FILE",
	)
	cmd.Env = append(
		cmd.Env,
		"HOME="+tempDir,
		"SCREENSHOT_BLOCKED_TERMS=",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "extract-db.sh failed: %s", out)

	outConn, err := sql.Open("sqlite3", outPath)
	require.NoError(t, err)
	defer outConn.Close()
	rows, err := outConn.Query("SELECT id FROM sessions ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{"s_keep"}, ids)
}

func TestExtractDBRedactsHomePathByDefault(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "source.db")
	d, err := avdb.Open(srcPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err)
	defer conn.Close()

	homePath := filepath.Join(tempDir, "home", "local-user")
	require.NoError(t, os.MkdirAll(homePath, 0o700))

	const ts = "2026-06-01T12:00:00.000Z"
	seed := func(id, firstMessage, content string) {
		_, err := conn.Exec(
			`INSERT INTO sessions
			   (id, project, created_at, started_at, message_count,
			    user_message_count, first_message)
			 VALUES (?, 'agentsview', ?, '', 1, 1, ?)`,
			id, ts, firstMessage,
		)
		require.NoError(t, err)
		_, err = conn.Exec(
			`INSERT INTO messages (session_id, ordinal, role, content)
			 VALUES (?, 0, 'user', ?)`,
			id, content,
		)
		require.NoError(t, err)
	}
	seed("s_keep", "ordinary screenshot-safe notes", "clean transcript")
	seed(
		"s_redact",
		"Review work under "+filepath.Join(homePath, "code", "project-a"),
		"Inspect "+filepath.Join(homePath, "code", "project-a", "main.go"),
	)

	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	outPath := filepath.Join(tempDir, "out.db")
	scriptPath := filepath.Join("..", "docs", "screenshots", "extract-db.sh")
	cmd := exec.Command("bash", scriptPath, srcPath, outPath)
	cmd.Env = append(
		filteredEnv(
			"SCREENSHOT_BLOCKED_TERMS_FILE",
			"KENN_PRIVATE_TERMS_FILE",
		),
		"HOME="+homePath,
		"SCREENSHOT_BLOCKED_TERMS=",
		"SCREENSHOT_BLOCKED_TERMS_FILE="+filepath.Join(tempDir, "absent.txt"),
		"KENN_PRIVATE_TERMS_FILE="+filepath.Join(tempDir, "absent-private.txt"),
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "extract-db.sh failed: %s", out)

	outConn, err := sql.Open("sqlite3", outPath)
	require.NoError(t, err)
	defer outConn.Close()
	rows, err := outConn.Query("SELECT id FROM sessions ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{"s_keep", "s_redact"}, ids)

	var firstMessage, content string
	require.NoError(t, outConn.QueryRow(
		`SELECT s.first_message, m.content
		 FROM sessions s
		 JOIN messages m ON m.session_id = s.id
		 WHERE s.id = 's_redact'`,
	).Scan(&firstMessage, &content))
	assert.Equal(t, "Review work under ~/code/project-a", firstMessage)
	assert.Equal(t, "Inspect ~/code/project-a/main.go", content)
	assert.NotContains(t, firstMessage, homePath)
	assert.NotContains(t, content, homePath)
}

func TestExtractDBUsesPrivateTermsFileWithScreenshotFileOverride(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "source.db")
	d, err := avdb.Open(srcPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err)
	defer conn.Close()

	const ts = "2026-06-01T12:00:00.000Z"
	seed := func(id, content string) {
		_, err := conn.Exec(
			`INSERT INTO sessions
			   (id, project, created_at, started_at, message_count,
			    user_message_count)
			 VALUES (?, 'agentsview', ?, '', 1, 1)`,
			id, ts,
		)
		require.NoError(t, err)
		_, err = conn.Exec(
			`INSERT INTO messages (session_id, ordinal, role, content)
			 VALUES (?, 0, 'user', ?)`,
			id, content,
		)
		require.NoError(t, err)
	}
	seed("s_keep", "ordinary screenshot-safe notes")
	seed("s_drop_private", "mentions private-only-demo in the transcript")
	seed("s_drop_screenshot", "mentions screenshot-only-demo in the transcript")

	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	privateTerms := filepath.Join(tempDir, "private-terms.txt")
	require.NoError(t, os.WriteFile(privateTerms, []byte("private-only-demo\n"), 0o600))
	screenshotTerms := filepath.Join(tempDir, "screenshot-terms.txt")
	require.NoError(t, os.WriteFile(screenshotTerms, []byte("screenshot-only-demo\n"), 0o600))

	outPath := filepath.Join(tempDir, "out.db")
	scriptPath := filepath.Join("..", "docs", "screenshots", "extract-db.sh")
	cmd := exec.Command("bash", scriptPath, srcPath, outPath)
	cmd.Env = append(
		filteredEnv("KENN_PRIVATE_TERMS_FILE"),
		"SCREENSHOT_BLOCKED_TERMS=",
		"SCREENSHOT_BLOCKED_TERMS_FILE="+screenshotTerms,
		"KENN_PRIVATE_TERMS_FILE="+privateTerms,
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "extract-db.sh failed: %s", out)

	outConn, err := sql.Open("sqlite3", outPath)
	require.NoError(t, err)
	defer outConn.Close()
	rows, err := outConn.Query("SELECT id FROM sessions ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{"s_keep"}, ids)
}

// TestExtractDBKeepsOnlyRootTrees exercises the screenshot fixture's sidebar
// hygiene end to end. Child sessions only survive when their parent tree is
// exported, old descendants of an exported root stay available, and automated
// sessions remain in the fixture so the app can classify/filter them itself.
func TestExtractDBKeepsOnlyRootTrees(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "source.db")
	d, err := avdb.Open(srcPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err)
	defer conn.Close()

	insertSession := func(
		id, parentID, relationship, createdAt string,
		automated, messageCount, userMessageCount int,
	) {
		_, err := conn.Exec(
			`INSERT INTO sessions
			   (id, project, created_at, started_at, message_count,
			    user_message_count, parent_session_id, relationship_type,
			    is_automated, first_message)
			 VALUES (?, 'agentsview', ?, '', ?, ?, NULLIF(?, ''), ?, ?, ?)`,
			id, createdAt, messageCount, userMessageCount,
			parentID, relationship, automated, id+" prompt",
		)
		require.NoError(t, err)
		_, err = conn.Exec(
			`INSERT INTO messages (session_id, ordinal, role, content)
			 VALUES (?, 0, 'user', ?)`,
			id, id+" message",
		)
		require.NoError(t, err)
	}

	const recent = "2026-06-01T12:00:00.000Z"
	const old = "2026-01-01T12:00:00.000Z"
	insertSession("root_keep", "", "", recent, 0, 3, 2)
	insertSession("child_keep", "root_keep", "subagent", recent, 0, 1, 1)
	insertSession("old_child_keep", "root_keep", "subagent", old, 0, 1, 1)
	insertSession("automated_root_keep", "", "", recent, 1, 1, 1)
	insertSession("automated_child_keep", "root_keep", "subagent", recent, 1, 1, 1)
	insertSession("child_of_automated_root_keep", "automated_root_keep", "subagent", recent, 0, 1, 1)
	insertSession("orphan_subagent_drop", "missing_parent", "subagent", recent, 0, 1, 1)
	insertSession("orphan_fork_drop", "missing_parent", "fork", recent, 0, 1, 1)
	insertSession("old_root_drop", "", "", old, 0, 3, 2)
	insertSession("old_thinking_keep", "", "", old, 0, 3, 2)
	insertSession("automated_thinking_keep", "", "", recent, 1, 1, 1)
	_, err = conn.Exec(
		"UPDATE sessions SET machine = 'private-workstation'",
	)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO project_identity_observations
		   (session_id, project, machine, root_path, observed_at)
		 VALUES
		   ('root_keep', 'agentsview', 'private-workstation',
		    '/private/agentsview', ?)`,
		recent,
	)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE session_project_identity_snapshots
		 SET machine = 'private-workstation', root_path = '/private/agentsview'
		 WHERE session_id = 'root_keep'`,
	)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO worktree_project_mappings
		   (machine, path_prefix, project)
		 VALUES ('private-workstation', '/private', 'agentsview')`,
	)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE messages
		 SET thinking_text = 'private reasoning', has_thinking = 1
		 WHERE session_id IN ('old_thinking_keep', 'automated_thinking_keep')`,
	)
	require.NoError(t, err)

	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	outPath := filepath.Join(tempDir, "out.db")
	scriptPath := filepath.Join("..", "docs", "screenshots", "extract-db.sh")
	cmd := exec.Command("bash", scriptPath, srcPath, outPath)
	cmd.Env = append(
		os.Environ(),
		"SCREENSHOT_BLOCKED_TERMS=",
		"SCREENSHOT_BLOCKED_TERMS_FILE="+filepath.Join(tempDir, "absent.txt"),
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "extract-db.sh failed: %s", out)

	outConn, err := sql.Open("sqlite3", outPath)
	require.NoError(t, err)
	defer outConn.Close()

	rows, err := outConn.Query("SELECT id FROM sessions ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{
		"automated_child_keep",
		"automated_root_keep",
		"automated_thinking_keep",
		"child_keep",
		"child_of_automated_root_keep",
		"old_child_keep",
		"old_thinking_keep",
		"root_keep",
	}, ids)

	var automatedCount int
	require.NoError(t, outConn.QueryRow(
		"SELECT COUNT(*) FROM sessions WHERE is_automated = 1",
	).Scan(&automatedCount))
	assert.Equal(t, 3, automatedCount)

	machineRows, err := outConn.Query(
		`SELECT m.name
		 FROM sqlite_master m
		 JOIN pragma_table_info(m.name) p ON p.name = 'machine'
		 WHERE m.type = 'table'
		 ORDER BY m.name`,
	)
	require.NoError(t, err)
	var machineTables []string
	for machineRows.Next() {
		var table string
		require.NoError(t, machineRows.Scan(&table))
		machineTables = append(machineTables, table)
	}
	require.NoError(t, machineRows.Err())
	require.NoError(t, machineRows.Close())
	for _, table := range machineTables {
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		var unexpected int
		require.NoError(t, outConn.QueryRow(
			"SELECT COUNT(*) FROM "+quoted+" WHERE machine != 'dev-laptop'",
		).Scan(&unexpected))
		assert.Zero(t, unexpected, table)
	}

	for _, table := range []string{
		"project_identity_observations",
		"session_project_identity_snapshots",
		"project_identity_observation_changes",
		"session_project_identity_snapshot_changes",
		"worktree_project_mappings",
	} {
		var count int
		require.NoError(t, outConn.QueryRow(
			"SELECT COUNT(*) FROM "+table,
		).Scan(&count))
		assert.Zero(t, count, table)
	}

	var orphanChildCount int
	require.NoError(t, outConn.QueryRow(
		`SELECT COUNT(*)
		 FROM sessions child
		 WHERE child.relationship_type IN ('subagent', 'fork', 'continuation')
		   AND NOT EXISTS (
		     SELECT 1 FROM sessions parent
		     WHERE parent.id = child.parent_session_id
		   )`,
	).Scan(&orphanChildCount))
	assert.Zero(t, orphanChildCount)
}

// TestExtractDBTermsFileSkipsCommentsAndBlanks pins the terms-file format:
// comment lines (leading '#') and blank lines are ignored. A bare '#' must
// not become the pattern '%#%', which would match nearly every transcript and
// drop almost all sessions.
func TestExtractDBTermsFileSkipsCommentsAndBlanks(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "source.db")
	d, err := avdb.Open(srcPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err)
	defer conn.Close()

	const ts = "2026-06-01T12:00:00.000Z"
	seed := func(id, content string) {
		_, err := conn.Exec(
			`INSERT INTO sessions
			   (id, project, created_at, started_at, message_count,
			    user_message_count)
			 VALUES (?, 'agentsview', ?, '', 1, 1)`,
			id, ts,
		)
		require.NoError(t, err)
		_, err = conn.Exec(
			`INSERT INTO messages (session_id, ordinal, role, content)
			 VALUES (?, 0, 'user', ?)`,
			id, content,
		)
		require.NoError(t, err)
	}
	// Clean session whose transcript contains '#' (a markdown heading). If a
	// bare '#' comment line leaked through as the pattern '%#%', this session
	// would be dropped.
	seed("s_keep", "## Heading\nordinary notes, nothing blocked here")
	// Session referencing the one real blocked term.
	seed("s_drop", "spent the day on blocklist-demo-service internals")

	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	termsFile := filepath.Join(tempDir, "terms.txt")
	require.NoError(t, os.WriteFile(termsFile, []byte(
		"# a comment line, ignored\n"+
			"#\n"+
			"   # indented comment, ignored\n"+
			"\n"+
			"blocklist-demo-service\n",
	), 0o600))

	outPath := filepath.Join(tempDir, "out.db")
	scriptPath := filepath.Join("..", "docs", "screenshots", "extract-db.sh")
	cmd := exec.Command("bash", scriptPath, srcPath, outPath)
	cmd.Env = append(
		os.Environ(),
		"SCREENSHOT_BLOCKED_TERMS=",
		"SCREENSHOT_BLOCKED_TERMS_FILE="+termsFile,
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "extract-db.sh failed: %s", out)

	outConn, err := sql.Open("sqlite3", outPath)
	require.NoError(t, err)
	defer outConn.Close()
	rows, err := outConn.Query("SELECT id FROM sessions ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{"s_keep"}, ids,
		"comment and blank lines must be skipped: the clean session containing "+
			"'#' must survive and only the real term may drop a session")
}

func filteredEnv(dropKeys ...string) []string {
	drops := make(map[string]struct{}, len(dropKeys))
	for _, key := range dropKeys {
		drops[key] = struct{}{}
	}

	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			filtered = append(filtered, entry)
			continue
		}
		if _, drop := drops[key]; drop {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
