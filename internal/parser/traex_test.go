package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

	relabelCodexResultAsTraeX(sess, msgs, nil)

	assert.Equal(t, "traex:child", sess.ID)
	assert.Equal(t, "traex:parent", sess.ParentSessionID)
	assert.Equal(t, "traex:origin", sess.SourceSessionID)
	assert.Equal(t, AgentTraeX, sess.Agent)
	assert.Equal(t,
		"traex:spawned", msgs[0].ToolCalls[0].SubagentSessionID,
	)
	assert.Equal(t,
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
		relabelCodexResultAsTraeX(nil, msgs, nil)
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

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentTraeX, sources[0].Provider)
	assert.Equal(t, path, sources[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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

func TestTraeXProviderIgnoresCopiedCodexSessionIndex(t *testing.T) {
	root := t.TempDir()
	sessionsRoot := filepath.Join(root, "sessions")
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	path := filepath.Join(
		sessionsRoot, "2026", "08", "01",
		"rollout-2026-08-01T18-07-03-"+uuid+".jsonl",
	)
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex-tui",
			"2026-08-01T18:07:03Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "transcript prompt", "2026-08-01T18:07:04Z",
		),
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	indexPath := filepath.Join(root, CodexSessionIndexFilename)
	alphaIndex := `{"id":"` + uuid +
		`","thread_name":"Alpha title"}` + "\n"
	bravoIndex := `{"id":"` + uuid +
		`","thread_name":"Bravo title"}` + "\n"
	require.Len(t, bravoIndex, len(alphaIndex))
	require.NoError(t, os.WriteFile(indexPath, []byte(alphaIndex), 0o644))
	rolloutTime := time.Now().Add(-2 * time.Hour)
	indexTime := rolloutTime.Add(time.Hour)
	require.NoError(t, os.Chtimes(path, rolloutTime, rolloutTime))
	require.NoError(t, os.Chtimes(indexPath, indexTime, indexTime))
	assert.Equal(t, "Alpha title", LookupCodexThreadName(path, uuid))

	require.NoError(t, os.WriteFile(indexPath, []byte(bravoIndex), 0o644))
	require.NoError(t, os.Chtimes(indexPath, indexTime, indexTime))

	provider, ok := NewProvider(AgentTraeX, ProviderConfig{
		Roots: []string{sessionsRoot}, Machine: "host",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, ForceParse: true,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	rolloutInfo, err := os.Stat(path)
	require.NoError(t, err)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, rolloutInfo.ModTime().UnixNano(), fingerprint.MTimeNS)
	assert.Equal(t, rolloutInfo.ModTime().UnixNano(), sess.File.Mtime)
	assert.Empty(t, sess.SessionName)
	codexSessionIndexCache.mu.Lock()
	cachedTitle := codexSessionIndexCache.entries[indexPath].titles[uuid]
	codexSessionIndexCache.mu.Unlock()
	assert.Equal(t, "Alpha title", cachedTitle,
		"TraeX force parse must not evict or reload the Codex index cache")
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

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages

	assert.Equal(t, AgentTraeX, sess.Agent)
	assert.Equal(t, "traex:"+uuid, sess.ID)
	assert.Equal(t,
		"traex:019fbc4a-48b9-7472-a0da-6d92901383db",
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

func TestTraeXProviderParsesHistoryMutationRollout(t *testing.T) {
	root := t.TempDir()
	const uuid = "019fcf70-1111-7000-8000-000000000001"
	dst := filepath.Join(
		root, "2026", "09", "01",
		"rollout-2026-09-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	fixture, err := os.ReadFile(filepath.Join(
		"testdata", "traex", "history_mutation_session.jsonl",
	))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, fixture, 0o644))

	provider, ok := NewProvider(AgentTraeX, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages

	assert.Equal(t, AgentTraeX, sess.Agent)
	assert.Equal(t, "traex:"+uuid, sess.ID)
	assert.Equal(t, 4, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "Inspect the parser.", sess.FirstMessage)
	require.Len(t, msgs, 4)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Inspect the parser.", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "I will inspect the parser.", msgs[1].Content)
	require.Len(t, msgs[2].ToolCalls, 1)
	call := msgs[2].ToolCalls[0]
	assert.Equal(t, "call_read", call.ToolUseID)
	assert.Equal(t, "exec", call.ToolName)
	assert.Equal(t, "Bash", call.Category)
	assert.Equal(t, `{"cmd":"ls internal/parser"}`, call.InputJSON)
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(t, "codex.go\ntraex.go", call.ResultEvents[0].Content)
	assert.JSONEq(t, `{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":12}`, string(msgs[2].TokenUsage))
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Equal(t,
		"The parser files are codex.go and traex.go.",
		msgs[3].Content,
	)
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

// TestTraeXProviderIgnoresCodexSidecars covers the three Codex-only
// out-of-band surfaces the shared provider must not expose to a fork: the
// session_index.jsonl watch, the index changed-path fan-out, and the
// s3://.../raw/codex archive layout. TraeX writes none of them, and importing
// an S3 root through the Codex scanner would stamp AgentCodex, silently moving
// the sessions into Codex's identity namespace.
func TestTraeXProviderIgnoresCodexSidecars(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".trae", "cli")
	root := filepath.Join(base, "sessions")
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	path := filepath.Join(
		root, "2026", "08", "01",
		"rollout-2026-08-01T18-07-03-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/home/user/code/api", "codex-tui",
			"2026-08-01T18:07:03.636Z",
		),
	)), 0o644))
	// A stray index file: copied in, or left by a Codex root that used to own
	// this directory. It must not fan out to every TraeX session.
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Renamed","updated_at":`+
			`"2026-08-01T18:07:03Z"}`+"\n",
	), 0o644))

	provider, ok := NewProvider(AgentTraeX, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1, "no shallow session_index.jsonl watch")
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	classifier, ok := provider.(interface {
		SourcesForChangedPath(
			context.Context, ChangedPathRequest,
		) ([]SourceRef, error)
	})
	require.True(t, ok)
	sources, err := classifier.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{Path: indexPath},
	)
	require.NoError(t, err)
	assert.Empty(t, sources, "index events must not fan out for a fork")

	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })
	listS3Objects = func(string) ([]S3Object, error) {
		return []S3Object{{
			URI: "s3://bucket/devbox/raw/codex/2026/08/01/" +
				"rollout-2026-08-01T18-07-03-" + uuid + ".jsonl",
			Size:         11,
			LastModified: time.Unix(100, 0),
			Fingerprint:  "s3-meta:rollout",
		}}, nil
	}
	s3Provider, ok := NewProvider(AgentTraeX, ProviderConfig{
		Roots: []string{"s3://bucket/devbox/raw/codex"},
	})
	require.True(t, ok)
	s3Sources, err := s3Provider.Discover(t.Context())
	require.NoError(t, err)
	assert.Empty(t, s3Sources,
		"TraeX has no S3 archive convention and must not import one as Codex")
}

// TestTraeXRegistryCoversArchivedSessions guards the `traex archive <id>`
// destination: TRAE CLI moves a rollout out of the dated tree into a flat
// archived_sessions directory, mirroring `codex archive`.
func TestTraeXRegistryCoversArchivedSessions(t *testing.T) {
	def, ok := AgentByType(AgentTraeX)
	require.True(t, ok)
	assert.Equal(t, []string{
		".trae/cli/sessions",
		".trae/cli/archived_sessions",
	}, def.DefaultDirs)
	assert.Nil(t, def.ShallowWatchRootsFunc,
		"the shallow watch exists for Codex's session_index.jsonl only")
}

func TestTraeXProviderHistoryMutationUsage(t *testing.T) {
	const uuid = "019fcf70-1111-7000-8000-000000000002"
	const reply = `{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reply"}]}]}}`
	const usage = `"usage":{"input_tokens":100,"cached_input_tokens":40,"cache_creation_input_tokens":0,"output_tokens":12}`
	const record = `{"type":"token_usage_record","payload":{"response_id":"response-1",` + usage + `}}`
	tests := []struct {
		name      string
		rows      string
		wantUsage []string
	}{
		{
			name:      "empty usage before valid record",
			rows:      testjsonl.JoinJSONL(reply, `{"type":"token_usage_record","payload":{"response_id":"response-1","usage":{}}}`, record),
			wantUsage: []string{`{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":12}`},
		},
		{
			name:      "equal usage from distinct responses",
			rows:      testjsonl.JoinJSONL(reply, record, reply, `{"type":"token_usage_record","payload":{"response_id":"response-2",`+usage+`}}`),
			wantUsage: []string{`{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":12}`, `{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":12}`},
		},
		{
			name:      "repeated response counted once",
			rows:      testjsonl.JoinJSONL(reply, record, reply, record),
			wantUsage: []string{`{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":12}`, ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			prefix := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "codex-tui", tsEarly),
				testjsonl.CodexMsgJSON("user", "Inspect", tsEarlyS1),
			)
			path := writeCodexProviderSessionContent(t, root, uuid, prefix)
			provider, ok := NewProvider(AgentTraeX, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			appendCodexProviderContent(t, path, tt.rows+"\n")
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			msgs := outcome.Results[0].Result.Messages
			require.Len(t, msgs, 1+len(tt.wantUsage))
			for i, want := range tt.wantUsage {
				assert.Equal(t, "Reply", msgs[i+1].Content)
				if want == "" {
					assert.Empty(t, msgs[i+1].TokenUsage)
				} else {
					assert.JSONEq(t, want, string(msgs[i+1].TokenUsage))
					assert.Equal(t, 100, msgs[i+1].ContextTokens)
				}
			}
		})
	}
}

func TestTraeXProviderHistoryMutationValidation(t *testing.T) {
	const uuid = "019fcf70-1111-7000-8000-000000000005"
	tests := []struct {
		name    string
		payload string
		wantErr string
		mode    string
	}{
		{"missing version", `{"operation":"append","items":[]}`, "", "full"},
		{"missing items", `{"operation":"append"}`, "", "full"},
		{"string items", `{"operation":"append","items":"invalid"}`, "items", "full"},
		{"null items", `{"operation":"append","items":null}`, "items", "full"},
		{"missing operation", `{"items":[]}`, "operation", "full"},
		{"newer version", `{"version":2,"operation":"append","items":[]}`, "version", "full"},
		{"seed refusal", `{"operation":"replace","items":[]}`, "operation", "seed"},
		{"incremental refusal", `{"operation":"replace","items":[]}`, "operation", "incremental"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			prefix := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "codex-tui", tsEarly),
				testjsonl.CodexMsgJSON("user", "Inspect", tsEarlyS1),
			)
			path := writeCodexProviderSessionContent(t, root, uuid, prefix)
			provider, ok := NewProvider(AgentTraeX, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			appendCodexProviderContent(t, path, `{"type":"history_mutation","payload":`+tt.payload+"}\n")
			if tt.mode == "full" {
				_, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			} else {
				offset := int64(len(prefix))
				if tt.mode == "seed" {
					info, statErr := os.Stat(path)
					require.NoError(t, statErr)
					offset = info.Size()
					appendCodexProviderContent(t, path, testjsonl.CodexMsgJSON("assistant", "Reply", tsEarlyS5)+"\n")
				}
				fingerprint, fingerprintErr := provider.Fingerprint(t.Context(), sources[0])
				require.NoError(t, fingerprintErr)
				_, _, err = provider.ParseIncremental(t.Context(), IncrementalRequest{
					Source: sources[0], Fingerprint: fingerprint, SessionID: "traex:" + uuid,
					Offset: offset, StartOrdinal: 1,
				})
			}
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			wantErr := "invalid TraeX history_mutation: " + tt.wantErr
			require.ErrorContains(t, err, wantErr)
		})
	}
}

func TestTraeXProviderHistoryMutationReplay(t *testing.T) {
	const parent = "019fcf70-1111-7000-8000-000000000006"
	const child = "019fcf70-1111-7000-8000-000000000007"
	for _, tt := range []struct{ name, lineage string }{
		{"fork", `"forked_from_id":"` + parent + `"`},
		{"subagent", `"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `"}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeCodexProviderSessionContent(t, root, parent, testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parent, "/workspace/project", "codex-tui", tsEarly),
				`{"type":"turn_context","payload":{"turn_id":"parent-turn"}}`,
			))
			path := writeCodexProviderSessionContent(t, root, child, testjsonl.JoinJSONL(
				`{"type":"session_meta","payload":{"id":"`+child+`","cwd":"/workspace/project",`+tt.lineage+`}}`,
				testjsonl.CodexSessionMetaJSON(parent, "/workspace/project", "codex-tui", tsEarly),
				`{"type":"turn_context","payload":{"turn_id":"parent-turn"}}`,
				`{"type":"history_mutation","payload":{"operation":"replace","items":[]}}`,
				`{"type":"history_mutation","payload":{"version":2,"operation":"append","items":[]}}`,
				`{"type":"turn_context","payload":{"turn_id":"child-turn"}}`,
				`{"type":"history_mutation","payload":{"version":1,"operation":"append","items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Child prompt"}]}]}}`,
			))
			provider, ok := NewProvider(AgentTraeX, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: child, StoredFilePath: path})
			require.NoError(t, err)
			require.True(t, found)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			require.Len(t, outcome.Results[0].Result.Messages, 1)
			assert.Equal(t, "Child prompt", outcome.Results[0].Result.Messages[0].Content)
		})
	}
}

func TestCodexProviderIgnoresHistoryMutation(t *testing.T) {
	const uuid = "019fcf70-1111-7000-8000-000000000008"
	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "codex-tui", tsEarly),
		testjsonl.CodexMsgJSON("user", "Inspect", tsEarlyS1),
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Ignore"}]}]}}`,
		`{"type":"history_mutation","payload":{"version":2,"operation":"replace","items":[]}}`,
	))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: uuid, StoredFilePath: path})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	require.Len(t, outcome.Results[0].Result.Messages, 1)
	assert.Equal(t, "Inspect", outcome.Results[0].Result.Messages[0].Content)
}

func TestTraeXProviderHistoryMutationIncremental(t *testing.T) {
	const uuid = "019fcf70-1111-7000-8000-000000000003"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "codex-tui", tsEarly),
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect"}]},{"type":"function_call","name":"shell","call_id":"call-late","arguments":"{}"}]}}`,
	)
	tail := testjsonl.JoinJSONL(
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"function_call_output","call_id":"call-late","output":[{"type":"text","text":"Done"}]}]}}`,
		`{"type":"token_usage_record","payload":{"response_id":"response-1","usage":{"input_tokens":100,"cached_input_tokens":40,"cache_creation_input_tokens":0,"output_tokens":12}}}`,
	)
	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider, ok := NewProvider(AgentTraeX, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	appendCodexProviderContent(t, path, tail)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	ordinal := 1
	outcome, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
		Source: sources[0], Fingerprint: fingerprint, SessionID: "traex:" + uuid,
		Offset: int64(len(prefix)), StartOrdinal: 2, StoredPendingUsageOrdinal: &ordinal,
	})
	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	assert.False(t, outcome.ForceReplace)
	assert.Empty(t, outcome.Messages)
	require.Len(t, outcome.ToolCallUpdates, 1)
	update := outcome.ToolCallUpdates[0]
	assert.Equal(t, "call-late", update.ToolUseID)
	require.Len(t, update.ResultEvents, 1)
	assert.Equal(t, "Done", update.ResultEvents[0].Content)
	require.Len(t, outcome.MessageTokenUsageUpdates, 1)
	assert.Equal(t, 1, outcome.MessageTokenUsageUpdates[0].Ordinal)
	assert.JSONEq(t, `{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":12}`, string(outcome.MessageTokenUsageUpdates[0].TokenUsage))
	full, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], ForceParse: true})
	require.NoError(t, err)
	require.Len(t, full.Results, 1)
	msgs := full.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, string(msgs[1].TokenUsage), string(outcome.MessageTokenUsageUpdates[0].TokenUsage))
	assert.Equal(t, msgs[1].ToolCalls[0].ResultEvents, update.ResultEvents)
	appendCodexProviderContent(t, path, testjsonl.JoinJSONL(
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"function_call","name":"wait_agent","call_id":"call-wait","arguments":"{}"}]}}`,
	))
	nextFingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	fallback, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
		Source: sources[0], Fingerprint: nextFingerprint, SessionID: "traex:" + uuid,
		Offset: fingerprint.Size, StartOrdinal: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, fallback.ForceReplace)
}

func TestTraeXProviderHistoryMutationToolOutputs(t *testing.T) {
	tests := []struct {
		name         string
		tool         string
		output       string
		want         string
		wantSubagent string
	}{
		{"split text JSON", "spawn_agent", `[{"type":"text","text":"{\"agent_id\":\"ch"},{"type":"output_text","text":"ild\"}"}]`, `{"agent_id":"child"}`, "traex:child"},
		{"split text", "shell", `[{"type":"input_text","text":"Do"},{"type":"text","text":"ne\n"},{"type":"output_text","text":"now"}]`, "Done\nnow", ""},
		{"mixed image", "shell", `[{"type":"text","text":"Done"},{"type":"image","url":"https://example.com/image.png"}]`, `[{"type":"text","text":"Done"},{"type":"image","url":"https://example.com/image.png"}]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const uuid = "019fcf70-1111-7000-8000-000000000004"
			root := t.TempDir()
			writeCodexProviderSessionContent(t, root, uuid, testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "codex-tui", tsEarly),
				`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"function_call","name":"`+tt.tool+`","call_id":"call-1","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":`+tt.output+`}]}}`,
			))
			provider, ok := NewProvider(AgentTraeX, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			msgs := outcome.Results[0].Result.Messages
			require.Len(t, msgs, 1)
			require.Len(t, msgs[0].ToolCalls, 1)
			assert.Equal(t, tt.wantSubagent, msgs[0].ToolCalls[0].SubagentSessionID)
			if tt.wantSubagent != "" {
				return
			}
			require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
			assert.Equal(t, tt.want, msgs[0].ToolCalls[0].ResultEvents[0].Content)
		})
	}
}
