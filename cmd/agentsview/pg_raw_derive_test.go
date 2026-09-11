package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/server"
)

func TestPGRawRuntimeChild(t *testing.T) {
	if os.Getenv("RAW_RUNTIME_CHILD") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestPGRawRuntimeReadinessCancellationJoinsChild(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan *exec.Cmd, 1)
	joined := make(chan struct{})
	r := newPGRawRuntime(t.Context(), time.Hour, func(ctx context.Context) {
		calls.Add(1)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPGRawRuntimeChild$")
		cmd.Env = []string{"RAW_RUNTIME_CHILD=1"}
		if err := cmd.Start(); err != nil {
			entered <- nil
			return
		}
		entered <- cmd
		_ = cmd.Wait()
		close(joined)
	})
	assert.Zero(t, calls.Load(), "preparation must not start claims")
	r.Start()
	r.Start()
	var child *exec.Cmd
	select {
	case child = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	require.NotNil(t, child)
	r.Stop()
	select {
	case <-joined:
	default:
		t.Fatal("Stop returned before child Wait")
	}
	require.NotNil(t, child.ProcessState)
	r.Start()
	r.Stop()
	assert.EqualValues(t, 1, calls.Load())
}
func TestPGRawRuntimeClosedBeforeReadinessNeverStarts(t *testing.T) {
	var calls atomic.Int32
	r := newPGRawRuntime(t.Context(), time.Second, func(context.Context) { calls.Add(1) })
	r.Stop()
	r.Start()
	r.Stop()
	assert.Zero(t, calls.Load())
}

func TestMain(m *testing.M) {
	if handled, code := rawderive.RunParserChild(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestPGRawRuntimeReadinessFailureCleansBeforeReturn(t *testing.T) {
	var cleaned, started atomic.Bool
	cfg := config.Config{DataDir: t.TempDir(), Host: "invalid host", Port: 12345, NoBrowser: true}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startup := pgServeStartup{cfg: cfg, ctx: ctx, srv: server.New(cfg, nil, nil), rtOpts: serveRuntimeOptions{Mode: "pg-serve", RequestedPort: cfg.Port}, cleanup: func() { cleaned.Store(true) }, startWorker: func() { started.Store(true) }}
	require.Error(t, runPreparedPGServe(startup))
	assert.True(t, cleaned.Load())
	assert.False(t, started.Load())
}

func TestHostedProjectionConfiguredContentPolicies(t *testing.T) {
	parsed := parser.ParseResult{Session: parser.ParsedSession{ID: "policy-session", Agent: parser.AgentClaude}, Messages: []parser.ParsedMessage{
		{Ordinal: 0, Role: parser.RoleAssistant, Content: "checking", ToolCalls: []parser.ParsedToolCall{{ToolUseID: "read", ToolName: "Read", Category: "Read", InputJSON: `{"path":"file"}`}, {ToolUseID: "bash", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"true"}`}}},
		{Ordinal: 1, Role: parser.RoleUser, ToolResults: []parser.ParsedToolResult{{ToolUseID: "read", ContentRaw: `"private result"`, ContentLength: 14}, {ToolUseID: "bash", ContentRaw: `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`}}},
	}}
	imageContent, err := json.Marshal(`[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`)
	require.NoError(t, err)
	parsed.Messages[1].ToolResults[1].ContentRaw = string(imageContent)
	for _, mode := range []string{"full", "blocked", "images", "transcripts"} {
		t.Run(mode, func(t *testing.T) {
			app := config.Config{}
			switch mode {
			case "blocked":
				app.ResultContentBlockedCategories = []string{" rEaD ", " "}
			case "images":
				app.ToolResultImages = config.ToolResultImagesDrop
			case "transcripts":
				app.ArchiveContent = config.ArchiveContentTranscripts
			}
			options := hostedRawProjectionOptions(app, "tenant", rawderive.RetryPolicy{}).Content
			candidate, err := ingest.PrepareCandidate(t.Context(), parsed, options)
			require.NoError(t, err)
			prepared, err := ingest.Finalize(t.Context(), candidate, options)
			require.NoError(t, err)
			require.Len(t, prepared.Messages, 1)
			require.Len(t, prepared.Messages[0].ToolCalls, 2)
			read, bash := prepared.Messages[0].ToolCalls[0], prepared.Messages[0].ToolCalls[1]
			if mode == "blocked" || mode == "transcripts" {
				assert.Empty(t, read.ResultContent)
			} else {
				assert.Equal(t, "private result", read.ResultContent)
			}
			if mode == "images" || mode == "transcripts" {
				assert.NotContains(t, bash.ResultContent, "base64,AAEC")
			} else {
				assert.Contains(t, bash.ResultContent, "base64,AAEC")
			}
			if mode == "transcripts" {
				assert.Empty(t, bash.InputJSON)
			} else {
				assert.Equal(t, `{"command":"true"}`, bash.InputJSON)
			}
		})
	}
}

func hostedRuntimeClaudeFixture() []byte {
	return []byte(`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"runtime-session","message":{"content":"hello runtime"},"cwd":"/work/project"}` + "\n" + `{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"runtime-session","message":{"content":"hello viewer"}}` + "\n")
}

func TestHostedRuntimeClaudeFixtureHasTwoPreparedMessages(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "runtime-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, hostedRuntimeClaudeFixture(), 0400))
	factory, ok := parser.ProviderFactoryByType(parser.AgentClaude)
	require.True(t, ok)
	provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{root}, Machine: "hosted"})
	outcome, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: parser.SourceRef{Provider: parser.AgentClaude, DisplayPath: path, Key: path, ProjectHint: "project"}})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	candidate, err := ingest.PrepareCandidate(t.Context(), outcome.Results[0].Result, ingest.ContentOptions{})
	require.NoError(t, err)
	prepared, err := ingest.Finalize(t.Context(), candidate, ingest.ContentOptions{})
	require.NoError(t, err)
	require.Len(t, prepared.Messages, 2)
	assert.Equal(t, "runtime-session", prepared.Session.ID)
	assert.Equal(t, "user", prepared.Messages[0].Role)
	assert.Equal(t, "hello runtime", prepared.Messages[0].Content)
	assert.Equal(t, "assistant", prepared.Messages[1].Role)
	assert.Equal(t, "hello viewer", prepared.Messages[1].Content)
}
