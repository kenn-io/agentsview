// ABOUTME: tests for the parse-diff CLI command: flag validation,
// ABOUTME: empty-archive runs, JSON shape, and the text renderer.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// isolateParseDiffEnv points the data dir, HOME, and every per-agent
// directory override at empty temp dirs so end-to-end runs never
// discover the developer machine's real session files.
func isolateParseDiffEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, def := range parser.Registry {
		if def.EnvVar != "" {
			t.Setenv(def.EnvVar,
				filepath.Join(home, "agent-dirs", string(def.Type)))
		}
	}
}

func TestParseDiff_RegisteredInRootHelp(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err, "Execute")
	assert.Contains(t, help, "parse-diff",
		"root help should list the parse-diff command")
}

func TestParseDiff_UnknownAgentListsSupported(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())

	_, err := executeCommand(newRootCommand(),
		"parse-diff", "--agent", "definitely-not-an-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		`unknown agent "definitely-not-an-agent"`)
	for _, want := range []string{"claude", "codex", "gemini"} {
		assert.Contains(t, err.Error(), want,
			"error should list supported agent %q", want)
	}
	for _, unwanted := range []string{"forge", "piebald", "warp"} {
		assert.NotContains(t, err.Error(), unwanted,
			"error should not list unsupported agent %q", unwanted)
	}
}

func TestParseDiff_RejectsAgentsWithoutOnDiskSource(t *testing.T) {
	tests := []struct {
		name  string
		agent string
	}{
		{"database-backed forge", "forge"},
		{"database-backed piebald", "piebald"},
		{"database-backed warp", "warp"},
		{"import-only claude-ai", "claude-ai"},
		{"import-only chatgpt", "chatgpt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())

			_, err := executeCommand(newRootCommand(),
				"parse-diff", "--agent", tc.agent)
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf(
				"agent %q is not supported by parse-diff "+
					"(no on-disk source to re-parse)", tc.agent))
		})
	}
}

func TestParseDiff_RejectsNegativeLimit(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())

	_, err := executeCommand(newRootCommand(),
		"parse-diff", "--limit", "-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--limit must be >= 0")
}

func TestParseDiffAgentTypes(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{name: "empty means all", in: nil, want: nil},
		{
			name: "single agent",
			in:   []string{"claude"},
			want: []string{"claude"},
		},
		{
			name: "trims and lowercases",
			in:   []string{" Claude "},
			want: []string{"claude"},
		},
		{
			name: "dedupes preserving order",
			in:   []string{"codex", "claude", "codex"},
			want: []string{"codex", "claude"},
		},
		{
			name:    "unknown agent",
			in:      []string{"nope"},
			wantErr: `unknown agent "nope"`,
		},
		{
			name:    "db-backed agent",
			in:      []string{"forge"},
			wantErr: "no on-disk source to re-parse",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDiffAgentTypes(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			strs := make([]string, 0, len(got))
			for _, a := range got {
				strs = append(strs, string(a))
			}
			if tc.want == nil {
				assert.Empty(t, strs)
				return
			}
			assert.Equal(t, tc.want, strs)
		})
	}
}

func TestParseDiff_EmptyArchiveRunsClean(t *testing.T) {
	isolateParseDiffEnv(t)

	out, err := executeCommand(newRootCommand(), "parse-diff")
	require.NoError(t, err)
	assert.Contains(t, out, "Parse diff: 0 files re-parsed (all agents)")
	assert.Contains(t, out, "Summary")
	assert.Contains(t, out, "Examined")
	assert.Contains(t, out, "0 sessions changed, 0 identical.")
	assert.NotContains(t, out, "Changed fields")
	assert.NotContains(t, out, "Changed sessions")
}

func TestParseDiff_JSONShape(t *testing.T) {
	isolateParseDiffEnv(t)

	out, err := executeCommand(newRootCommand(), "parse-diff", "--json")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	for _, key := range []string{
		"generated_at", "data_version", "agents",
		"files_examined", "files_limited",
		"totals", "field_counts", "sessions",
	} {
		assert.Contains(t, got, key,
			"JSON report missing top-level key %q", key)
	}
	assert.NotEmpty(t, got["generated_at"])
}

func TestDoParseDiff_FailOnChangeFalseOnEmptyArchive(t *testing.T) {
	isolateParseDiffEnv(t)

	var buf bytes.Buffer
	failed := doParseDiff(ParseDiffConfig{
		FailOnChange: true,
		Stdout:       &buf,
		Stderr:       &buf,
	})
	assert.False(t, failed,
		"empty archive must not trip --fail-on-change")
	assert.Contains(t, buf.String(),
		"0 sessions changed, 0 identical.")
}

// TestParseDiffExitFailure pins the --fail-on-change exit contract,
// including the rule that a vacuous run (data version ahead of the whole
// archive) is a gate failure, not a passing vet, with an explanation on
// stderr that also reaches --json callers.
func TestParseDiffExitFailure(t *testing.T) {
	tests := []struct {
		name         string
		totals       sync.ParseDiffTotals
		failOnChange bool
		wantFail     bool
		wantStderr   bool
	}{
		{
			name:         "flag off never fails",
			totals:       sync.ParseDiffTotals{Examined: 5, PendingResync: 5},
			failOnChange: false,
		},
		{
			name:         "identical passes",
			totals:       sync.ParseDiffTotals{Examined: 5, Identical: 5},
			failOnChange: true,
		},
		{
			name:         "real change fails without stderr note",
			totals:       sync.ParseDiffTotals{Examined: 5, Identical: 4, Changed: 1},
			failOnChange: true,
			wantFail:     true,
		},
		{
			name:         "parse error fails without stderr note",
			totals:       sync.ParseDiffTotals{Examined: 1, Identical: 1, ParseErrors: 1},
			failOnChange: true,
			wantFail:     true,
		},
		{
			name:         "vacuous run fails with stderr note",
			totals:       sync.ParseDiffTotals{Examined: 5, PendingResync: 5},
			failOnChange: true,
			wantFail:     true,
			wantStderr:   true,
		},
		{
			name: "partial pending resync is not vacuous",
			totals: sync.ParseDiffTotals{
				Examined: 5, Identical: 1, PendingResync: 4,
			},
			failOnChange: true,
		},
		{
			name:         "no examined sessions is not vacuous",
			totals:       sync.ParseDiffTotals{},
			failOnChange: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &sync.ParseDiffReport{Totals: tc.totals}
			var stderr bytes.Buffer
			got := parseDiffExitFailure(r, tc.failOnChange, &stderr)
			assert.Equal(t, tc.wantFail, got, "exit failure")
			if tc.wantStderr {
				assert.Contains(t, stderr.String(),
					"--fail-on-change failed: the run was vacuous",
					"vacuous run must explain the non-zero exit on stderr")
			} else {
				assert.Empty(t, stderr.String(),
					"only a vacuous run writes to stderr")
			}
		})
	}
}

// changedSession builds a DiffChanged SessionDiff fixture.
func changedSession(
	agent, id string, fields ...sync.FieldDiff,
) sync.SessionDiff {
	return sync.SessionDiff{
		SessionID: id,
		Agent:     agent,
		Class:     sync.DiffChanged,
		Fields:    fields,
	}
}

func TestRenderParseDiffReport_ChangedSessions(t *testing.T) {
	r := &sync.ParseDiffReport{
		GeneratedAt:   "2026-06-12T00:00:00Z",
		DataVersion:   42,
		Agents:        []string{"claude", "codex"},
		FilesExamined: 7,
		Totals: sync.ParseDiffTotals{
			Examined:  5,
			Identical: 3,
			Changed:   2,
		},
		FieldCounts: map[string]int{
			sync.FieldMessageCount: 2,
			sync.FieldModels:       1,
		},
		Sessions: []sync.SessionDiff{
			changedSession("claude", "abcdef1234567890",
				sync.FieldDiff{
					Field:  sync.FieldMessageCount,
					Stored: "10",
					Parsed: "11",
				},
				sync.FieldDiff{
					Field:  sync.FieldModels,
					Stored: "opus",
					Parsed: "opus, sonnet",
				},
			),
			changedSession("codex", "ffff00001111",
				sync.FieldDiff{
					Field:  sync.FieldMessageCount,
					Stored: "4",
					Parsed: "5",
				},
			),
			{
				SessionID: "skip-me",
				Agent:     "claude",
				Class:     sync.DiffSkipped,
				Reason:    "source missing",
			},
		},
	}

	var buf bytes.Buffer
	renderParseDiffReport(&buf, r, "/tmp/sessions.db", "claude, codex", false)
	out := buf.String()

	assert.Contains(t, out,
		"Parse diff: 7 files re-parsed (claude, codex) "+
			"against /tmp/sessions.db (data version 42)")
	assert.Contains(t, out, "Changed fields (sessions affected)")
	assert.Contains(t, out, "Changed sessions")
	assert.Contains(t, out, "abcdef12",
		"changed session should be listed by short id")
	assert.Contains(t, out, "message_count, models")
	assert.Contains(t, out, "2 sessions changed, 3 identical.")
	assert.NotContains(t, out, "skip-me",
		"skipped sessions must not appear in the changed list")
	assert.NotContains(t, out, "more; use --verbose",
		"no cap notice when under the cap")
}

func TestRenderParseDiffReport_FieldCountOrdering(t *testing.T) {
	r := &sync.ParseDiffReport{
		FieldCounts: map[string]int{
			"alpha_field": 2,
			"beta_field":  2,
			"big_field":   9,
		},
		Totals: sync.ParseDiffTotals{Changed: 9, Examined: 9},
	}

	var buf bytes.Buffer
	renderParseDiffReport(&buf, r, "db", "all agents", false)
	out := buf.String()

	big := strings.Index(out, "big_field")
	alpha := strings.Index(out, "alpha_field")
	beta := strings.Index(out, "beta_field")
	require.NotEqual(t, -1, big)
	require.NotEqual(t, -1, alpha)
	require.NotEqual(t, -1, beta)
	assert.Less(t, big, alpha,
		"highest count must sort first")
	assert.Less(t, alpha, beta,
		"ties must break alphabetically")
}

func TestRenderParseDiffReport_CapAndVerbose(t *testing.T) {
	// IDs stay at 7 chars so shortID does not truncate them and
	// each compact line keeps a distinguishable id.
	var sessions []sync.SessionDiff
	for i := range parseDiffChangedCap + 5 {
		sessions = append(sessions, changedSession(
			"claude",
			fmt.Sprintf("sess-%02d", i),
			sync.FieldDiff{
				Field:  sync.FieldFirstMessage,
				Stored: "old",
				Parsed: "new",
				Detail: "lengths 3 vs 3",
			},
			sync.FieldDiff{
				Field:         sync.FieldTerminationStatus,
				Stored:        "completed",
				Parsed:        "(null)",
				Informational: true,
			},
		))
	}
	r := &sync.ParseDiffReport{
		Totals: sync.ParseDiffTotals{
			Examined: len(sessions),
			Changed:  len(sessions),
		},
		Sessions: sessions,
	}

	t.Run("capped", func(t *testing.T) {
		var buf bytes.Buffer
		renderParseDiffReport(&buf, r, "db", "all agents", false)
		out := buf.String()

		assert.Contains(t, out,
			"... (5 more; use --verbose or --json)")
		assert.Contains(t, out, "sess-00")
		assert.Contains(t, out,
			fmt.Sprintf("sess-%02d", parseDiffChangedCap-1))
		assert.NotContains(t, out,
			fmt.Sprintf("sess-%02d", parseDiffChangedCap),
			"sessions past the cap must be elided")
		assert.NotContains(t, out, "[informational]",
			"compact lines summarize non-informational fields only")
	})

	t.Run("verbose", func(t *testing.T) {
		var buf bytes.Buffer
		renderParseDiffReport(&buf, r, "db", "all agents", true)
		out := buf.String()

		assert.NotContains(t, out, "more; use --verbose",
			"verbose output is never capped")
		assert.Contains(t, out,
			fmt.Sprintf("sess-%02d", parseDiffChangedCap+4),
			"verbose lists every changed session")
		assert.Contains(t, out,
			"first_message: old -> new (lengths 3 vs 3)")
		assert.Contains(t, out,
			"termination_status: completed -> (null) [informational]")
	})
}

func TestRenderParseDiffReport_EmptyReport(t *testing.T) {
	r := &sync.ParseDiffReport{
		DataVersion: 7,
		FieldCounts: map[string]int{},
	}

	var buf bytes.Buffer
	require.NotPanics(t, func() {
		renderParseDiffReport(&buf, r, "/data/sessions.db", "all agents", false)
	})
	out := buf.String()

	assert.Contains(t, out,
		"Parse diff: 0 files re-parsed (all agents) "+
			"against /data/sessions.db (data version 7)")
	assert.Contains(t, out, "Examined")
	assert.NotContains(t, out, "Changed fields")
	assert.NotContains(t, out, "Changed sessions")
	assert.Contains(t, out, "0 sessions changed, 0 identical.")
}

func TestRenderParseDiffReport_NonZeroTotalsOnly(t *testing.T) {
	r := &sync.ParseDiffReport{
		FilesLimited: true,
		Totals: sync.ParseDiffTotals{
			Examined:    4,
			Identical:   3,
			Changed:     1,
			ParseErrors: 2,
		},
		FieldCounts: map[string]int{},
	}

	var buf bytes.Buffer
	renderParseDiffReport(&buf, r, "db", "all agents", false)
	out := buf.String()

	for _, want := range []string{
		"Examined", "Identical", "Changed", "Parse errors",
		"--limit truncated discovery",
	} {
		assert.Contains(t, out, want)
	}
	for _, unwanted := range []string{
		"Pending resync", "New on disk", "Skipped",
		"Excluded by parser", "Needs retry", "Informational only",
	} {
		assert.NotContains(t, out, unwanted,
			"zero totals must not render a summary line")
	}
}

func TestRenderParseDiffReport_ParseErrorsListed(t *testing.T) {
	r := &sync.ParseDiffReport{
		DataVersion: 39,
		Totals:      sync.ParseDiffTotals{ParseErrors: 2},
		FieldCounts: map[string]int{},
		Sessions: []sync.SessionDiff{
			{
				SessionID: "broken-1", Agent: "claude",
				FilePath: "/data/proj/broken-1.jsonl",
				Class:    sync.DiffParseError,
				Reason:   "unexpected end of JSON input",
			},
			{
				Agent:    "gemini",
				FilePath: "/data/proj/headless.json",
				Class:    sync.DiffParseError,
				Reason:   "invalid character '}'",
			},
		},
	}

	var buf bytes.Buffer
	renderParseDiffReport(&buf, r, "db", "all agents", false)
	out := buf.String()

	assert.Contains(t, out, "Parse errors")
	assert.Contains(t, out, "/data/proj/broken-1.jsonl",
		"parse-error file path must be shown, not just the count")
	assert.Contains(t, out, "unexpected end of JSON input",
		"parse-error reason must be shown")
	assert.Contains(t, out, "/data/proj/headless.json")
	assert.Contains(t, out, "invalid character")
}

// TestRenderParseDiffReport_SanitizesControlSequences proves the
// human renderer strips terminal control bytes from every
// session-derived value (agents, IDs, paths, field values, details,
// parse-error reasons). Session files control these strings, so
// without sanitization a crafted session could emit OSC 52 clipboard
// writes, OSC 8 phishing hyperlinks, or cursor movement on a plain
// `parse-diff --verbose` run.
func TestRenderParseDiffReport_SanitizesControlSequences(t *testing.T) {
	r := &sync.ParseDiffReport{
		DataVersion: 42,
		Totals: sync.ParseDiffTotals{
			Examined: 1, Changed: 1, ParseErrors: 1,
		},
		FieldCounts: map[string]int{
			sync.FieldFirstMessage + "\x1b[31m": 1,
		},
		Sessions: []sync.SessionDiff{
			changedSession(
				"claude\x1b[31m",
				"evil\x1b]0;title\x07session",
				sync.FieldDiff{
					Field:  sync.FieldFirstMessage,
					Stored: "safe\x1b]52;c;ZXZpbA==\x07clip",
					Parsed: "new\x1b[2K\rvalue",
					Detail: "detail\x1b[1;31mred",
				},
			),
			{
				Agent:    "gemini",
				FilePath: "/data/evil\x1b[8mhidden.jsonl",
				Class:    sync.DiffParseError,
				Reason:   "bad\x1b]8;;https://evil.example\x07link",
			},
		},
	}

	for _, verbose := range []bool{false, true} {
		var buf bytes.Buffer
		renderParseDiffReport(&buf, r,
			"/tmp/db\x1b[5m", "all agents", verbose)
		out := buf.String()

		assert.NotContains(t, out, "\x1b",
			"verbose=%v output must contain no ESC bytes", verbose)
		assert.NotContains(t, out, "\x07",
			"verbose=%v output must contain no BEL bytes", verbose)
		assert.NotContains(t, out, "\r",
			"verbose=%v output must contain no carriage returns", verbose)
		// The text around the stripped sequences must survive.
		assert.Contains(t, out, "link", "reason text retained")
		assert.Contains(t, out, "hidden.jsonl", "path text retained")
	}

	var verbose bytes.Buffer
	renderParseDiffReport(&verbose, r, "db", "all agents", true)
	out := verbose.String()
	assert.Contains(t, out, "clip", "stored value text retained")
	assert.Contains(t, out, "value", "parsed value text retained")
	assert.Contains(t, out, "red", "detail text retained")
}

func TestRenderParseDiffReport_VacuousResyncWarning(t *testing.T) {
	r := &sync.ParseDiffReport{
		DataVersion: 40,
		Totals: sync.ParseDiffTotals{
			Examined: 5, PendingResync: 5,
		},
		FieldCounts: map[string]int{},
	}

	var buf bytes.Buffer
	renderParseDiffReport(&buf, r, "db", "all agents", false)
	out := buf.String()
	assert.Contains(t, out, "Warning:")
	assert.Contains(t, out, "data version is ahead",
		"vacuous run must warn that no drift can be detected")

	// A run with at least one comparable session is not vacuous.
	r.Totals = sync.ParseDiffTotals{Examined: 5, Identical: 1, PendingResync: 4}
	var buf2 bytes.Buffer
	renderParseDiffReport(&buf2, r, "db", "all agents", false)
	assert.NotContains(t, buf2.String(), "Warning:",
		"a run with comparable sessions must not warn")
}

func TestRenderParseDiffReport_PendingResyncVerbose(t *testing.T) {
	r := &sync.ParseDiffReport{
		DataVersion: 40,
		Totals:      sync.ParseDiffTotals{Examined: 1, PendingResync: 1},
		FieldCounts: map[string]int{},
		Sessions: []sync.SessionDiff{{
			SessionID:         "stale-1",
			Agent:             "claude",
			Class:             sync.DiffPendingResync,
			StoredDataVersion: 38,
			Fields: []sync.FieldDiff{{
				Field: sync.FieldFirstMessage, Stored: "old", Parsed: "new",
			}},
		}},
	}

	var plain bytes.Buffer
	renderParseDiffReport(&plain, r, "db", "all agents", false)
	assert.NotContains(t, plain.String(), "stale-1",
		"pending-resync drill-down is verbose-only")

	var verbose bytes.Buffer
	renderParseDiffReport(&verbose, r, "db", "all agents", true)
	out := verbose.String()
	assert.Contains(t, out, "Pending-resync sessions")
	assert.Contains(t, out, "stale-1")
	assert.Contains(t, out, "first_message: old -> new")
}

func TestParseDiff_JSONSessionsAndDBPath(t *testing.T) {
	isolateParseDiffEnv(t)

	out, err := executeCommand(newRootCommand(), "parse-diff", "--json")
	require.NoError(t, err)

	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	// A clean run must serialize an empty array, never null, so jq
	// pipelines and typed consumers do not break.
	require.Contains(t, got, "sessions")
	assert.Equal(t, "[]", strings.TrimSpace(string(got["sessions"])),
		"clean run must emit sessions: [] not null")
	// The archive identity must be present so the report is
	// self-describing when attached to a PR.
	require.Contains(t, got, "db_path")
	var dbPath string
	require.NoError(t, json.Unmarshal(got["db_path"], &dbPath))
	assert.NotEmpty(t, dbPath, "db_path must identify the vetted archive")
}

// TestDoParseDiff_FailOnChangeDirections exercises both directions of
// the exit-code conjunction (cfg.FailOnChange && report.HasFailures()).
// A stored session at the current data version whose source file no
// longer emits it is a presence change, so HasFailures() is true; the
// flag then decides the exit. Staging it via a valid source file plus a
// phantom stored row keeps the test independent of the parser's session
// ID derivation.
func TestDoParseDiff_FailOnChangeDirections(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("HOME", t.TempDir())
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	// A valid Claude source file so discovery and parse succeed.
	projDir := filepath.Join(claudeDir, "-home-proj")
	require.NoError(t, os.MkdirAll(projDir, 0o755))
	srcPath := filepath.Join(projDir, "real-session.jsonl")
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "hello").
		AddClaudeAssistant("2026-01-01T00:00:01Z", "hi").
		String()
	require.NoError(t, os.WriteFile(srcPath, []byte(content), 0o644))

	// A phantom stored row under that file at the current data version:
	// the re-parse never emits this id, so it reports as a presence
	// change (HasFailures() == true).
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(db.Session{
		ID: "phantom-session", Project: "proj", Machine: "m",
		Agent: "claude", MessageCount: 4, UserMessageCount: 2,
		FilePath: &srcPath,
	}))
	require.NoError(t,
		d.SetSessionDataVersion("phantom-session", db.CurrentDataVersion()))
	require.NoError(t, d.Close())

	var failBuf bytes.Buffer
	failed := doParseDiff(ParseDiffConfig{
		FailOnChange: true, Stdout: &failBuf, Stderr: &failBuf,
	})
	assert.True(t, failed,
		"a presence change with --fail-on-change must fail")
	assert.Contains(t, failBuf.String(), "sessions changed")

	var cleanBuf bytes.Buffer
	notFailed := doParseDiff(ParseDiffConfig{
		FailOnChange: false, Stdout: &cleanBuf, Stderr: &cleanBuf,
	})
	assert.False(t, notFailed,
		"without --fail-on-change the same drift must not fail")
}
