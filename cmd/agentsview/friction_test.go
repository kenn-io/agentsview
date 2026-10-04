package main

import (
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

// seedGoldenDigest stores the PR 4 golden summary and Markdown as the
// digest for 2026-05-10, the date in jilog's review-nightly.json fixture.
func seedGoldenDigest(t *testing.T, dataDir string) *db.DB {
	t.Helper()
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	snapJSON, err := review.EncodeSnapshot(friction.DigestSnapshot{Date: "2026-05-10", Timezone: "UTC", RulesVersion: friction.RulesVersion})
	require.NoError(t, err)
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: "2026-05-10", Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 5, 11, 1, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 3,
		SnapshotJSON: snapJSON, SummaryJSON: readFrictionGolden(t, "summary.json"),
		Markdown: readFrictionGolden(t, "friction-log.md"), MarkdownSHA256: "s", RunID: "r",
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
	for _, name := range []string{"server", "server-token-file"} {
		assert.NotNil(t, cmd.PersistentFlags().Lookup(name), name)
	}
}

func TestFrictionDigestJSONMatchesGolden(t *testing.T) {
	golden := readFrictionGolden(t, "summary.json")
	goldenMD := readFrictionGolden(t, "friction-log.md")
	tests := []struct {
		name   string
		daemon bool
	}{{"local_without_daemon", false}, {"through_daemon", true}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := testDataDir(t)
			t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
			d := seedGoldenDigest(t, dataDir)
			args := []string{"friction", "digest", "--date", "2026-05-10"}
			if tt.daemon {
				ts := localServerProxy(t, d, dataDir)
				args = append(args, "--server", ts.URL)
			} else {
				require.NoError(t, d.Close())
			}
			stdout, stderr, err := runInsightCommand(t, append(args, "--format", "json")...)
			require.NoError(t, err, stderr)
			assert.Equal(t, string(golden), stdout, "byte-identical to the golden")
			alias, stderr, err := runInsightCommand(t, append(args, "--json")...)
			require.NoError(t, err, stderr)
			assert.Equal(t, stdout, alias, "--json aliases --format json")

			stdout, _, err = runInsightCommand(t, args...)
			require.NoError(t, err)
			assert.Equal(t, string(goldenMD), stdout, "default format is the stored Markdown")

			stdout, _, err = runInsightCommand(t, "friction", "digest", "--format", "json")
			if tt.daemon {
				stdout, _, err = runInsightCommand(t, "friction", "digest", "--format", "json", "--server", args[len(args)-1])
			}
			require.NoError(t, err)
			assert.Equal(t, string(golden), stdout, "no --date means the latest digest")
		})
	}
}

func TestFrictionDigestErrors(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	require.NoError(t, dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db")).Close())
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
			stdout, _, err := runInsightCommand(t, tt.args...)
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

	// On by default (D34): no config needed.
	stdout, _, err := runInsightCommand(t, "friction", "run", "--date", "2026-09-14", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, stdout, "0 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns")
	assert.Contains(t, stdout, "0 session(s) scanned")
	assert.NotContains(t, stdout, "Digest:", "dry run omits the digest line (jilog review.rs:180-182)")

	stdout, _, err = runInsightCommand(t, "friction", "run", "--date", "2026-09-14", "--json")
	require.NoError(t, err)
	assert.Contains(t, stdout, "\"schema_version\": 3")
	assert.Contains(t, stdout, "\"frustrations\": 0")
	assert.Contains(t, stdout, "\"interruptions\": 0")
	assert.Contains(t, stdout, "\"digest_path\": \"friction:2026-09-14\"")
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

func TestFrictionFindingsAndPatternsLocal(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, d, "claude:s1", "proj")
	require.NoError(t, d.ReplaceSessionFriction(t.Context(), "claude:s1", []db.FrictionFinding{
		{
			SessionID: "claude:s1", Kind: "error", Detector: "error", ToolName: "Bash", MessageOrdinal: new(3),
			Title: "[friction/error] Bash: boom", Fingerprint: "fl1:x", Seq: 0, RulesVersion: friction.RulesVersion,
		},
	}, nil, friction.RulesVersion, "h"))
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: "2026-09-14", Timezone: "UTC", RulesVersion: friction.RulesVersion, BuiltAt: time.Now().UTC(),
		Revision: 1, SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}\n"), Markdown: []byte("x"),
		MarkdownSHA256: "s", RunID: "r",
	}, []db.FrictionDigestSubject{{SubjectID: "claude:s1", Date: "2026-09-14", SubjectKind: "session"}},
		[]db.FrictionPatternUpdate{{Fingerprint: "fl1:x", Kind: "error", Title: "[friction/error] Bash: boom", Date: "2026-09-14", SubjectID: "claude:s1", Ordinal: new(3), Occurrences: 1}}))
	require.NoError(t, d.Close())

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"findings_table", []string{"friction", "findings", "--date", "2026-09-14"}, []string{"SESSION", "claude:s1", "error", "[friction/error] Bash: boom"}},
		{"findings_kind_filter_empty", []string{"friction", "findings", "--kind", "deferral"}, []string{"(no friction findings)"}},
		{"patterns_table", []string{"friction", "patterns"}, []string{"COUNT", "fl1:x", "2026-09-14"}},
		{"patterns_linked_empty", []string{"friction", "patterns", "--linked"}, []string{"(no friction patterns)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := runInsightCommand(t, tt.args...)
			require.NoError(t, err)
			for _, w := range tt.want {
				assert.Contains(t, stdout, w)
			}
		})
	}
	_, _, err := runInsightCommand(t, "friction", "patterns", "--linked", "--unlinked")
	require.Error(t, err)
}
