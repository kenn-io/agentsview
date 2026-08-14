package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestCodexCheckpointBootstrapForUpgradedArchive pins the upgrade path:
// an existing Codex archive (data version current, content committed) that
// predates parser checkpoints must earn a checkpoint on its next ordinary
// sync — one authoritative full parse that commits content and resume
// state together — without forceParse or ResyncAll. After the bootstrap,
// no-op syncs read no transcript bytes and appends read only the tail.
// Deleting the bootstrap branch (falling back to the fingerprint skip)
// makes this test fail because the checkpoint row never reappears.
func TestCodexCheckpointBootstrapForUpgradedArchive(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	root := writeCodexParityRoot(t, uuid)
	sessionID := "codex:" + uuid

	database, err := db.Open(filepath.Join(t.TempDir(), "bootstrap.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	// Initial cold sync: content plus a checkpoint.
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	cp, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, codexCheckpointVersion, cp.Version)
	before, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, before)

	// Simulate a v88 archive written before checkpoints existed: the
	// session content stays, the checkpoint rows are gone, and the data
	// version remains current (88).
	require.NoError(t, database.DeleteParserCheckpoint(sessionID))
	_, ok, err = database.GetParserCheckpoint(sessionID)
	require.NoError(t, err)
	require.False(t, ok)

	// Ordinary sync (no forceParse, no ResyncAll): the bootstrap must
	// re-establish the checkpoint while preserving the stored content.
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	cp, ok, err = database.GetParserCheckpoint(sessionID)
	require.NoError(t, err)
	require.True(t, ok, "the bootstrap must recreate the checkpoint")
	require.Equal(t, codexCheckpointVersion, cp.Version)
	after, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, before, after,
		"the bootstrap must preserve the stored projection")

	// The next sync is a true no-op: zero writes and no transcript read.
	if runtime.GOOS == "linux" {
		rcharBefore := processRchar(t)
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Synced, "post-bootstrap sync must skip")
		require.Less(t, processRchar(t)-rcharBefore, int64(1<<20),
			"the no-op sync must not read the transcript")
	} else {
		require.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
	}

	// An appended late output resumes from the checkpoint: only the new
	// tail (plus the bounded anchor) is read, and the merged rows match
	// an authoritative full parse of the same bytes.
	path := filepath.Join(
		root, "2024", "01", "01",
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a", "late output", "2024-01-01T10:00:13Z",
		),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	if runtime.GOOS == "linux" {
		rcharBefore := processRchar(t)
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		require.Equal(t, 1, stats.Synced)
		require.Less(t, processRchar(t)-rcharBefore, int64(256<<10),
			"the append must read only the bounded tail and anchor")
	} else {
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		require.Equal(t, 1, stats.Synced)
	}

	// Authoritative parity for the appended tail.
	cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(t, ok)
	source, found, err := provider.FindSource(
		context.Background(), parser.FindSourceRequest{
			FullSessionID: sessionID,
		},
	)
	require.NoError(t, err)
	require.True(t, found)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), parser.ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	var wantEvents int
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ToolUseID == "call_a" {
				wantEvents = len(tc.ResultEvents)
			}
		}
	}
	require.Equal(t, 2, wantEvents,
		"the authoritative parse sees both call_a outputs")
	stored, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	for _, m := range stored {
		for _, tc := range m.ToolCalls {
			if tc.ToolUseID == "call_a" {
				require.Len(t, tc.ResultEvents, wantEvents,
					"the append must merge exactly the authoritative result")
				require.Equal(t, "late output",
					tc.ResultEvents[len(tc.ResultEvents)-1].Content)
			}
		}
	}
}

// writeCodexParityRoot writes the standard parity transcript fixture.
// (The staged-path tests share this fixture; it lives here until the
// staging layer lands and takes ownership.)
func writeCodexParityRoot(t *testing.T, uuid string) string {
	t.Helper()
	return writeCodexTranscriptRoot(t, uuid, codexParityTranscript(uuid))
}

func writeCodexTranscriptRoot(t *testing.T, uuid, transcript string) string {
	t.Helper()
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.WriteFile(
		path, []byte(transcript), 0o644,
	))
	return root
}

func codexParityTranscript(uuid string) string {
	return testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexMsgJSON("user", "run the suite", "2024-01-01T10:00:02Z"),
		testjsonl.CodexMsgJSON(
			"assistant", "running", "2024-01-01T10:00:03Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_a", nil, "2024-01-01T10:00:04Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a",
			"build passed AKIA7QHWN2DKR4FYPLJM",
			"2024-01-01T10:00:05Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:00:06Z", 120, 40, 80,
		),
		testjsonl.CodexMsgJSON("user", "again", "2024-01-01T10:00:07Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_b", nil, "2024-01-01T10:00:08Z",
		),
		// A status-carrying output keeps the failure classification
		// status-driven: the staged in-memory model omits event content
		// (content heuristics arrive with the streaming signal reducer),
		// so parity here pins the status-based path.
		`{"timestamp":"2024-01-01T10:00:09Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_b","status":"errored","output":[{"type":"input_text","text":"command not found"}]}}`,
		// A status-less output whose failure lives only in the content
		// heuristics: the streaming reducer must classify it identically.
		testjsonl.CodexMsgJSON("user", "third", "2024-01-01T10:00:10Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_c", nil, "2024-01-01T10:00:11Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_c",
			"bash: python3: command not found",
			"2024-01-01T10:00:12Z",
		),
	)
}

// processRchar returns the process's cumulative read bytes (Linux).
func processRchar(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "rchar: ") {
			v, err := strconv.ParseInt(
				strings.TrimSpace(strings.TrimPrefix(line, "rchar: ")),
				10, 64,
			)
			require.NoError(t, err)
			return v
		}
	}
	t.Fatal("no rchar in /proc/self/io")
	return 0
}
