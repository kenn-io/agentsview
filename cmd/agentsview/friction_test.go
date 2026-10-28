package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/server"
)

func readFrictionGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "friction", "testdata", "golden", name))
	require.NoError(t, err)
	return b
}

// seedGoldenDigest stores the renderer's golden summary and Markdown as the
// digest for their date.
func seedGoldenDigest(t *testing.T, dataDir string) *db.DB {
	t.Helper()
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	snapJSON, err := review.EncodeSnapshot(review.Snapshot{Date: "2026-09-16", Timezone: "UTC", RulesVersion: friction.RulesVersion})
	require.NoError(t, err)
	md := readFrictionGolden(t, "friction-log.md")
	sum := sha256.Sum256(md)
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: "2026-09-16", Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 3,
		SnapshotJSON: snapJSON, SummaryJSON: readFrictionGolden(t, "summary.json"),
		Markdown: md, MarkdownSHA256: hex.EncodeToString(sum[:]), RunID: "r",
	}, nil, nil))
	return d
}

// localServerProxy serves the real API over httptest while presenting the
// Host/Origin the server was configured for.
func localServerProxy(t *testing.T, d *db.DB, dataDir string, opts ...server.Option) *httptest.Server {
	t.Helper()
	h := server.New(config.Config{Host: "127.0.0.1", DataDir: dataDir}, d, nil, opts...).Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "127.0.0.1:0"
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", "http://127.0.0.1:0")
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestNewFrictionCommand_RegistersSubcommands(t *testing.T) {
	root := newRootCommand()
	cmd, _, err := root.Find([]string{"friction"})
	require.NoError(t, err)
	assert.Equal(t, "friction", cmd.Name())
	for _, name := range []string{"run", "digest", "findings", "patterns"} {
		child, _, err := root.Find([]string{"friction", name})
		require.NoError(t, err)
		assert.Equal(t, name, child.Name())
	}
	for _, name := range []string{"server", "server-token-file", "pg"} {
		assert.NotNil(t, cmd.PersistentFlags().Lookup(name), name)
	}
}

func TestFrictionDigestJSONMatchesGolden(t *testing.T) {
	golden := readFrictionGolden(t, "summary.json")
	goldenMD := readFrictionGolden(t, "friction-log.md")
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	ts := localServerProxy(t, seedGoldenDigest(t, dataDir), dataDir)
	args := []string{"friction", "digest", "--server", ts.URL, "--date", "2026-09-16"}

	stdout, stderr, err := runInsightCommand(t, append(args, "--format", "json")...)
	require.NoError(t, err, stderr)
	assert.Equal(t, string(golden), stdout, "byte-identical to the stored summary")
	alias, stderr, err := runInsightCommand(t, append(args, "--json")...)
	require.NoError(t, err, stderr)
	assert.Equal(t, stdout, alias, "--json aliases --format json")

	stdout, _, err = runInsightCommand(t, args...)
	require.NoError(t, err)
	assert.Equal(t, string(goldenMD), stdout, "default format is the stored Markdown")

	stdout, _, err = runInsightCommand(t, "friction", "digest", "--server", ts.URL, "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, string(golden), stdout, "no --date means the latest digest")

	stdout, _, err = runInsightCommand(t, "friction", "digest", "--server", ts.URL, "--from", "2026-09-01", "--to", "2026-09-30")
	require.NoError(t, err)
	assert.Equal(t, string(goldenMD), stdout, "a range prints each digest")
}

func TestFrictionDigestErrors(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	ts := localServerProxy(t, dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db")), dataDir)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"none_yet", []string{"friction", "digest"}, "no friction digests have been built yet"},
		{"missing_date", []string{"friction", "digest", "--date", "2026-01-01"}, "no friction digest for 2026-01-01"},
		{"bad_format", []string{"friction", "digest", "--format", "yaml"}, "--format must be md or json"},
		{"date_and_range", []string{"friction", "digest", "--date", "2026-01-01", "--from", "2026-01-01"}, "--date cannot be combined with --from/--to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := runInsightCommand(t, append(tt.args, "--server", ts.URL)...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Empty(t, stdout)
		})
	}
}

func TestFrictionRunLocalWithoutDaemon(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	require.NoError(t, dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db")).Close())
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"),
		[]byte("[friction]\nenabled = true\n"), 0o600))

	stdout, _, err := runInsightCommand(t, "friction", "run", "--date", "2026-09-14", "--dry-run")
	require.NoError(t, err)
	assert.Equal(t, "2026-09-14: 0 session(s) scanned; 0 correction(s), 0 error(s), 0 workaround(s), "+
		"0 deferral(s), 0 pattern(s), 0 frustration(s), 0 interruption(s); 0 P0 alert(s) (not written)\n", stdout)

	stdout, _, err = runInsightCommand(t, "friction", "run", "--date", "2026-09-14", "--json")
	require.NoError(t, err)
	assert.Contains(t, stdout, "\"schema_version\": 1")
	assert.Contains(t, stdout, "\"date\": \"2026-09-14\"")
	assert.Contains(t, stdout, "\"interruptions\": 0")
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	stored, err := d.GetFrictionDigest(t.Context(), "2026-09-14")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, stdout, string(stored.SummaryJSON), "the run wrote the digest it reported")
	require.NoError(t, d.Close())
	formatted, _, err := runInsightCommand(t, "friction", "run", "--date", "2026-09-14", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, stdout, formatted)

	_, _, err = runInsightCommand(t, "friction", "run", "--date", "2999-01-01")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a complete local day")

	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"),
		[]byte("[friction]\nenabled = false\n"), 0o600))
	stdout, _, err = runInsightCommand(t, "friction", "run", "--date", "2026-09-13")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "friction review is disabled")
	assert.Empty(t, stdout)
}

func TestFrictionRunIgnoresUnrelatedInvalidSettings(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	require.NoError(t, dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db")).Close())
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"),
		[]byte("public_url = \"ftp://example.test\"\n\n[friction]\nenabled = true\n"), 0o600))
	_, _, err := runInsightCommand(t, "friction", "run", "--date", "2026-09-14", "--dry-run")
	require.NoError(t, err, "an invalid public_url is not this command's concern")
}

func TestFrictionRunThroughDaemon(t *testing.T) {
	dataDir := testDataDir(t)
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	fixed := time.Date(2026, 9, 16, 2, 0, 0, 0, time.UTC)
	runner := &review.Runner{Store: d, Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
	ts := localServerProxy(t, d, dataDir, server.WithFriction(runner, nil))

	stdout, _, err := runInsightCommand(t, "friction", "run", "--server", ts.URL, "--date", "2026-09-14", "--json")
	require.NoError(t, err)
	out, err := friction.CanonicalSummaryJSON([]byte(stdout))
	require.NoError(t, err)
	assert.Equal(t, string(out), stdout, "daemon run output is canonical summary bytes")

	stdout, _, err = runInsightCommand(t, "friction", "findings", "--server", ts.URL, "--format", "json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"findings":[],"next_cursor":""}`, stdout)
	alias, _, err := runInsightCommand(t, "friction", "findings", "--server", ts.URL, "--json")
	require.NoError(t, err)
	assert.JSONEq(t, stdout, alias)

	stdout, _, err = runInsightCommand(t, "friction", "patterns", "--server", ts.URL)
	require.NoError(t, err)
	assert.Equal(t, "(no friction patterns)\n", stdout)
	jsonPatterns, _, err := runInsightCommand(t, "friction", "patterns", "--server", ts.URL, "--json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"patterns":[],"next_cursor":""}`, jsonPatterns)
}

func TestFrictionFindingsAndPatterns(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, d, "claude:s1", "proj")
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "claude:s1", db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{RulesVersion: friction.RulesVersion, Hash: "h", Findings: []db.FrictionFinding{{
			SessionID: "claude:s1", Kind: "error", Detector: "error", ToolName: "Bash", MessageOrdinal: new(3),
			Title: "[friction/error] Bash: boom", Fingerprint: "fl1:x", Seq: 0, RulesVersion: friction.RulesVersion,
		}}},
	}))
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: "2026-09-14", Timezone: "UTC", RulesVersion: friction.RulesVersion, BuiltAt: time.Now().UTC(),
		Revision: 1, SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}\n"), Markdown: []byte("x"),
		MarkdownSHA256: "s", RunID: "r",
	}, []db.FrictionDigestSubject{{SubjectID: "claude:s1", Date: "2026-09-14", SubjectKind: "session"}},
		[]db.FrictionPatternUpdate{{Fingerprint: "fl1:x", Kind: "error", Title: "[friction/error] Bash: boom", Date: "2026-09-14", SubjectID: "claude:s1", Ordinal: new(3), Occurrences: 1}}))
	ts := localServerProxy(t, d, dataDir)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"findings_table", []string{"friction", "findings", "--date", "2026-09-14"}, []string{"SESSION", "claude:s1", "error", "[friction/error] Bash: boom"}},
		{"findings_kind_filter_empty", []string{"friction", "findings", "--kind", "deferral"}, []string{"(no friction findings)"}},
		{"patterns_table", []string{"friction", "patterns"}, []string{"COUNT", "fl1:x", "2026-09-14"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := runInsightCommand(t, append(tt.args, "--server", ts.URL)...)
			require.NoError(t, err)
			for _, w := range tt.want {
				assert.Contains(t, stdout, w)
			}
		})
	}
}
