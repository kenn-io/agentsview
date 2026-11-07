package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const openInterpreterFixtureUUID = "01a1ad20-5c4f-7010-9529-8716a3356361"

// writeOpenInterpreterFixture copies the de-identified rollout to path.
func writeOpenInterpreterFixture(t *testing.T, path string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join(
		"testdata", "openinterpreter", "rollout_session.jsonl",
	))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, fixture, 0o644))
}

func openInterpreterRolloutName() string {
	return "rollout-2026-11-05T19-12-21-" + openInterpreterFixtureUUID + ".jsonl"
}

// TestOpenInterpreterProviderParsesRollout runs Discover -> Parse over a
// fixture built from an Open Interpreter 0.0.59 rollout (paths, prompts, and
// instructions replaced). Its first turn keeps the records Codex-era fixtures
// lack: the top-level ordinal, world_state, item_completed, and a turn that
// ended in a provider error. The second turn exercises a tool call and token
// usage in the shared Codex record shapes.
func TestOpenInterpreterProviderParsesRollout(t *testing.T) {
	root := t.TempDir()
	writeOpenInterpreterFixture(t, filepath.Join(
		root, "2026", "11", "05", openInterpreterRolloutName(),
	))

	provider, ok := NewProvider(AgentInterpreter, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentInterpreter, sources[0].Provider)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages

	assert.Equal(t, AgentInterpreter, sess.Agent)
	assert.Equal(t, "open-interpreter:"+openInterpreterFixtureUUID, sess.ID)
	assert.Equal(t, "notes", sess.Project)
	assert.Equal(t, "/home/user/code/notes", sess.Cwd)
	assert.Equal(t, "which model", sess.FirstMessage)
	assert.Zero(t, sess.MalformedLines,
		"world_state, item_completed, and ordinal are valid records")
	assert.Equal(t, 2, sess.UserMessageCount)

	require.Len(t, msgs, 4)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "which model", msgs[0].Content)
	assert.Equal(t, RoleUser, msgs[1].Role)
	assert.Equal(t, "List the notes directory.", msgs[1].Content)
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Equal(t, "The directory holds README.md and todo.md.", msgs[3].Content)
	for _, msg := range msgs {
		assert.Equal(t, "glm-5.2", msg.Model)
	}

	require.Len(t, msgs[2].ToolCalls, 1)
	call := msgs[2].ToolCalls[0]
	assert.Equal(t, "call_exec_1", call.ToolUseID)
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(t, "README.md\ntodo.md", call.ResultEvents[0].Content)

	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 90, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 5200, sess.PeakContextTokens)

	assert.True(t, outcome.ForceReplace)
}

// TestOpenInterpreterProviderFindsArchivedRollout covers the flat
// archived_sessions directory `interpreter archive` moves rollouts into, the
// second default root alongside the dated sessions tree.
func TestOpenInterpreterProviderFindsArchivedRollout(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	archived := filepath.Join(home, "archived_sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	writeOpenInterpreterFixture(t, filepath.Join(
		archived, openInterpreterRolloutName(),
	))

	provider, ok := NewProvider(AgentInterpreter, ProviderConfig{
		Roots: []string{sessions, archived},
	})
	require.True(t, ok)

	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: openInterpreterFixtureUUID,
	})
	require.NoError(t, err)
	require.True(t, found)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t,
		"open-interpreter:"+openInterpreterFixtureUUID,
		outcome.Results[0].Result.Session.ID,
	)
}

// TestOpenInterpreterProviderKeepsSeparateSourceKeys guards the discovery
// namespace: every Codex fork shares the rollout UUID shape, so a shared
// source key would let one agent's session resolve to another's file.
func TestOpenInterpreterProviderKeepsSeparateSourceKeys(t *testing.T) {
	for _, agent := range []AgentType{AgentCodex, AgentTraeX, AgentAugureCode} {
		assert.NotEqual(t,
			CodexSourceKey(agent, openInterpreterFixtureUUID),
			CodexSourceKey(AgentInterpreter, openInterpreterFixtureUUID),
		)
	}
}

// TestOpenInterpreterProviderKeepsCodexOnlySurfaces covers the Codex-only
// surfaces the shared provider keeps from forks: session_index.jsonl watches
// and fan-out, the S3 archive layout, and raw capture.
func TestOpenInterpreterProviderKeepsCodexOnlySurfaces(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".openinterpreter")
	root := filepath.Join(base, "sessions")
	writeOpenInterpreterFixture(t, filepath.Join(
		root, "2026", "11", "05", openInterpreterRolloutName(),
	))
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+openInterpreterFixtureUUID+`","thread_name":"Renamed",`+
			`"updated_at":"2026-11-05T19:12:30Z"}`+"\n",
	), 0o644))

	provider, ok := NewProvider(AgentInterpreter, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1, "no shallow session_index.jsonl watch")
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{Path: indexPath},
	)
	require.NoError(t, err)
	assert.Empty(t, sources, "index events must not fan out for a fork")

	assert.False(t, AgentSupportsS3Discovery(AgentInterpreter))
	_, ok = S3ProviderFor(AgentInterpreter)
	assert.False(t, ok)
	assert.Equal(t, RawCaptureCapabilities{}, provider.Capabilities().RawCapture)
}

// TestRelabelCodexResultAsOpenInterpreter covers the subagent links the Codex
// parser stamps with codex:, on both the full parse (session plus messages)
// and the incremental path (appended messages and late tool-result updates
// with no session).
func TestRelabelCodexResultAsOpenInterpreter(t *testing.T) {
	sess := &ParsedSession{
		ID:              "codex:child",
		ParentSessionID: "codex:parent",
		SourceSessionID: "codex:origin",
		Agent:           AgentCodex,
	}
	msgs := []ParsedMessage{{
		ToolCalls: []ParsedToolCall{{
			ToolUseID:         "call_1",
			SubagentSessionID: "codex:spawned",
			ResultEvents: []ParsedToolResultEvent{{
				AgentID:           "spawned",
				SubagentSessionID: "codex:spawned",
			}},
		}},
	}}
	relabelCodexResultAsOpenInterpreter(sess, msgs, nil)

	assert.Equal(t, "open-interpreter:child", sess.ID)
	assert.Equal(t, "open-interpreter:parent", sess.ParentSessionID)
	assert.Equal(t, "open-interpreter:origin", sess.SourceSessionID)
	assert.Equal(t, AgentInterpreter, sess.Agent)
	call := msgs[0].ToolCalls[0]
	assert.Equal(t, "open-interpreter:spawned", call.SubagentSessionID)
	assert.Equal(t, "open-interpreter:spawned", call.ResultEvents[0].SubagentSessionID)
	assert.Equal(t, "spawned", call.ResultEvents[0].AgentID,
		"AgentID is the raw upstream thread ID")

	updates := []ParsedToolCallUpdate{{
		ToolUseID: "call_2",
		ResultEvents: []ParsedToolResultEvent{{
			SubagentSessionID: "codex:late",
		}},
	}}
	relabelCodexResultAsOpenInterpreter(nil, nil, updates)
	assert.Equal(t, "open-interpreter:late", updates[0].ResultEvents[0].SubagentSessionID)

	assert.Empty(t, openInterpreterSessionID(""))
	assert.Equal(t, "traex:x", openInterpreterSessionID("traex:x"))
	assert.Equal(t, "devbox~open-interpreter:x", openInterpreterSessionID("devbox~codex:x"))
}
