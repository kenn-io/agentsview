package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
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
	<-t.Context().Done()
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
		require.FailNow(t, "worker did not start")
	}
	require.NotNil(t, child)
	r.Stop()
	select {
	case <-joined:
	default:
		require.FailNow(t, "Stop returned before child Wait")
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
	// Match the default daemon fixtures; replacement tests set their own versions.
	version = "test"
	os.Exit(runWithTestRuntimeRoot(m))
}

// testRuntimeRootEnv returns the runtime root variables for a helper child
// whose environment a test builds by hand.
func testRuntimeRootEnv() []string {
	return []string{
		testRuntimeRootVar + "=" + os.Getenv(testRuntimeRootVar),
		"XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR"),
	}
}

// Some helpers strip AGENTSVIEW_* from child environments.
const testRuntimeRootVar = "AVTEST_RUNTIME_ROOT"

// runWithTestRuntimeRoot points XDG_RUNTIME_DIR at one short private directory
// shared with helper child processes, which inherit the environment. Serve
// tests with long t.TempDir data dirs put the Chrome host socket there, and
// removing it at exit cleans up after children that tests kill.
func runWithTestRuntimeRoot(m *testing.M) int {
	if os.Getenv(testRuntimeRootVar) != "" {
		return m.Run()
	}
	root, err := os.MkdirTemp("", "av")
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			panic(err)
		}
	}()
	for key, value := range map[string]string{testRuntimeRootVar: root, "XDG_RUNTIME_DIR": root} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	return m.Run()
}

func TestPGRawRuntimeReadinessFailureCleansBeforeReturn(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprintf("watch=%t", watch), func(t *testing.T) {
			var cleaned, started atomic.Bool
			dir := t.TempDir()
			cfg := config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db"), Host: "invalid host", Port: 12345, NoBrowser: true, PG: config.PGConfig{URL: "postgres://user@127.0.0.1:1/archive", AllowInsecure: true}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			startup := replicaServeStartup{cfg: cfg, ctx: ctx, srv: server.New(cfg, nil, nil), rtOpts: serveRuntimeOptions{Mode: "pg-serve", RequestedPort: cfg.Port}, cleanup: func() { cleaned.Store(true) }}
			if watch {
				require.NoError(t, attachReplicaServeWatch(&startup, cancel, pgReplica{}, nil, &ReplicaPushConfig{}))
			}
			startup.startWorker = func() { started.Store(true) }
			require.Error(t, runPreparedReplicaServe("pg", startup))
			assert.True(t, cleaned.Load())
			assert.False(t, started.Load())
			if watch {
				for _, name := range []string{"db.write.lock", "pg-watch.lock"} {
					lock := flock.New(filepath.Join(cfg.DataDir, name))
					locked, err := lock.TryLock()
					require.NoError(t, err)
					require.True(t, locked)
					require.NoError(t, lock.Unlock())
				}
			}
		})
	}
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

func TestHostedRawDerivationRejectsImageOffloadBeforeOpeningDatabase(t *testing.T) {
	_, err := prepareHostedPGServe(
		config.Config{RequireAuth: true, ToolResultImages: config.ToolResultImagesOffload},
		config.PGConfig{RawTenant: "tenant", Schema: "hosted", RawDerivation: true, URL: "://invalid"},
		"",
	)
	require.ErrorContains(t, err, "tool_result_images = offload is not supported for hosted raw derivation")
}

func hostedRuntimeClaudeFixture() []byte {
	return []byte(`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"runtime-session","message":{"content":"hello runtime"},"cwd":"/work/project"}` + "\n" + `{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"runtime-session","message":{"content":"hello viewer"}}` + "\n")
}

func TestHostedRuntimeClaudeFixtureHasTwoPreparedMessages(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "runtime-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, hostedRuntimeClaudeFixture(), 0o400))
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
