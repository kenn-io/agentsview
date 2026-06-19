package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSessionHelp_ShowsSubcommands(t *testing.T) {
	t.Parallel()
	cmd := newRootCommand()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"session", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	help := buf.String()
	for _, name := range []string{
		"get", "usage", "list", "messages", "tool-calls",
		"export", "sync", "watch",
	} {
		assert.Contains(t, help, name,
			"expected subcommand %q in help", name)
	}
	assert.Contains(t, help, "--format",
		"expected --format persistent flag in help")
	assert.Contains(t, help, "--pg",
		"expected --pg persistent flag in help")
}

// seedSession opens the SQLite DB at dataDir/sessions.db, inserts
// one session with the given id+project (plus sane defaults), and
// closes the DB. Each subtest gets its own dataDir so parallel
// runs don't step on each other.
func seedSession(t *testing.T, dataDir, id, project string) {
	t.Helper()
	seedSessionWithOpts(t, dataDir, id, project, nil)
}

// seedSessionWithOpts is like seedSession but allows mutation of
// the db.Session before insert via the optional mut callback.
// Use this when a test needs to set signal counts or other
// non-default fields (e.g. ToolFailureSignalCount = 0 to
// exercise the --min-tool-failures flag's *int handling).
func seedSessionWithOpts(
	t *testing.T, dataDir, id, project string,
	mut func(*db.Session),
) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	// UserMessageCount >= 2 so seeded sessions pass the default
	// ExcludeOneShot filter in `session list` (one-shot means
	// user_message_count <= 1). See internal/db/analytics.go.
	s := db.Session{
		ID:               id,
		Project:          project,
		Machine:          "m",
		Agent:            "claude",
		MessageCount:     4,
		UserMessageCount: 2,
	}
	if mut != nil {
		mut(&s)
	}
	require.NoError(t, d.UpsertSession(s))
	require.NoError(t, d.Close())
}

func TestSessionGet_JSON(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-1", "proj")

	out, err := executeCommand(newRootCommand(),
		"session", "get", "s-1", "--format", "json")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, "s-1", got["id"])
	assert.Equal(t, "proj", got["project"])
}

func TestSessionGet_JSONAlias(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-json", "proj")

	out, err := executeCommand(newRootCommand(),
		"session", "get", "s-json", "--json")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, "s-json", got["id"])
	assert.Equal(t, "proj", got["project"])
}

func TestSessionGet_NotFound(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(),
		"session", "get", "missing", "--format", "json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
	assert.Contains(t, err.Error(), "not found")
}

func TestSessionGet_Human(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-2", "proj")

	out, err := executeCommand(newRootCommand(),
		"session", "get", "s-2")
	require.NoError(t, err)
	assert.True(t, strings.Contains(out, "s-2"),
		"human output should contain session id, got: %q", out)
	assert.True(t, strings.Contains(out, "proj"),
		"human output should contain project, got: %q", out)
}

// TestSessionGet_BareIDFindsPrefixed covers the case where a user
// passes a bare UUID (e.g. copied from a Codex session file name)
// for a session whose stored ID carries an agent prefix. The CLI
// retries the lookup with each registered IDPrefix.
func TestSessionGet_BareIDFindsPrefixed(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	bareID := "019da6a6-8c67-7c23-b102-ef48502852d0"
	seedSessionWithOpts(t, dataDir, "codex:"+bareID, "proj",
		func(s *db.Session) { s.Agent = "codex" })

	out, err := executeCommand(newRootCommand(),
		"session", "get", bareID, "--format", "json")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, "codex:"+bareID, got["id"])
}

func TestSessionList_JSONShape(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-a", "proj")
	seedSession(t, dataDir, "s-b", "proj")

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Sessions   []map[string]any `json:"sessions"`
		NextCursor string           `json:"next_cursor"`
		Total      int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 2, got.Total)
	assert.Len(t, got.Sessions, 2)
}

func TestSessionList_FilterByProject(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-a", "p1")
	seedSession(t, dataDir, "s-b", "p2")

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--project", "p1", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Sessions []map[string]any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "s-a", got.Sessions[0]["id"])
}

func TestSessionList_ServerFlagUsesHTTP(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "local-session", "local")

	var gotPath, gotProject string
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotProject = r.URL.Query().Get("project")
			assert.Equal(t, http.MethodGet, r.Method)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"sessions": [
					{"id":"remote-session","project":"remote","agent":"claude"}
				],
				"total": 1
			}`))
		}))
	defer ts.Close()

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--project", "remote",
		"--json")
	require.NoError(t, err)

	var got struct {
		Sessions []map[string]any `json:"sessions"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, "/api/v1/sessions", gotPath)
	assert.Equal(t, "remote", gotProject)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "remote-session", got.Sessions[0]["id"])
}

func TestSessionList_ServerFlagDoesNotSendConfigAuthToken(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "")
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`auth_token = "local-secret"`),
		0o600,
	))

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
		}))
	defer ts.Close()

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--json")
	require.NoError(t, err)
}

func TestSessionList_ServerFlagDoesNotLoadLocalConfig(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`not = valid = toml`),
		0o600,
	))

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
		}))
	defer ts.Close()

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--json")
	require.NoError(t, err)
}

func TestSessionList_ServerTokenSendsBearer(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "remote-secret")

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer remote-secret",
				r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
		}))
	defer ts.Close()

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--json")
	require.NoError(t, err)
}

func TestSessionList_PGFlagUsesPGReadStore(t *testing.T) {
	localDir := t.TempDir()
	remoteDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", localDir)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	t.Setenv("AGENTSVIEW_PG_SCHEMA", "custom_schema")

	seedSession(t, localDir, "local-session", "local")
	seedSession(t, remoteDir, "pg-session", "remote")

	remoteDB, err := db.Open(filepath.Join(remoteDir, "sessions.db"))
	require.NoError(t, err)
	var gotPG config.PGConfig
	cleanupCalled := false
	orig := openPGReadStore
	openPGReadStore = func(
		_ config.Config, pgCfg config.PGConfig,
	) (db.Store, func(), error) {
		gotPG = pgCfg
		return remoteDB, func() {
			cleanupCalled = true
			require.NoError(t, remoteDB.Close())
		}, nil
	}
	t.Cleanup(func() {
		openPGReadStore = orig
	})

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--pg", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Sessions []map[string]any `json:"sessions"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "pg-session", got.Sessions[0]["id"])
	assert.Equal(t, "postgres://example.test/agentsview", gotPG.URL)
	assert.Equal(t, "custom_schema", gotPG.Schema)
	assert.True(t, cleanupCalled, "expected PG store cleanup")
}

func TestSessionList_PGEnvUsesPGReadStore(t *testing.T) {
	localDir := t.TempDir()
	remoteDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", localDir)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/from-env")

	seedSession(t, localDir, "local-session", "local")
	seedSession(t, remoteDir, "pg-session", "remote")

	remoteDB, err := db.Open(filepath.Join(remoteDir, "sessions.db"))
	require.NoError(t, err)
	var gotPG config.PGConfig
	orig := openPGReadStore
	openPGReadStore = func(
		_ config.Config, pgCfg config.PGConfig,
	) (db.Store, func(), error) {
		gotPG = pgCfg
		return remoteDB, func() { require.NoError(t, remoteDB.Close()) }, nil
	}
	t.Cleanup(func() {
		openPGReadStore = orig
	})

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Sessions []map[string]any `json:"sessions"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "pg-session", got.Sessions[0]["id"])
	assert.Equal(t, "postgres://example.test/from-env", gotPG.URL)
	assert.Equal(t, "agentsview", gotPG.Schema)
}

func TestSessionList_PGFlagRequiresURL(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "")

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--pg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg url not configured")
	assert.Contains(t, err.Error(), "AGENTSVIEW_PG_URL")
}

func TestSessionList_DefaultDoesNotOpenPGStore(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "")
	seedSession(t, dataDir, "local-session", "local")

	orig := openPGReadStore
	openPGReadStore = func(
		config.Config, config.PGConfig,
	) (db.Store, func(), error) {
		t.Fatal("openPGReadStore should not be called without --pg or PG URL")
		return nil, nil, nil
	}
	t.Cleanup(func() {
		openPGReadStore = orig
	})

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Sessions []map[string]any `json:"sessions"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "local-session", got.Sessions[0]["id"])
}

func TestPGReadServiceClosesStoreWhenOpenFailsAfterCleanupProvided(t *testing.T) {
	closed := false
	orig := openPGReadStore
	openPGReadStore = func(
		config.Config, config.PGConfig,
	) (db.Store, func(), error) {
		return nil, func() { closed = true },
			errors.New("schema check failed")
	}
	t.Cleanup(func() {
		openPGReadStore = orig
	})

	_, _, err := newPGReadService(config.Config{}, config.PGConfig{
		URL:    "postgres://example.test/agentsview",
		Schema: "agentsview",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening pg store")
	assert.True(t, closed, "expected cleanup on error-after-open path")
}

// TestSessionList_MinToolFailuresZero verifies that passing
// --min-tool-failures 0 is treated as an explicit filter value
// (sessions with >=0 failures) rather than skipped as the int
// zero value. This exercises the cmd.Flags().Changed() guard
// that converts the int flag into a *int on ListFilter.
func TestSessionList_MinToolFailuresZero(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSessionWithOpts(t, dataDir, "s-a", "proj",
		func(s *db.Session) { s.ToolFailureSignalCount = 0 })

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--min-tool-failures", "0",
		"--format", "json")
	require.NoError(t, err)

	var got struct {
		Sessions []map[string]any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "s-a", got.Sessions[0]["id"])
}

// seedMessages inserts n message rows for sessionID with alternating
// user/assistant roles, ordinals starting at 1, and RFC3339
// timestamps one minute apart starting at 2026-04-01T00:00:00Z.
func seedMessages(t *testing.T, dataDir, sessionID string, n int) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	msgs := make([]db.Message, 0, n)
	for i := range n {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		content := fmt.Sprintf("msg-%d", i+1)
		msgs = append(msgs, db.Message{
			SessionID:     sessionID,
			Ordinal:       i + 1,
			Role:          role,
			Content:       content,
			ContentLength: len(content),
			Timestamp:     base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
		})
	}
	require.NoError(t, d.InsertMessages(msgs))
	require.NoError(t, d.Close())
}

func TestSessionMessages_JSONShape(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-msgs", "proj")
	seedMessages(t, dataDir, "s-msgs", 3)

	out, err := executeCommand(newRootCommand(),
		"session", "messages", "s-msgs", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Messages []map[string]any `json:"messages"`
		Count    int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 3, got.Count)
	require.Len(t, got.Messages, 3)
	assert.Equal(t, float64(1), got.Messages[0]["ordinal"])
}

func TestSessionMessages_FromLimit(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-msgs", "proj")
	seedMessages(t, dataDir, "s-msgs", 5)

	out, err := executeCommand(newRootCommand(),
		"session", "messages", "s-msgs",
		"--from", "3", "--limit", "2", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Messages []map[string]any `json:"messages"`
		Count    int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 2, got.Count)
	require.Len(t, got.Messages, 2)
	assert.Equal(t, float64(3), got.Messages[0]["ordinal"])
}

func TestSessionMessages_DirectionDesc(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-msgs", "proj")
	seedMessages(t, dataDir, "s-msgs", 4)

	out, err := executeCommand(newRootCommand(),
		"session", "messages", "s-msgs",
		"--direction", "desc", "--format", "json")
	require.NoError(t, err)

	var got struct {
		Messages []map[string]any `json:"messages"`
		Count    int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 4, got.Count)
	require.Len(t, got.Messages, 4)
	assert.Equal(t, float64(4), got.Messages[0]["ordinal"])
	assert.Equal(t, float64(3), got.Messages[1]["ordinal"])
	assert.Equal(t, float64(2), got.Messages[2]["ordinal"])
	assert.Equal(t, float64(1), got.Messages[3]["ordinal"])
}

// seedMessagesWithToolCalls inserts one assistant message for sessionID
// carrying n tool_use blocks, numbered 1..n with ToolName "Bash<i>".
// Ordinals start at 1. Timestamp is fixed for determinism.
func seedMessagesWithToolCalls(
	t *testing.T, dataDir, sessionID string, n int,
) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	calls := make([]db.ToolCall, 0, n)
	for i := range n {
		calls = append(calls, db.ToolCall{
			SessionID: sessionID,
			ToolName:  fmt.Sprintf("Bash%d", i+1),
			Category:  "shell",
			ToolUseID: fmt.Sprintf("tu-%d", i+1),
			InputJSON: `{"command":"echo hi"}`,
		})
	}
	msg := db.Message{
		SessionID:     sessionID,
		Ordinal:       1,
		Role:          "assistant",
		Content:       "",
		ContentLength: 0,
		Timestamp:     "2026-04-01T00:00:00Z",
		HasToolUse:    true,
		ToolCalls:     calls,
	}
	require.NoError(t, d.InsertMessages([]db.Message{msg}))
	require.NoError(t, d.Close())
}

func TestSessionToolCalls_JSONShape(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-tc", "proj")
	seedMessagesWithToolCalls(t, dataDir, "s-tc", 2)

	out, err := executeCommand(newRootCommand(),
		"session", "tool-calls", "s-tc", "--format", "json")
	require.NoError(t, err)

	var got struct {
		ToolCalls []map[string]any `json:"tool_calls"`
		Count     int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	assert.Equal(t, 2, got.Count)
	require.Len(t, got.ToolCalls, 2)
	assert.NotEmpty(t, got.ToolCalls[0]["tool_name"])
	assert.NotEmpty(t, got.ToolCalls[0]["timestamp"])
}

func TestSessionToolCalls_HumanTable(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-tc2", "proj")
	seedMessagesWithToolCalls(t, dataDir, "s-tc2", 2)

	out, err := executeCommand(newRootCommand(),
		"session", "tool-calls", "s-tc2")
	require.NoError(t, err)

	for _, token := range []string{
		"ORDINAL", "TIMESTAMP", "TOOL", "CATEGORY",
		"Bash1", "Bash2",
	} {
		assert.Contains(t, out, token,
			"human output should contain %q, got: %q", token, out)
	}
}

func TestSessionExport_StreamsFromDisk(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	src := filepath.Join(t.TempDir(), "session.jsonl")
	body := "{\"type\":\"user\",\"content\":\"hello\"}\n" +
		"{\"type\":\"assistant\",\"content\":\"hi\"}\n"
	require.NoError(t, os.WriteFile(src, []byte(body), 0o600))

	seedSessionWithOpts(t, dataDir, "s-1", "proj",
		func(s *db.Session) { s.FilePath = &src })

	out, err := executeCommand(newRootCommand(),
		"session", "export", "s-1")
	require.NoError(t, err)
	assert.Equal(t, body, out)
}

func TestSessionExport_AiderVirtualPathStreamsOnlySelectedRun(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	history := filepath.Join(repo, parser.AiderHistoryFileName())
	run0 := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n"
	run1 := "# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"
	run2 := "# aider chat started at 2026-06-09 16:45:00\n" +
		"#### third prompt\nanswer three\n"
	require.NoError(t, os.WriteFile(
		history, []byte("ignored preamble\n"+run0+run1+run2), 0o600,
	))
	rawID, ok := parser.AiderRawIDAt(history, 1)
	require.True(t, ok, "run 1 raw ID")

	seedSessionWithOpts(t, dataDir, "aider:"+rawID, "repo",
		func(s *db.Session) {
			s.Agent = string(parser.AgentAider)
			vp := parser.AiderVirtualPath(history, 1)
			s.FilePath = &vp
		})

	out, err := executeCommand(newRootCommand(),
		"session", "export", "aider:"+rawID)
	require.NoError(t, err)
	assert.Equal(t, run1, out)
	assert.NotContains(t, out, "first prompt")
	assert.NotContains(t, out, "third prompt")
}

func TestSessionExport_AiderStaleIndexReResolvesBySessionID(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	history := filepath.Join(repo, parser.AiderHistoryFileName())
	run0 := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n"
	run1 := "# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"
	require.NoError(t, os.WriteFile(history, []byte(run0+run1), 0o600))
	rawID, ok := parser.AiderRawIDAt(history, 1)
	require.True(t, ok, "run 1 raw ID")

	seedSessionWithOpts(t, dataDir, "aider:"+rawID, "repo",
		func(s *db.Session) {
			s.Agent = string(parser.AgentAider)
			vp := parser.AiderVirtualPath(history, 1)
			s.FilePath = &vp
		})

	inserted := "# aider chat started at 2026-06-09 13:00:00\n" +
		"#### inserted prompt\ninserted answer\n"
	require.NoError(t, os.WriteFile(
		history, []byte(inserted+run0+run1), 0o600,
	))

	out, err := executeCommand(newRootCommand(),
		"session", "export", "aider:"+rawID)
	require.NoError(t, err)
	assert.Equal(t, run1, out)
	assert.NotContains(t, out, "inserted prompt")
	assert.NotContains(t, out, "first prompt")
}

func TestSessionExport_FailsWhenSourceMissing(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	nonExistent := filepath.Join(t.TempDir(), "gone.jsonl")
	seedSessionWithOpts(t, dataDir, "s-1", "proj",
		func(s *db.Session) { s.FilePath = &nonExistent })

	_, err := executeCommand(newRootCommand(),
		"session", "export", "s-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source file not found")
}

func TestSessionExport_FailsWhenNotInLocalArchive(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(),
		"session", "export", "unknown-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in local archive")
	assert.Contains(t, err.Error(), "unknown-id")
}

// TestSessionExport_RejectsFormatFlag verifies that export refuses
// --format because it streams raw bytes. Previously --format was a
// silently-accepted inherited flag, which was a contract footgun
// for scripts that expected JSON output.
func TestSessionExport_RejectsFormatFlag(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(),
		"session", "export", "some-id", "--format", "json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--format not supported")
}

func TestSessionExport_RejectsPGFlag(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	_, err := executeCommand(newRootCommand(),
		"session", "export", "some-id", "--pg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local-only command")
	assert.Contains(t, err.Error(), "--pg not supported")
}

func TestSessionUsage_ServerFlagUsesHTTP(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/sessions/remote-session":
				_, _ = w.Write([]byte(`{
					"id": "remote-session",
					"agent": "codex",
					"project": "remote-project"
				}`))
			case "/api/v1/sessions/remote-session/usage":
				gotPath = r.URL.Path
				_, _ = w.Write([]byte(`{
					"session_id": "remote-session",
					"agent": "codex",
					"project": "remote-project",
					"total_output_tokens": 42,
					"peak_context_tokens": 2048,
					"has_token_data": true,
					"cost_usd": 0.5,
					"has_cost": true,
					"models": ["gpt-5.1"],
					"unpriced_models": [],
					"server_running": true
				}`))
			default:
				t.Errorf("unexpected path: %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
	defer ts.Close()

	root := newRootCommand()
	args := []string{"session", "usage", "remote-session", "--server", ts.URL}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))

	out, code, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "/api/v1/sessions/remote-session/usage", gotPath)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, "remote-session", out.SessionID)
	assert.Equal(t, "remote-project", out.Project)
	assert.Equal(t, 42, out.TotalOutputTokens)
	assert.True(t, out.ServerRunning)
}

func TestSessionUsage_ServerFlagResolvesBareID(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	const bareID = "019da6a6-8c67-7c23-b102-ef48502852d0"
	const canonicalID = "codex:" + bareID

	var gotUsagePath string
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/sessions/" + bareID:
				http.NotFound(w, r)
			case "/api/v1/sessions/" + canonicalID:
				_, _ = w.Write([]byte(`{
					"id": "` + canonicalID + `",
					"agent": "codex",
					"project": "remote-project"
				}`))
			case "/api/v1/sessions/" + canonicalID + "/usage":
				gotUsagePath = r.URL.Path
				_, _ = w.Write([]byte(`{
					"session_id": "` + canonicalID + `",
					"agent": "codex",
					"project": "remote-project",
					"total_output_tokens": 42,
					"peak_context_tokens": 2048,
					"has_token_data": true,
					"cost_usd": 0.5,
					"has_cost": true,
					"models": ["gpt-5.1"],
					"unpriced_models": [],
					"server_running": true
				}`))
			default:
				http.NotFound(w, r)
			}
		}))
	defer ts.Close()

	root := newRootCommand()
	args := []string{"session", "usage", bareID, "--server", ts.URL}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))

	out, code, err := sessionUsageDataForCommand(cmd, bareID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, canonicalID, out.SessionID)
	assert.Equal(t, "/api/v1/sessions/"+canonicalID+"/usage", gotUsagePath)
}

func TestSessionUsage_ServerFlagResolvesKimiRawID(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	const rawID = "project-hash:session-uuid"
	const canonicalID = "kimi:" + rawID

	var gotUsagePath string
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/sessions/" + rawID:
				http.NotFound(w, r)
			case "/api/v1/sessions/" + canonicalID:
				_, _ = w.Write([]byte(`{
					"id": "` + canonicalID + `",
					"agent": "kimi",
					"project": "remote-project"
				}`))
			case "/api/v1/sessions/" + canonicalID + "/usage":
				gotUsagePath = r.URL.Path
				_, _ = w.Write([]byte(`{
					"session_id": "` + canonicalID + `",
					"agent": "kimi",
					"project": "remote-project",
					"total_output_tokens": 84,
					"peak_context_tokens": 2048,
					"has_token_data": true,
					"cost_usd": 0.5,
					"has_cost": true,
					"models": ["kimi-k2"],
					"unpriced_models": [],
					"server_running": true
				}`))
			default:
				http.NotFound(w, r)
			}
		}))
	defer ts.Close()

	root := newRootCommand()
	args := []string{"session", "usage", rawID, "--server", ts.URL}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))

	out, code, err := sessionUsageDataForCommand(cmd, rawID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, canonicalID, out.SessionID)
	assert.Equal(t, "/api/v1/sessions/"+canonicalID+"/usage", gotUsagePath)
}

func TestSessionUsage_ServerFlagDoesNotSendConfigAuthToken(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "")
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`auth_token = "local-secret"`),
		0o600,
	))

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/sessions/remote-session":
				_, _ = w.Write([]byte(`{"id":"remote-session"}`))
			case "/api/v1/sessions/remote-session/usage":
				_, _ = w.Write([]byte(`{
					"session_id": "remote-session",
					"agent": "codex",
					"project": "remote-project",
					"total_output_tokens": 42,
					"peak_context_tokens": 2048,
					"has_token_data": true,
					"cost_usd": 0.5,
					"has_cost": true,
					"models": ["gpt-5.1"],
					"unpriced_models": [],
					"server_running": true
				}`))
			default:
				http.NotFound(w, r)
			}
		}))
	defer ts.Close()

	root := newRootCommand()
	args := []string{"session", "usage", "remote-session", "--server", ts.URL}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))

	out, _, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestSessionUsage_ServerFlagDoesNotLoadLocalConfig(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`not = valid = toml`),
		0o600,
	))

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/sessions/remote-session":
				_, _ = w.Write([]byte(`{"id":"remote-session"}`))
			case "/api/v1/sessions/remote-session/usage":
				_, _ = w.Write([]byte(`{
					"session_id": "remote-session",
					"agent": "codex",
					"project": "remote-project",
					"total_output_tokens": 42,
					"peak_context_tokens": 2048,
					"has_token_data": true,
					"cost_usd": 0.5,
					"has_cost": true,
					"models": ["gpt-5.1"],
					"unpriced_models": [],
					"server_running": true
				}`))
			default:
				http.NotFound(w, r)
			}
		}))
	defer ts.Close()

	root := newRootCommand()
	args := []string{"session", "usage", "remote-session", "--server", ts.URL}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))

	out, _, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestSessionUsage_ServerTokenSendsBearer(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	tokenFile := filepath.Join(dataDir, "remote-token")
	require.NoError(t, os.WriteFile(
		tokenFile, []byte("remote-secret\n"), 0o600,
	))

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer remote-secret",
				r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/sessions/remote-session":
				_, _ = w.Write([]byte(`{"id":"remote-session"}`))
			case "/api/v1/sessions/remote-session/usage":
				_, _ = w.Write([]byte(`{
					"session_id": "remote-session",
					"agent": "codex",
					"project": "remote-project",
					"total_output_tokens": 42,
					"peak_context_tokens": 2048,
					"has_token_data": true,
					"cost_usd": 0.5,
					"has_cost": true,
					"models": ["gpt-5.1"],
					"unpriced_models": [],
					"server_running": true
				}`))
			default:
				http.NotFound(w, r)
			}
		}))
	defer ts.Close()

	root := newRootCommand()
	args := []string{
		"session", "usage", "remote-session",
		"--server", ts.URL,
		"--server-token-file", tokenFile,
	}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))

	out, _, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestSessionUsage_ServerHTTPClientHasTimeout(t *testing.T) {
	oldClient := sessionUsageHTTPClient
	sessionUsageHTTPClient = &http.Client{Timeout: 20 * time.Millisecond}
	t.Cleanup(func() { sessionUsageHTTPClient = oldClient })

	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/sessions/remote-session":
				_, _ = w.Write([]byte(`{"id":"remote-session"}`))
			case "/api/v1/sessions/remote-session/usage":
				time.Sleep(200 * time.Millisecond)
				_, _ = w.Write([]byte(`{"session_id":"remote-session"}`))
			default:
				http.NotFound(w, r)
			}
		}))
	defer ts.Close()

	root := newRootCommand()
	args := []string{"session", "usage", "remote-session", "--server", ts.URL}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))

	start := time.Now()
	out, code, err := sessionUsageDataForCommand(cmd, "remote-session")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, tokenUseExitErr, code)
	assert.Less(t, elapsed, 150*time.Millisecond)
}

func TestSessionUsage_PGEnvUsesPGStore(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	pgDB, err := db.Open(filepath.Join(dataDir, "pg.db"))
	require.NoError(t, err)
	t.Cleanup(func() { pgDB.Close() })
	require.NoError(t, pgDB.UpsertSession(db.Session{
		ID:                   "pg-session",
		Project:              "pg-project",
		Machine:              "pg-host",
		Agent:                "codex",
		MessageCount:         2,
		UserMessageCount:     2,
		TotalOutputTokens:    42,
		HasTotalOutputTokens: true,
	}))

	opened := false
	orig := openPGReadStore
	openPGReadStore = func(
		_ config.Config, pgCfg config.PGConfig,
	) (db.Store, func(), error) {
		opened = true
		assert.Equal(t, "postgres://example.test/agentsview", pgCfg.URL)
		return pgDB, func() {}, nil
	}
	t.Cleanup(func() { openPGReadStore = orig })

	root := newRootCommand()
	args := []string{"session", "usage", "pg-session"}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(nil))

	out, code, err := sessionUsageDataForCommand(cmd, "pg-session")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.True(t, opened, "expected session usage to open PG store")
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, "pg-session", out.SessionID)
	assert.Equal(t, "pg-project", out.Project)
	assert.Equal(t, 42, out.TotalOutputTokens)
	assert.False(t, out.ServerRunning)
}

func TestSessionUsage_PGEnvResolvesBareSessionID(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	bareID := "019da6a6-8c67-7c23-b102-ef48502852d0"
	storedID := "codex:" + bareID
	pgDB, err := db.Open(filepath.Join(dataDir, "pg.db"))
	require.NoError(t, err)
	t.Cleanup(func() { pgDB.Close() })
	require.NoError(t, pgDB.UpsertSession(db.Session{
		ID:                   storedID,
		Project:              "pg-project",
		Machine:              "pg-host",
		Agent:                "codex",
		MessageCount:         2,
		UserMessageCount:     2,
		TotalOutputTokens:    42,
		HasTotalOutputTokens: true,
	}))

	orig := openPGReadStore
	openPGReadStore = func(
		_ config.Config, _ config.PGConfig,
	) (db.Store, func(), error) {
		return pgDB, func() {}, nil
	}
	t.Cleanup(func() { openPGReadStore = orig })

	root := newRootCommand()
	args := []string{"session", "usage", bareID}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(nil))

	out, code, err := sessionUsageDataForCommand(cmd, bareID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, storedID, out.SessionID)
	assert.Equal(t, "pg-project", out.Project)
	assert.Equal(t, 42, out.TotalOutputTokens)
}

func TestSessionUsage_PGEnvResolvesColonBearingRawSessionID(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	rawID := "project-hash:session-uuid"
	storedID := "kimi:" + rawID
	pgDB, err := db.Open(filepath.Join(dataDir, "pg.db"))
	require.NoError(t, err)
	t.Cleanup(func() { pgDB.Close() })
	require.NoError(t, pgDB.UpsertSession(db.Session{
		ID:                   storedID,
		Project:              "pg-project",
		Machine:              "pg-host",
		Agent:                "kimi",
		MessageCount:         2,
		UserMessageCount:     2,
		TotalOutputTokens:    84,
		HasTotalOutputTokens: true,
	}))

	orig := openPGReadStore
	openPGReadStore = func(
		_ config.Config, _ config.PGConfig,
	) (db.Store, func(), error) {
		return pgDB, func() {}, nil
	}
	t.Cleanup(func() { openPGReadStore = orig })

	root := newRootCommand()
	args := []string{"session", "usage", rawID}
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(nil))

	out, code, err := sessionUsageDataForCommand(cmd, rawID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, storedID, out.SessionID)
	assert.Equal(t, "pg-project", out.Project)
	assert.Equal(t, 84, out.TotalOutputTokens)
}

// TestSessionSync_UnknownID_ReportsNoFilePath verifies that the
// sync engine is plumbed in direct mode. No daemon running, no
// sessions in DB — Execute returns an error whose message contains
// "no file_path recorded" AND the missing id. Critically the
// error must NOT be db.ErrReadOnly (that would mean the engine
// was nil, i.e. direct-backend constructed without a real
// sync.Engine as in the default newService path).
func TestSessionSync_UnknownID_ReportsNoFilePath(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(),
		"session", "sync", "missing-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing-id")
	assert.Contains(t, err.Error(), "no file_path recorded",
		"error should come from directBackend.Sync validation, not ErrReadOnly")
	assert.NotContains(t, err.Error(), "read-only",
		"engine must be plumbed; got ErrReadOnly-style message: %v", err)
}

func TestSessionSync_PGFlagRefusesWrite(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	_, err := executeCommand(newRootCommand(),
		"session", "sync", "--pg", "some-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--pg is read-only")
	assert.Contains(t, err.Error(), "write commands")
}

func TestSessionSync_ServerFlagTreatsPathShapedArgAsRemotePath(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`not = valid = toml`),
		0o600,
	))

	var got struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/sessions/sync", r.URL.Path)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": "remote-session",
				"agent": "codex",
				"project": "remote-project"
			}`))
		}))
	defer ts.Close()

	out, err := executeCommand(newRootCommand(),
		"session", "sync", "--server", ts.URL,
		"/remote/session.jsonl", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"id":"remote-session"`)
	assert.Empty(t, got.ID)
	assert.Equal(t, "/remote/session.jsonl", got.Path)
}

// TestSessionSync_AgainstReadOnlyDaemon_Refuses verifies the CLI
// refuses to sync when a pg serve (ReadOnly=true) daemon owns
// the runtime record. Discovery uses the shared daemon ping
// endpoint, so the fixture must answer /api/ping.
func TestSessionSync_AgainstReadOnlyDaemon_Refuses(t *testing.T) {
	dataDir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	host, port := freeHTTPDaemon(t)
	_, err := WriteDaemonRuntime(
		dataDir, host, port, "test", true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	_, err = executeCommand(newRootCommand(),
		"session", "sync", "some-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only",
		"should refuse against pg serve daemon")
}

// TestSessionSync_WhenDaemonRuntimeUnprobeable_Refuses verifies that
// an unprobeable writable runtime record still suppresses direct writes.
func TestSessionSync_WhenDaemonRuntimeUnprobeable_Refuses(t *testing.T) {
	dataDir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	// Bind then immediately close so the port is guaranteed
	// free and no TCP listener is accepting.
	ln, port := freeTCPListener(t)
	ln.Close()

	_, err := WriteDaemonRuntime(
		dataDir, "127.0.0.1", port, "test", false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	_, err = executeCommand(newRootCommand(),
		"session", "sync", "some-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not responding",
		"should refuse against unreachable active daemon")
	assert.NotContains(t, err.Error(), "no file_path",
		"must not fall through to direct-write engine")
}

// TestSessionWatch_ExitsOnCancel verifies that `session watch`
// exits cleanly when the cobra Command's context is cancelled,
// without hanging on the upstream channel. Any NDJSON emitted
// to stdout must parse as one JSON object per line. We don't
// drive DB changes here (poll interval is 1.5s) — this test
// only asserts the plumbing: service resolution, channel wiring,
// and the shutdown path.
//
// To distinguish a real Watch call from an early-return stub, we
// also assert the command runs for at least ~150ms: any stub that
// returns synchronously would complete in single-digit ms.
func TestSessionWatch_ExitsOnCancel(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-watch", "proj")

	root := newRootCommand()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"session", "watch", "s-watch"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	var execErr error
	select {
	case execErr = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session watch did not exit within 3s after ctx cancel")
	}
	elapsed := time.Since(start)

	// Clean cancellation must surface as either nil (upstream channel
	// closed on ctx cancel) or an error that wraps context.Canceled.
	// Anything else indicates a regression that earlier versions of
	// this test swallowed by discarding execErr.
	if execErr != nil && !errors.Is(execErr, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", execErr)
	}

	// A stub that returns immediately would complete far faster
	// than the 200ms cancel delay. Require the command to actually
	// wait on the Watch channel.
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond,
		"session watch returned too quickly (%v) — "+
			"likely a stub, not a real Watch", elapsed)

	// Any output must be valid NDJSON. Empty output is fine.
	for line := range bytes.SplitSeq(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		require.NoError(t, json.Unmarshal(line, &ev),
			"non-NDJSON line: %q", line)
	}
}

// TestSessionWatch_UnknownID_FailsFast verifies that `session
// watch` against an unknown session id fails fast with a clear
// "session not found" error rather than returning an indefinitely
// live heartbeat stream. Slow-failure mode would be a contract
// footgun for automation scripts.
func TestSessionWatch_UnknownID_FailsFast(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	_, err := executeCommand(newRootCommand(),
		"session", "watch", "unknown-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found",
		"expected 'session not found' error; got: %v", err)
	assert.Contains(t, err.Error(), "unknown-id",
		"error should name the missing session id")
}

// TestLooksLikePath covers both POSIX and Windows-style separators
// so "./session.jsonl" works on Windows and bare session IDs stay
// classified as IDs regardless of platform.
func TestLooksLikePath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abc-123", false},
		{"550e8400-e29b-41d4-a716-446655440000", false},
		{"codex:my-session", false},
		{".", true},
		{"..", true},
		{"./session.jsonl", true},
		{"../parent/session.jsonl", true},
		{`.\session.jsonl`, true},
		{`..\parent\session.jsonl`, true},
		{"subdir/session.jsonl", true},
		{`subdir\session.jsonl`, true},
		{"/abs/path.jsonl", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := looksLikePath(tc.in); got != tc.want {
				t.Fatalf("looksLikePath(%q) = %v, want %v",
					tc.in, got, tc.want)
			}
		})
	}
}
