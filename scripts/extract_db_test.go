package scripts

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
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
			   (id, project, created_at, started_at, git_branch, first_message)
			 VALUES (?, 'agentsview', ?, '', ?, ?)`,
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
	insertMessage(2, "s_msg", "today I integrated with ghosthub")

	// Term hidden in git_branch (newly covered column).
	insertSession("s_branch", "feat/ghosthub-sync", "branch work")
	insertMessage(3, "s_branch", "clean message body")

	// Term hidden in a tool call's file_path (newly covered column).
	insertSession("s_tcpath", "main", "file edits")
	insertMessage(4, "s_tcpath", "clean message body")
	insertToolCall("s_tcpath", "/Users/dev/code/ghosthub/main.go", "", `{"a":1}`)

	// Term hidden in a tool call's skill_name (newly covered column).
	insertSession("s_skill", "main", "skill run")
	insertMessage(5, "s_skill", "clean message body")
	insertToolCall("s_skill", "/tmp/clean.go", "ghosthub-deploy", `{"b":2}`)

	// Flush the WAL so the sqlite3 CLI sees every committed row.
	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	outPath := filepath.Join(tempDir, "out.db")
	scriptPath := filepath.Join("..", "docs", "screenshots", "extract-db.sh")
	cmd := exec.Command("bash", scriptPath, srcPath, outPath)
	cmd.Env = append(
		os.Environ(),
		"SCREENSHOT_BLOCKED_TERMS=ghosthub",
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
			`INSERT INTO sessions (id, project, created_at, started_at)
			 VALUES (?, 'agentsview', ?, '')`,
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
	seed("s_drop", "spent the day on ghosthub internals")

	_, err = conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	termsFile := filepath.Join(tempDir, "terms.txt")
	require.NoError(t, os.WriteFile(termsFile, []byte(
		"# a comment line, ignored\n"+
			"#\n"+
			"   # indented comment, ignored\n"+
			"\n"+
			"ghosthub\n",
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
