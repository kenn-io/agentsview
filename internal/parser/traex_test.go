package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestTraeXSessionIDRelabel(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"codex prefix", "codex:019fbcca", "traex:019fbcca"},
		{
			"already relabeled",
			"traex:019fbcca",
			"traex:019fbcca",
		},
		{
			"host-prefixed id keeps its host",
			"devbox/codex:019fbcca",
			"devbox/traex:019fbcca",
		},
		{
			// strings.Replace(..., 1) semantics, matching
			// relabelOpenCodeSessionAsKilo: a raw ID that repeats the
			// prefix keeps everything after the first occurrence verbatim.
			"only the first occurrence is replaced",
			"codex:codex:019fbcca",
			"traex:codex:019fbcca",
		},
		{
			"unprefixed id is untouched",
			"019fbcca",
			"019fbcca",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, traeXSessionID(tt.id))
		})
	}
}

func TestRelabelCodexResultAsTraeX(t *testing.T) {
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
				ToolUseID:         "call_1",
				AgentID:           "spawned",
				SubagentSessionID: "codex:spawned",
			}},
		}},
	}}

	relabelCodexResultAsTraeX(sess, msgs)

	assert.Equal(t, "traex:child", sess.ID)
	assert.Equal(t, "traex:parent", sess.ParentSessionID)
	assert.Equal(t, "traex:origin", sess.SourceSessionID)
	assert.Equal(t, AgentTraeX, sess.Agent)
	assert.Equal(
		t, "traex:spawned", msgs[0].ToolCalls[0].SubagentSessionID,
	)
	assert.Equal(
		t,
		"traex:spawned",
		msgs[0].ToolCalls[0].ResultEvents[0].SubagentSessionID,
	)
	// AgentID is the raw upstream thread ID, not an agentsview session ID,
	// so it must survive the relabel unchanged.
	assert.Equal(t, "spawned", msgs[0].ToolCalls[0].ResultEvents[0].AgentID)
}

// TestRelabelCodexResultAsTraeXIncremental covers the provider's incremental
// path, which has appended rows but no session to relabel.
func TestRelabelCodexResultAsTraeXIncremental(t *testing.T) {
	msgs := []ParsedMessage{{
		ToolCalls: []ParsedToolCall{{
			SubagentSessionID: "codex:spawned",
		}},
	}}
	require.NotPanics(t, func() {
		relabelCodexResultAsTraeX(nil, msgs)
	})
	assert.Equal(
		t, "traex:spawned", msgs[0].ToolCalls[0].SubagentSessionID,
	)
}

func TestTraeXRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentTraeX)
	require.True(t, ok)
	assert.Equal(t, "traex:", def.IDPrefix)
	assert.True(t, def.FileBased)
	assert.Equal(t, []string{".trae/cli/sessions"}, def.DefaultDirs)
	// TraeX writes plaintext rollouts, so unlike the Trae IDE entry it must
	// stay eligible for remote sync.
	assert.False(t, def.RemoteSyncExcluded)

	// traex: must not be swallowed by the Trae IDE entry's trae: prefix.
	byPrefix, ok := AgentByPrefix("traex:019fbcca")
	require.True(t, ok)
	assert.Equal(t, AgentTraeX, byPrefix.Type)
}

func TestTraeXProviderParseRelabelsCodexSession(t *testing.T) {
	root := t.TempDir()
	const (
		uuid     = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
		parent   = "019fbc4a-48b9-7472-a0da-6d92901383db"
		spawned  = "019fbcd0-1111-7000-8000-000000000001"
		callID   = "call_spawn_1"
		filename = "rollout-2026-08-01T18-07-03-" + uuid + ".jsonl"
	)
	path := filepath.Join(root, "2026", "08", "01", filename)
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSubagentSessionMetaJSON(
			uuid, parent,
			"/home/user/code/api", "codex-tui",
			"2026-08-01T18:07:03.636Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "Explore the parser", "2026-08-01T18:07:04Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"spawn_agent", callID,
			`{"prompt":"explore"}`, "2026-08-01T18:07:05Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			callID,
			`{"agent_id":"`+spawned+`"}`,
			"2026-08-01T18:07:06Z",
		),
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	provider, ok := NewProvider(AgentTraeX, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentTraeX, sources[0].Provider)
	assert.Equal(t, path, sources[0].DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages

	assert.Equal(t, AgentTraeX, sess.Agent)
	assert.Equal(t, "traex:"+uuid, sess.ID)
	assert.Equal(t, "traex:"+parent, sess.ParentSessionID)
	assert.Equal(t, "devbox", sess.Machine)

	var subagentIDs []string
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			if call.SubagentSessionID != "" {
				subagentIDs = append(subagentIDs, call.SubagentSessionID)
			}
		}
	}
	assert.Equal(t, []string{"traex:" + spawned}, subagentIDs)
}

// TestTraeXProviderParsesDeidentifiedRollout runs the full Discover -> Parse
// path over a fixture captured from a real TRAE CLI 2.0 rollout (paths,
// prompts, and identifiers replaced), guarding the claim that TraeX rollouts
// are byte-compatible with the Codex format.
func TestTraeXProviderParsesDeidentifiedRollout(t *testing.T) {
	root := t.TempDir()
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	dst := filepath.Join(
		root, "2026", "08", "01",
		"rollout-2026-08-01T18-07-03-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	fixture, err := os.ReadFile(filepath.Join(
		"testdata", "traex", "rollout_subagent.jsonl",
	))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, fixture, 0o644))

	provider, ok := NewProvider(AgentTraeX, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages

	assert.Equal(t, AgentTraeX, sess.Agent)
	assert.Equal(t, "traex:"+uuid, sess.ID)
	assert.Equal(
		t, "traex:019fbc4a-48b9-7472-a0da-6d92901383db",
		sess.ParentSessionID,
	)
	assert.Equal(t, "api", sess.Project)
	require.NotEmpty(t, msgs)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Explore the parser package.", msgs[0].Content)
	assert.Equal(t, "gpt-5-codex", msgs[len(msgs)-1].Model)
	assert.Positive(t, sess.TotalOutputTokens)

	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			assert.NotContains(t, call.SubagentSessionID, "codex:")
		}
	}
}

// TestTraeXAndCodexProvidersKeepSeparateSourceKeys guards the discovery
// namespace: the two agents share a UUID shape, so a shared source key would
// let one agent's session resolve to the other's file.
func TestTraeXAndCodexProvidersKeepSeparateSourceKeys(t *testing.T) {
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	assert.NotEqual(
		t,
		CodexSourceKey(AgentCodex, uuid),
		CodexSourceKey(AgentTraeX, uuid),
	)
}
