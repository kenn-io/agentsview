package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAugureCodeSessionIDRelabel(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"codex prefix", "codex:019fbcca", "augure-code:019fbcca"},
		{
			"already relabeled",
			"augure-code:019fbcca",
			"augure-code:019fbcca",
		},
		{
			"traex prefix is not touched",
			"traex:019fbcca",
			"traex:019fbcca",
		},
		{
			"host-prefixed id keeps its host",
			"devbox/codex:019fbcca",
			"devbox/augure-code:019fbcca",
		},
		{
			// strings.Replace(..., 1) semantics, matching traeXSessionID:
			// a raw ID that repeats the prefix keeps everything after the
			// first occurrence verbatim.
			"only the first occurrence is replaced",
			"codex:codex:019fbcca",
			"augure-code:codex:019fbcca",
		},
		{
			"unprefixed id is untouched",
			"019fbcca",
			"019fbcca",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, augureCodeSessionID(tt.id))
		})
	}
}

func TestRelabelCodexResultAsAugureCode(t *testing.T) {
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

	relabelCodexResultAsAugureCode(sess, msgs, nil)

	assert.Equal(t, "augure-code:child", sess.ID)
	assert.Equal(t, "augure-code:parent", sess.ParentSessionID)
	assert.Equal(t, "augure-code:origin", sess.SourceSessionID)
	assert.Equal(t, AgentAugureCode, sess.Agent)
	assert.Equal(
		t, "augure-code:spawned", msgs[0].ToolCalls[0].SubagentSessionID,
	)
	assert.Equal(
		t,
		"augure-code:spawned",
		msgs[0].ToolCalls[0].ResultEvents[0].SubagentSessionID,
	)
	// AgentID is the raw upstream thread ID, not an agentsview session ID,
	// so it must survive the relabel unchanged.
	assert.Equal(t, "spawned", msgs[0].ToolCalls[0].ResultEvents[0].AgentID)
}

// TestRelabelCodexResultAsAugureCodeIncremental covers the provider's
// incremental path, which has appended rows but no session to relabel.
func TestRelabelCodexResultAsAugureCodeIncremental(t *testing.T) {
	msgs := []ParsedMessage{{
		ToolCalls: []ParsedToolCall{{
			SubagentSessionID: "codex:spawned",
		}},
	}}
	require.NotPanics(t, func() {
		relabelCodexResultAsAugureCode(nil, msgs, nil)
	})
	assert.Equal(
		t, "augure-code:spawned", msgs[0].ToolCalls[0].SubagentSessionID,
	)
}

// TestRelabelCodexToolCallUpdatesAsAugureCode covers the incremental path's
// late tool-result updates: their events are appended after the message
// relabel ran, so they must be relabeled too or the stored rows keep codex:
// links.
func TestRelabelCodexToolCallUpdatesAsAugureCode(t *testing.T) {
	updates := []ParsedToolCallUpdate{{
		ToolUseID: "call_1",
		ResultEvents: []ParsedToolResultEvent{{
			SubagentSessionID: "codex:spawned",
			AgentID:           "spawned",
		}},
	}}

	relabelCodexToolCallUpdatesAsAugureCode(updates)

	require.Len(t, updates[0].ResultEvents, 1)
	assert.Equal(
		t, "augure-code:spawned",
		updates[0].ResultEvents[0].SubagentSessionID,
	)
	assert.Equal(t, "spawned", updates[0].ResultEvents[0].AgentID,
		"AgentID is the raw upstream thread ID and must be untouched")

	// The provider hook relabels updates on the incremental path and must
	// tolerate an empty update slice.
	require.NotPanics(t, func() {
		relabelCodexResultAsAugureCode(nil, nil, updates)
		relabelCodexResultAsAugureCode(nil, nil, nil)
	})
	assert.Equal(
		t, "augure-code:spawned",
		updates[0].ResultEvents[0].SubagentSessionID,
	)
}

func TestAugureCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentAugureCode)
	require.True(t, ok)
	assert.Equal(t, "augure-code:", def.IDPrefix)
	assert.Equal(t, "AUGURE_CODE_SESSIONS_DIR", def.EnvVar)
	assert.Equal(t, "augure_code_sessions_dirs", def.ConfigKey)
	assert.True(t, def.FileBased)
	assert.True(t, def.PostAnswerToolWork,
		"Codex-format rollouts emit the answer before later tool calls")
	assert.False(t, def.RemoteSyncExcluded,
		"Augure Code rollouts are plaintext and stay eligible for remote sync")

	// augure-code: must resolve through AgentByPrefix.
	byPrefix, ok := AgentByPrefix("augure-code:019fbcca")
	require.True(t, ok)
	assert.Equal(t, AgentAugureCode, byPrefix.Type)

	assert.Equal(t, []string{".augure/sessions"}, def.DefaultDirs)
	assert.Nil(t, def.ShallowWatchRootsFunc,
		"the shallow watch exists for Codex's session_index.jsonl only; Augure Code writes none")
}

// TestAugureCodeProviderParsesDeidentifiedRollout runs the full Discover ->
// Parse path over a fixture captured from a real Augure CLI 1.0.6 rollout
// (paths, prompts, and identifiers replaced), guarding the claim that
// Augure Code rollouts are byte-compatible with the Codex format.
func TestAugureCodeProviderParsesDeidentifiedRollout(t *testing.T) {
	root := t.TempDir()
	const uuid = "3f2b1c9a-7d51-4e0a-9b2f-1c8a4d5e6f70"
	dst := filepath.Join(
		root, "2026", "09", "07",
		"rollout-2026-09-07T14-02-01-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	fixture, err := os.ReadFile(filepath.Join(
		"testdata", "augure", "rollout_session.jsonl",
	))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, fixture, 0o644))

	provider, ok := NewProvider(AgentAugureCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentAugureCode, sources[0].Provider)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages

	assert.Equal(t, AgentAugureCode, sess.Agent)
	assert.Equal(t, "augure-code:"+uuid, sess.ID)
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, "api", sess.Project)
	assert.Equal(t, "devbox", sess.Machine)
	require.NotEmpty(t, msgs)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Summarize the provider facade.", msgs[0].Content)
	// The model comes from turn_context (the Codex parser's model source);
	// thread_settings_applied carries the same value.
	assert.Equal(t, "ossington-5", msgs[len(msgs)-1].Model)

	// token_count-derived usage lands as session aggregates.
	assert.True(t, sess.HasPeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Positive(t, sess.TotalOutputTokens)

	// The exec_command / function_call_output pair completes a result event.
	var foundCompletedResult bool
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			assert.NotContains(t, call.SubagentSessionID, "codex:")
			for range call.ResultEvents {
				foundCompletedResult = true
			}
		}
	}
	assert.True(t, foundCompletedResult,
		"expected the exec_command call to carry a result event")

	// Codex-format full parses force-replace stored rows.
	assert.True(t, outcome.ForceReplace)
}

// TestAugureCodeProviderKeepsSeparateSourceKeys guards the discovery
// namespace: Codex, TraeX, and Augure Code share a UUID shape, so a shared
// source key would let one agent's session resolve to another's file.
func TestAugureCodeProviderKeepsSeparateSourceKeys(t *testing.T) {
	const uuid = "3f2b1c9a-7d51-4e0a-9b2f-1c8a4d5e6f70"
	assert.NotEqual(
		t,
		CodexSourceKey(AgentCodex, uuid),
		CodexSourceKey(AgentAugureCode, uuid),
	)
	assert.NotEqual(
		t,
		CodexSourceKey(AgentTraeX, uuid),
		CodexSourceKey(AgentAugureCode, uuid),
	)
}

// TestAugureCodeProviderIgnoresCodexSidecars covers the Codex-only
// out-of-band surfaces the shared provider must not expose to a fork: the
// session_index.jsonl watch, the index changed-path fan-out, and the
// s3://.../raw/codex archive layout. Augure Code writes none of them.
func TestAugureCodeProviderIgnoresCodexSidecars(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".augure")
	root := filepath.Join(base, "sessions")
	const uuid = "3f2b1c9a-7d51-4e0a-9b2f-1c8a4d5e6f70"
	path := filepath.Join(
		root, "2026", "09", "07",
		"rollout-2026-09-07T14-02-01-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"timestamp":"2026-09-07T14:02:01.500Z","type":"session_meta",`+
			`"payload":{"id":"`+uuid+`","cwd":"/home/user/code/api",`+
			`"originator":"codex-tui","cli_version":"1.0.6",`+
			`"model_provider":"augure"}}`+"\n",
	), 0o644))
	// A stray index file: copied in, or left by a Codex root that used to
	// own this directory. It must not fan out to every Augure Code session.
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Renamed","updated_at":`+
			`"2026-09-07T14:02:01Z"}`+"\n",
	), 0o644))

	provider, ok := NewProvider(AgentAugureCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
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

	assert.False(t, AgentSupportsS3Discovery(AgentAugureCode),
		"Augure Code has no S3 archive convention and must not import one as Codex")
	s3Provider, ok := S3ProviderFor(AgentAugureCode)
	assert.False(t, ok)
	assert.Nil(t, s3Provider)

	// Raw capture is a Codex-owned capability: the fork path must clear it.
	caps := provider.Capabilities()
	assert.Equal(t, RawCaptureCapabilities{}, caps.RawCapture,
		"Augure Code must not claim Codex raw capture")
}
