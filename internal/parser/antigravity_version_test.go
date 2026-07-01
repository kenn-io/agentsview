package parser

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agyBaselineCreateStatements are the exact CREATE statements of the verified
// agy 1.0.7-1.0.10 schema (user_version=1, 7 tables, 2 indices), copied
// byte-for-byte from a real ~/.gemini/antigravity-cli database. Executing them
// verbatim makes sqlite_master store identical sql text, so the Go fingerprint
// reproduces agy-reader's recorded sha256:1ca98426...f72f43.
var agyBaselineCreateStatements = []string{
	"CREATE TABLE `trajectory_meta` (`trajectory_id` text,`cascade_id` text," +
		"`trajectory_type` integer,`source` integer,PRIMARY KEY (`trajectory_id`))",
	"CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0," +
		"`status` integer NOT NULL DEFAULT 0," +
		"`has_subtrajectory` numeric NOT NULL DEFAULT false," +
		"`metadata` blob,`error_details` blob,`permissions` blob," +
		"`task_details` blob,`render_info` blob,`step_payload` blob," +
		"`step_format` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`))",
	"CREATE TABLE `gen_metadata` (`idx` integer,`data` blob," +
		"`size` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`))",
	"CREATE TABLE `executor_metadata` (`idx` integer,`data` blob,PRIMARY KEY (`idx`))",
	"CREATE TABLE `parent_references` (`idx` integer,`data` blob,PRIMARY KEY (`idx`))",
	"CREATE TABLE `trajectory_metadata_blob` (`id` text DEFAULT \"main\"," +
		"`data` blob,PRIMARY KEY (`id`))",
	"CREATE TABLE `battle_mode_infos` (`idx` integer,`data` blob,PRIMARY KEY (`idx`))",
	"CREATE INDEX `idx_steps_status` ON `steps`(`status`)",
	"CREATE INDEX `idx_steps_step_type` ON `steps`(`step_type`)",
}

// createAntigravityBaselineSchemaDB writes a .db whose schema fingerprint
// matches the verified agy 1.0.7-1.0.10 baseline. It executes the real CREATE
// statements verbatim and sets user_version=1, then seeds two decodable steps
// so the parser produces a usable session.
func createAntigravityBaselineSchemaDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open")
	defer db.Close()
	for _, stmt := range agyBaselineCreateStatements {
		mustExec(t, db, stmt)
	}
	mustExec(t, db, "PRAGMA user_version = 1")

	tsEarly := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000000},
	})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user prompt text goes here")},
	})
	tsLate := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000100},
	})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant reply content body")},
	})
	mustExec(t, db,
		"INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)",
		0, 14, userPayload)
	mustExec(t, db,
		"INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)",
		1, 17, asstPayload)
}

// openSchemaDB opens a fresh SQLite db at path and returns a handle the caller
// must close. The db is created by running each statement in stmts plus
// user_version.
func openSchemaDB(
	t *testing.T, path string, userVersion int, stmts ...string,
) *sql.DB {
	t.Helper()
	build, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open for build")
	for _, stmt := range stmts {
		_, err := build.Exec(stmt)
		require.NoError(t, err, "exec %q", stmt)
	}
	_, err = build.Exec("PRAGMA user_version = " + itoa(userVersion))
	require.NoError(t, err, "set user_version")
	require.NoError(t, build.Close(), "close build")

	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "reopen")
	return db
}

const agyBaselineFullFingerprint = "1ca98426f561fe73223c8620a238405030fdb3014444d970e1300a6009f72f43"

func TestAntigravitySchemaLabel(t *testing.T) {
	tests := []struct {
		name        string
		fingerprint string
		want        string
	}{
		{
			name:        "known baseline maps to release range",
			fingerprint: agyBaselineFullFingerprint,
			want:        "1.0.7-1.0.10",
		},
		{
			name:        "unknown fingerprint becomes agy-schema marker",
			fingerprint: "deadbeefcafe0000111122223333444455556666777788889999aaaabbbbcccc",
			want:        "agy-schema:deadbeefcafe",
		},
		{
			name:        "empty fingerprint yields empty label",
			fingerprint: "",
			want:        "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := antigravitySchemaLabel(tt.fingerprint)
			assert.Equal(t, tt.want, got)
			if got != "" {
				assert.LessOrEqual(t, len(got), 63,
					"label must stay under 64 chars")
			}
		})
	}
}

// TestDecodeConfidence exercises the single-source-of-truth confidence rule
// derived from an agent name and its source_version label. It covers the
// agent gate against SourceVersion being a generic field set by other parsers.
func TestDecodeConfidence(t *testing.T) {
	tests := []struct {
		name          string
		agent         string
		sourceVersion string
		want          string
	}{
		{
			name:          "antigravity known range is high",
			agent:         string(AgentAntigravity),
			sourceVersion: "1.0.7-1.0.10",
			want:          DecodeConfidenceHigh,
		},
		{
			name:          "antigravity-cli known range is high",
			agent:         string(AgentAntigravityCLI),
			sourceVersion: "1.0.7-1.0.10",
			want:          DecodeConfidenceHigh,
		},
		{
			name:          "antigravity unknown schema is low",
			agent:         string(AgentAntigravity),
			sourceVersion: antigravitySchemaUnknownPrefix + "abc123def456",
			want:          DecodeConfidenceLow,
		},
		{
			name:          "antigravity-cli unknown schema is low",
			agent:         string(AgentAntigravityCLI),
			sourceVersion: antigravitySchemaUnknownPrefix + "abc123def456",
			want:          DecodeConfidenceLow,
		},
		{
			name:          "antigravity empty source_version yields no value",
			agent:         string(AgentAntigravity),
			sourceVersion: "",
			want:          "",
		},
		{
			name:          "antigravity-cli empty source_version yields no value",
			agent:         string(AgentAntigravityCLI),
			sourceVersion: "",
			want:          "",
		},
		{
			name:          "piebald generic label yields no value",
			agent:         string(AgentPiebald),
			sourceVersion: "piebald-appdb-v1",
			want:          "",
		},
		{
			name:          "commandcode generic label yields no value",
			agent:         string(AgentCommandCode),
			sourceVersion: "2",
			want:          "",
		},
		{
			name:          "other agent with agy-schema-shaped label yields no value",
			agent:         string(AgentClaude),
			sourceVersion: antigravitySchemaUnknownPrefix + "abc123def456",
			want:          "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				DecodeConfidence(tt.agent, tt.sourceVersion))
		})
	}
}

// TestAntigravitySchemaFingerprintBaseline verifies the Go fingerprint of the
// real baseline schema matches agy-reader's recorded sha256, so the
// known-label mapping actually fires on real databases.
func TestAntigravitySchemaFingerprintBaseline(t *testing.T) {
	dir := t.TempDir()
	stmts := append([]string{}, agyBaselineCreateStatements...)
	db := openSchemaDB(t, filepath.Join(dir, "baseline.db"), 1, stmts...)
	defer db.Close()

	fp, err := antigravitySchemaFingerprint(db)
	require.NoError(t, err)
	assert.Equal(t, agyBaselineFullFingerprint, fp,
		"Go fingerprint must equal agy-reader recorded sha256")
	assert.Equal(t, "1.0.7-1.0.10", antigravitySchemaLabel(fp))
}

func TestAntigravitySchemaFingerprintCases(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name        string
		userVersion int
		stmts       []string
		wantLabel   string
	}{
		{
			name:        "baseline schema known label",
			userVersion: 1,
			stmts:       agyBaselineCreateStatements,
			wantLabel:   "1.0.7-1.0.10",
		},
		{
			name:        "extra table yields unknown marker",
			userVersion: 1,
			stmts: append(append([]string{}, agyBaselineCreateStatements...),
				"CREATE TABLE `future_widget` (`idx` integer,`data` blob,"+
					"PRIMARY KEY (`idx`))"),
			wantLabel: "", // checked as marker prefix below
		},
		{
			name:        "bumped user_version yields unknown marker",
			userVersion: 2,
			stmts:       agyBaselineCreateStatements,
			wantLabel:   "",
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, itoa(i)+".db")
			db := openSchemaDB(t, path, tt.userVersion, tt.stmts...)
			defer db.Close()

			fp, err := antigravitySchemaFingerprint(db)
			require.NoError(t, err)
			label := antigravitySchemaLabel(fp)

			if tt.wantLabel != "" {
				assert.Equal(t, tt.wantLabel, label)
			} else {
				assert.True(t,
					strings.HasPrefix(label, antigravitySchemaUnknownPrefix),
					"mutated schema should produce unknown marker, got %q",
					label)
				assert.NotEqual(t, agyBaselineFullFingerprint, fp,
					"mutated schema must not reuse baseline fingerprint")
			}
		})
	}
}

// TestAntigravitySchemaFingerprintDeterministic asserts the same schema always
// produces the same fingerprint across independent databases.
func TestAntigravitySchemaFingerprintDeterministic(t *testing.T) {
	dir := t.TempDir()
	stmts := append([]string{}, agyBaselineCreateStatements...)

	db1 := openSchemaDB(t, filepath.Join(dir, "a.db"), 1, stmts...)
	defer db1.Close()
	db2 := openSchemaDB(t, filepath.Join(dir, "b.db"), 1, stmts...)
	defer db2.Close()

	fp1, err := antigravitySchemaFingerprint(db1)
	require.NoError(t, err)
	fp2, err := antigravitySchemaFingerprint(db2)
	require.NoError(t, err)
	assert.Equal(t, fp1, fp2, "identical schema must hash identically")
}

// TestAntigravityIDESourceVersionBaseline verifies the IDE parser stamps the
// schema-fingerprint label when the .db matches the baseline schema.
func TestAntigravityIDESourceVersionBaseline(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-1111-2222-3333-444444444444"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityBaselineSchemaDB(t, dbPath)

	sess, _, _, err := parseAntigravityTestSession(t, dbPath, "/tmp/proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "1.0.7-1.0.10", sess.SourceVersion)
}

// TestAntigravityCLISourceVersionBaseline verifies the CLI .db parse path
// stamps the same label as the IDE path for the baseline schema.
func TestAntigravityCLISourceVersionBaseline(t *testing.T) {
	root := t.TempDir()
	id := "bbbbbbbb-1111-2222-3333-444444444444"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityBaselineSchemaDB(t, dbPath)

	sess, _, err := parseAntigravityCLITestSession(t, dbPath, "/tmp/proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "1.0.7-1.0.10", sess.SourceVersion)
}

// TestAntigravityIDESourceVersionUnknownSchema verifies that a .db with a
// non-baseline schema (the minimal 2-table test schema, no user_version)
// gets an agy-schema:<hex> marker rather than a fabricated release.
func TestAntigravityIDESourceVersionUnknownSchema(t *testing.T) {
	root := t.TempDir()
	id := "cccccccc-1111-2222-3333-444444444444"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath)

	sess, _, _, err := parseAntigravityTestSession(t, dbPath, "/tmp/proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.True(t,
		strings.HasPrefix(sess.SourceVersion, antigravitySchemaUnknownPrefix),
		"non-baseline schema should yield unknown marker, got %q",
		sess.SourceVersion)
}

// TestAntigravityCLIDBStepsCarriesSourceVersionOnStepError verifies that a CLI
// .db whose schema is readable but whose steps query fails still returns the
// schema-fingerprint label. Without this the parser falls back to the
// trajectory sidecar and persists a session with an empty SourceVersion,
// silently losing the agy-schema marker for an unrecognized (newer) schema.
func TestAntigravityCLIDBStepsCarriesSourceVersionOnStepError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nosteps.db")
	// A readable schema with NO steps table: the fingerprint query over
	// sqlite_master succeeds, but loadAntigravityStepsWithRawCount's
	// `SELECT ... FROM steps` fails. The lone table also makes the schema
	// non-baseline, so the expected label is an agy-schema:<prefix> marker.
	db := openSchemaDB(t, path, 1,
		"CREATE TABLE `trajectory_meta` (`trajectory_id` text,"+
			"PRIMARY KEY (`trajectory_id`))")
	require.NoError(t, db.Close())

	result, err := loadAntigravityCLIDBSteps(path)
	require.Error(t, err, "missing steps table must fail the step query")
	assert.True(t,
		strings.HasPrefix(result.sourceVersion, antigravitySchemaUnknownPrefix),
		"readable schema must still yield an agy-schema marker, got %q",
		result.sourceVersion)
}

// TestAntigravityCLIPBSourceVersionEmpty verifies that a legacy .pb session
// (no .db schema available) leaves SourceVersion empty rather than fabricating.
func TestAntigravityCLIPBSourceVersionEmpty(t *testing.T) {
	root := t.TempDir()
	id := "dddddddd-1111-2222-3333-444444444444"
	mustMkdir(t, filepath.Join(root, "conversations"))

	pbPath := filepath.Join(root, "conversations", id+".pb")
	mustWrite(t, pbPath, []byte("encrypted-placeholder"))
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"pb prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/pb-proj","conversationId":"`+id+`"}`))

	sess, _, err := parseAntigravityCLITestSession(t, pbPath, "/tmp/pb-proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Empty(t, sess.SourceVersion,
		"sidecar-only/.pb session must not fabricate SourceVersion")
}
