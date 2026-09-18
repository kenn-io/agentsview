package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	sessionID := "session-main"
	sourcePath := filepath.Join(root, projectDir, sessionID+".jsonl")
	subagentPath := filepath.Join(
		root,
		projectDir,
		sessionID,
		"subagents",
		"workflows",
		"wf-123",
		"agent-worker.jsonl",
	)
	writeSourceFile(t, sourcePath, claudeProviderFixture("main question"))
	writeSourceFile(t, subagentPath, claudeProviderFixture("subagent question"))
	writeSourceFile(
		t,
		filepath.Join(root, projectDir, sessionID, "subagents", "not-agent.jsonl"),
		claudeProviderFixture("ignored"),
	)
	writeSourceFile(t, filepath.Join(root, projectDir, "agent-root.jsonl"), claudeProviderFixture("ignored"))

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)
	assert.Equal([]string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	assert.ElementsMatch([]string{sourcePath, subagentPath}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})
	for _, source := range discovered {
		assert.Equal(AgentClaude, source.Provider)
		assert.Equal(projectDir, source.ProjectHint)
	}

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "remote~" + sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "agent-worker",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(subagentPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(subagentPath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.Positive(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: subagentPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(subagentPath, changed[0].DisplayPath)

	require.NoError(os.Remove(sourcePath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)

	require.NoError(os.Remove(subagentPath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: subagentPath, EventKind: "rename", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(subagentPath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, projectDir, "agent-root.jsonl"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(err)
	assert.Empty(ignored)
}

func TestClaudeFullParseHonorsContextBetweenLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeSourceFile(t, path, claudeProviderFixture("question"))
	ctx := newCancelOnErrCheckContext(t, 4)

	_, _, err := claudeParseFile(
		path, "project", "machine", claudeParseOptions{ctx: ctx},
	)

	require.ErrorIs(t, err, context.Canceled)
}

func TestClaudeProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	targetRoot := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	sessionID := "session-main"
	targetProject := filepath.Join(targetRoot, projectDir)
	sourceProject := filepath.Join(root, projectDir)
	sourcePath := filepath.Join(sourceProject, sessionID+".jsonl")
	subagentPath := filepath.Join(
		sourceProject,
		sessionID,
		"subagents",
		"jobs",
		"job-1",
		"agent-linked.jsonl",
	)
	writeSourceFile(
		t,
		filepath.Join(targetProject, sessionID+".jsonl"),
		claudeProviderFixture("from symlink"),
	)
	writeSourceFile(
		t,
		filepath.Join(targetProject, sessionID, "subagents", "jobs", "job-1", "agent-linked.jsonl"),
		claudeProviderFixture("from symlink subagent"),
	)
	if err := os.Symlink(targetProject, sourceProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	assert.ElementsMatch([]string{sourcePath, subagentPath}, sourceDisplayPaths(discovered))

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "agent-linked",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(subagentPath, found.DisplayPath)
}

// A followed project-directory symlink whose target cannot be resolved must
// surface incomplete streaming discovery rather than reading as absent:
// reconciliation treats a clean DiscoverEach as authoritative and would
// tombstone every session beneath the symlink.
func TestClaudeProviderStreamingDiscoveryPropagatesProjectSymlinkErrors(t *testing.T) {
	discoverEach := func(t *testing.T, root string) ([]string, error) {
		t.Helper()
		provider, ok := NewProvider(AgentClaude, ProviderConfig{
			Roots: []string{root},
		})
		require.True(t, ok)
		discoverer, ok := provider.(StreamingDiscoverer)
		require.True(t, ok)
		var yielded []string
		err := discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})
		return yielded, err
	}
	healthyPath := func(root string) string {
		return filepath.Join(root, "-Users-dev-code-demo", "session-main.jsonl")
	}

	t.Run("dangling project symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		writeSourceFile(t, healthyPath(root), claudeProviderFixture("hello claude"))
		target := filepath.Join(t.TempDir(), "linked-project")
		require.NoError(os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(os.RemoveAll(target))

		yielded, err := discoverEach(t, root)

		require.Error(err)
		assert.ErrorIs(err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(err, &incomplete)
		// The walker records the failure and continues with healthy siblings.
		assert.Equal([]string{healthyPath(root)}, yielded)

		require.NoError(os.Remove(link))
		yielded, err = discoverEach(t, root)
		require.NoError(err)
		assert.Equal([]string{healthyPath(root)}, yielded)
	})

	t.Run("unstatable project symlink target", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		if runtime.GOOS == "windows" {
			t.Skip("directory read permissions are not enforced on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		root := t.TempDir()
		writeSourceFile(t, healthyPath(root), claudeProviderFixture("hello claude"))
		targetParent := t.TempDir()
		target := filepath.Join(targetParent, "linked-project")
		require.NoError(os.MkdirAll(target, 0o755))
		if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(os.Chmod(targetParent, 0o000))
		t.Cleanup(func() { _ = os.Chmod(targetParent, 0o755) })

		yielded, err := discoverEach(t, root)

		require.Error(err)
		assert.ErrorIs(err, os.ErrPermission)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(err, &incomplete)
		assert.Equal([]string{healthyPath(root)}, yielded)

		require.NoError(os.Chmod(targetParent, 0o755))
		yielded, err = discoverEach(t, root)
		require.NoError(err)
		assert.Equal([]string{healthyPath(root)}, yielded)
	})
}

func TestClaudeRawCaptureRootReplacementIsIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	require.NoError(os.Mkdir(root, 0o700))
	require.NoError(os.WriteFile(
		filepath.Join(root, "ignored.txt"), nil, 0o600,
	))
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	paused := make(chan struct{})
	resume := make(chan struct{})
	progressCalls := 0
	ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error {
		progressCalls++
		if progressCalls == 2 {
			close(paused)
			<-resume
		}
		return nil
	})
	type discoveryResult struct {
		discovery RawCaptureDiscovery
		err       error
	}
	resultCh := make(chan discoveryResult, 1)
	go func() {
		discovery, err := DiscoverRawCaptureSources(ctx, provider)
		resultCh <- discoveryResult{discovery: discovery, err: err}
	}()

	<-paused
	require.NoError(os.Rename(root, root+"-old"))
	require.NoError(os.Mkdir(root, 0o700))
	close(resume)
	result := <-resultCh

	require.Error(result.err)
	assert.ErrorIs(result.err, errStreamingDirectoryChanged)
	var incomplete DiscoveryIncompleteError
	assert.ErrorAs(result.err, &incomplete)
	assert.False(result.discovery.Complete)
}

func TestClaudeProviderStreamingDiscoveryStopsAfterYieldError(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	for _, project := range []string{"-Users-dev-code-one", "-Users-dev-code-two"} {
		writeSourceFile(
			t, filepath.Join(root, project, "session.jsonl"),
			claudeProviderFixture("streamed"),
		)
	}
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(ok)

	stop := errors.New("stop discovery")
	calls := 0
	err := discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
		calls++
		return stop
	})
	require.ErrorIs(err, stop)
	assert.Equal(t, 1, calls)
}

func TestClaudeProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	sessionID := "session-main"
	sourcePath := filepath.Join(root, projectDir, sessionID+".jsonl")
	writeSourceFile(t, sourcePath, claudeProviderFixture("parse question"))

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.False(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal(sessionID, result.Result.Session.ID)
	assert.Equal(AgentClaude, result.Result.Session.Agent)
	assert.Equal("demo", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(sourcePath, result.Result.Session.File.Path)
	assert.Equal("abc123", result.Result.Session.File.Hash)
	assert.Equal("parse question", result.Result.Session.FirstMessage)
	assert.Len(result.Result.Messages, 2)
}

func TestClaudeProviderParseAdoptsAITitle(t *testing.T) {
	tests := []struct {
		name  string
		extra []string
		want  string
	}{
		{name: "ai title only", extra: []string{`{"type":"ai-title","aiTitle":" Generated Title "}`}, want: "Generated Title"},
		{name: "last title wins", extra: []string{`{"type":"ai-title","aiTitle":"Old"}`, `{"type":"ai-title","aiTitle":"New"}`}, want: "New"},
		{name: "rename before title", extra: []string{`{"type":"system","content":"<command-name>/rename</command-name><command-args>Renamed</command-args>"}`, `{"type":"ai-title","aiTitle":"Generated"}`}, want: "Renamed"},
		{name: "rename after title", extra: []string{`{"type":"ai-title","aiTitle":"Generated"}`, `{"type":"system","content":"<command-name>/rename</command-name><command-args>Renamed</command-args>"}`}, want: "Renamed"},
		{name: "empty rename clears title", extra: []string{`{"type":"ai-title","aiTitle":"Generated"}`, `{"type":"system","content":"<command-name>/rename</command-name><command-args></command-args>"}`}, want: ""},
		{name: "invalid titles keep fallback", extra: []string{`{"type":"ai-title","aiTitle":""}`, `{"type":"ai-title","aiTitle":42}`, `{"type":"ai-title"}`, `{malformed`}, want: ""},
		{name: "compatible fields stay decoys", extra: []string{`{"type":"custom-title","customTitle":"Custom"}`, `{"type":"user","sessionName":"Session"}`}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			path := filepath.Join(root, "project", "session.jsonl")
			lines := append([]string{
				`{"type":"user","timestamp":"2026-01-01T00:00:00Z","uuid":"u1","message":{"content":"First question"}}`,
				`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"text","text":"Answer"}]}}`,
			}, tt.extra...)
			writeSourceFile(t, path, strings.Join(lines, "\n")+"\n")
			provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			require.True(ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(err)
			require.Len(outcome.Results, 1)
			assert.Equal(tt.want, outcome.Results[0].Result.Session.SessionName)
			t.Logf("SessionName=%q", outcome.Results[0].Result.Session.SessionName)
			assert.Equal("First question", outcome.Results[0].Result.Session.FirstMessage)
			require.Len(outcome.Results[0].Result.Messages, 2)
			assert.Equal("Answer", outcome.Results[0].Result.Messages[1].Content)
		})
	}
	t.Run("fork title fans out", func(t *testing.T) {
		require := require.New(t)

		root := t.TempDir()
		path := filepath.Join(root, "project", "fork.jsonl")
		lines := []string{buildMetadataLine(map[string]any{
			"type": "user", "uuid": "root", "message": map[string]any{"content": "Fork question"},
		}), buildMetadataLine(map[string]any{
			"type": "assistant", "uuid": "root-answer", "parentUuid": "root",
			"message": map[string]any{"content": []map[string]any{{"type": "text", "text": "Root answer"}}},
		})}
		for _, prefix := range []string{"first", "second"} {
			parent := "root-answer"
			for i := 1; i <= 4; i++ {
				user := fmt.Sprintf("%s-user-%d", prefix, i)
				assistant := fmt.Sprintf("%s-assistant-%d", prefix, i)
				lines = append(lines, buildMetadataLine(map[string]any{
					"type": "user", "uuid": user, "parentUuid": parent,
					"message": map[string]any{"content": fmt.Sprintf("%s question %d", prefix, i)},
				}), buildMetadataLine(map[string]any{
					"type": "assistant", "uuid": assistant, "parentUuid": user,
					"message": map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("%s answer %d", prefix, i)}}},
				}))
				parent = assistant
			}
		}
		lines = append(lines, `{"type":"ai-title","aiTitle":"Fork title"}`)
		writeSourceFile(t, path, strings.Join(lines, "\n")+"\n")
		provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
		require.True(ok)
		sources, err := provider.Discover(t.Context())
		require.NoError(err)
		require.Len(sources, 1)
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
		require.NoError(err)
		require.Len(outcome.Results, 2)
		for _, result := range outcome.Results {
			assert.Equal(t, "Fork title", result.Result.Session.SessionName)
		}
	})
}

func TestClaudeProviderUploadAndTitleBoundaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "upload.jsonl")
	writeSourceFile(t, path, strings.Join([]string{
		`{"type":"ai-title","aiTitle":"Uploaded title"}`,
		`{"type":"user","sessionId":"upload-session","isSidechain":false,"message":{"content":"Uploaded question"}}`,
	}, "\n")+"\n")
	results, err := parseClaudeSession(path, "uploaded-project", "devbox")
	require.NoError(err)
	require.Len(results, 1)
	assert.Empty(results[0].Session.SessionName)
	assert.Equal("uploaded-project", results[0].Session.Project)
	assert.Equal("Uploaded question", results[0].Session.FirstMessage)
}

func TestClaudeProviderIncrementalAITitleEscalation(t *testing.T) {
	emptyName := ""
	existingName := "Existing title"
	tests := []struct {
		name         string
		storedName   *string
		appended     string
		wantStatus   IncrementalStatus
		wantForce    bool
		wantConsumed int64
		wantMessages int
		wantContent  string
	}{
		{
			name:         "empty stored name",
			storedName:   &emptyName,
			appended:     `{"type":"ai-title","aiTitle":"Appended title"}`,
			wantStatus:   IncrementalNeedsFullParse,
			wantForce:    true,
			wantMessages: 0,
		},
		{
			name:         "existing stored name",
			storedName:   &existingName,
			appended:     `{"type":"ai-title","aiTitle":"Appended title"}`,
			wantStatus:   IncrementalApplied,
			wantConsumed: int64(len(`{"type":"ai-title","aiTitle":"Appended title"}`) + 1),
			wantMessages: 0,
		},
		{
			name:         "stored name unavailable",
			appended:     `{"type":"ai-title","aiTitle":"Appended title"}`,
			wantStatus:   IncrementalApplied,
			wantConsumed: int64(len(`{"type":"ai-title","aiTitle":"Appended title"}`) + 1),
			wantMessages: 0,
		},
		{
			name:         "empty title",
			storedName:   &emptyName,
			appended:     `{"type":"ai-title","aiTitle":""}`,
			wantStatus:   IncrementalApplied,
			wantConsumed: int64(len(`{"type":"ai-title","aiTitle":""}`) + 1),
			wantMessages: 0,
		},
		{
			name:         "non-string title",
			storedName:   &emptyName,
			appended:     `{"type":"ai-title","aiTitle":42}`,
			wantStatus:   IncrementalApplied,
			wantConsumed: int64(len(`{"type":"ai-title","aiTitle":42}`) + 1),
			wantMessages: 0,
		},
		{
			name:         "missing title field",
			storedName:   &emptyName,
			appended:     `{"type":"ai-title"}`,
			wantStatus:   IncrementalApplied,
			wantConsumed: int64(len(`{"type":"ai-title"}`) + 1),
			wantMessages: 0,
		},
		{
			name:         "malformed line",
			storedName:   &emptyName,
			appended:     `{malformed`,
			wantStatus:   IncrementalNoNewData,
			wantMessages: 0,
		},
		{
			name:         "ordinary user message",
			storedName:   &emptyName,
			appended:     testjsonl.ClaudeUserJSON("Appended question", tsEarlyS5),
			wantStatus:   IncrementalApplied,
			wantConsumed: int64(len(testjsonl.ClaudeUserJSON("Appended question", tsEarlyS5)) + 1),
			wantMessages: 1,
			wantContent:  "Appended question",
		},
	}

	statusName := func(status IncrementalStatus) string {
		switch status {
		case IncrementalNoNewData:
			return "IncrementalNoNewData"
		case IncrementalApplied:
			return "IncrementalApplied"
		case IncrementalNeedsFullParse:
			return "IncrementalNeedsFullParse"
		default:
			return "IncrementalUnsupported"
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			path := filepath.Join(root, "project", "incremental.jsonl")
			initial := claudeProviderFixture("First question")
			writeSourceFile(t, path, initial)
			info, err := os.Stat(path)
			require.NoError(err)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(err)
			_, err = f.WriteString(tt.appended + "\n")
			require.NoError(err)
			require.NoError(f.Close())
			current, err := os.Stat(path)
			require.NoError(err)
			provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
			require.True(ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "incremental"})
			require.NoError(err)
			require.True(ok)
			outcome, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
				Source: source, Fingerprint: SourceFingerprint{Key: path, Size: current.Size()},
				SessionID: "incremental", Offset: info.Size(), StartOrdinal: 2,
				StoredSessionName: tt.storedName,
			})
			require.NoError(err)
			assert.Equal(tt.wantStatus, status)
			assert.Equal(tt.wantForce, outcome.ForceReplace)
			assert.Equal(tt.wantConsumed, outcome.ConsumedBytes)
			assert.Len(outcome.Messages, tt.wantMessages)
			if tt.wantContent != "" {
				require.Len(outcome.Messages, 1)
				assert.Equal(tt.wantContent, outcome.Messages[0].Content)
			}
			t.Logf("appended=%s status=%s force_replace=%t consumed=%d messages=%d", tt.appended, statusName(status), outcome.ForceReplace, outcome.ConsumedBytes, len(outcome.Messages))
		})
	}
}

func TestClaudeProviderParseResolvesPersistedToolResultsThroughStoredPathResolver(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "demo-project"
	sessionID := "session-persisted"
	sourcePath := filepath.Join(root, projectDir, sessionID+".jsonl")
	resultPath := filepath.Join(
		root, projectDir, sessionID, "tool-results", "r1.txt",
	)
	require.NoError(os.MkdirAll(filepath.Dir(resultPath), 0o755))
	fullOutput := "resolved full output line 1\nresolved full output line 2\n"
	require.NoError(os.WriteFile(resultPath, []byte(fullOutput), 0o644))
	// The transcript references the companion through a canonical stored
	// spelling that no longer matches the on-disk layout; only the caller's
	// StoredPathResolver can map it back to the physical companion file.
	storedPath := "devbox:" + resultPath
	persistedNotice := "<persisted-output>\nOutput too large (35B). Full output saved to: " +
		storedPath + "\n</persisted-output>"
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/work/demo"}`,
		`{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make"}}]}}`,
		`{"type":"user","timestamp":"2026-08-13T12:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + strconv.Quote(persistedNotice) + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + strconv.Quote(storedPath) + `,"persistedOutputSize":35}}`,
	}, "\n") + "\n"
	writeSourceFile(t, sourcePath, content)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
		Machine:     "devbox",
		StoredPathResolver: func(path string) (string, bool) {
			if unqualified, found := strings.CutPrefix(path, "devbox:"); found {
				return unqualified, true
			}
			return "", false
		},
	})

	require.NoError(err)
	require.Len(outcome.Results, 1)
	messages := outcome.Results[0].Result.Messages
	require.Len(messages, 3)
	toolResults := messages[2].ToolResults
	require.Len(toolResults, 1)
	assert.Equal(len(fullOutput), toolResults[0].ContentLength)
	assert.Equal(fullOutput, DecodeContent(toolResults[0].ContentRaw),
		"a stored persisted-output path must resolve through ParseRequest.StoredPathResolver")
}

func TestClaudePlanRawCaptureCarriesLineageSiblingInputs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := filepath.Join(root, "demo-project")
	require.NoError(os.MkdirAll(projectDir, 0o755))
	origPath := filepath.Join(projectDir, "orig-1111.jsonl")
	forkPath := filepath.Join(projectDir, "fork-2222.jsonl")
	unrelatedPath := filepath.Join(projectDir, "unrelated-3333.jsonl")
	require.NoError(os.WriteFile(origPath, []byte(lineageOriginalContent()), 0o644))
	require.NoError(os.WriteFile(forkPath, []byte(lineageForkContent()), 0o644))
	unrelated := strings.Join([]string{
		lineageUserLine("z1", "", "2026-02-01T10:00:00Z", "unrelated-3333", "", "other root"),
		lineageAssistantLine("z2", "z1", "2026-02-01T10:00:05Z", "unrelated-3333", "", "msg_z", "other answer", 3),
	}, "\n") + "\n"
	require.NoError(os.WriteFile(unrelatedPath, []byte(unrelated), 0o644))
	subagentDir := filepath.Join(projectDir, "fork-2222", "subagents", "agent-4444")
	require.NoError(os.MkdirAll(subagentDir, 0o755))
	subagentPath := filepath.Join(subagentDir, "agent-4444.jsonl")
	require.NoError(os.WriteFile(subagentPath, []byte(lineageForkContent()), 0o644))

	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 4)
	findSource := func(suffix string) SourceRef {
		for _, source := range sources {
			if strings.HasSuffix(source.Key, suffix) {
				return source
			}
		}
		t.Fatalf("source %s not discovered", suffix)
		return SourceRef{}
	}

	// The bg fork replays the original's chain, so its capture plan must carry
	// the original transcript as an appendable lineage input: the original
	// can keep growing independently of the fork.
	forkPlan, supported, err := ResolveRawCapturePlan(t.Context(), provider, findSource("fork-2222.jsonl"))
	require.NoError(err)
	require.True(supported)
	require.Len(forkPlan.Entries, 2)
	assert.Equal("demo-project/fork-2222.jsonl", forkPlan.Entries[0].Path)
	assert.True(forkPlan.Entries[0].Appendable)
	assert.Equal("demo-project/orig-1111.jsonl", forkPlan.Entries[1].Path)
	assert.True(forkPlan.Entries[1].Appendable,
		"sibling lineage inputs can grow independently of the fork")
	// Windows temp roots can surface the same physical file under both 8.3
	// and expanded path spellings, so compare file identity, not strings.
	assertSameFile := func(want, got string) {
		wantInfo, err := os.Stat(want)
		require.NoError(err)
		gotInfo, err := os.Stat(got)
		require.NoError(err)
		assert.True(os.SameFile(wantInfo, gotInfo),
			"expected LocalPath %q to refer to %q", got, want)
	}
	assertSameFile(forkPath, forkPlan.Entries[0].LocalPath)
	assertSameFile(origPath, forkPlan.Entries[1].LocalPath)

	// The interactive original never trims, so its plan must not carry
	// siblings; the unrelated-root transcript and subagent transcripts never
	// participate in lineage either.
	origPlan, supported, err := ResolveRawCapturePlan(t.Context(), provider, findSource("orig-1111.jsonl"))
	require.NoError(err)
	require.True(supported)
	require.Len(origPlan.Entries, 1)
	assert.Equal("demo-project/orig-1111.jsonl", origPlan.Entries[0].Path)
	assert.True(origPlan.Entries[0].Appendable)

	subagentPlan, supported, err := ResolveRawCapturePlan(
		t.Context(), provider, findSource("agent-4444.jsonl"))
	require.NoError(err)
	require.True(supported)
	require.Len(subagentPlan.Entries, 1)
	assert.True(subagentPlan.Entries[0].Appendable)
}

func TestClaudeProviderParseIncremental(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "inc.jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello world", tsEarly),
		testjsonl.ClaudeAssistantJSON("hi there", tsEarlyS1),
	)
	writeSourceFile(t, sourcePath, initial)
	info, err := os.Stat(sourcePath)
	require.NoError(err)

	appended := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("follow up", tsEarlyS5),
		testjsonl.ClaudeAssistantJSON("got it", tsLate),
	)
	f, err := os.OpenFile(sourcePath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(appended)
	require.NoError(err)
	require.NoError(f.Close())
	currentInfo, err := os.Stat(sourcePath)
	require.NoError(err)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "inc",
	})
	require.NoError(err)
	require.True(ok)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  SourceFingerprint{Key: sourcePath, Size: currentInfo.Size()},
			SessionID:    "inc",
			Offset:       info.Size(),
			StartOrdinal: 2,
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	assert.Equal("inc", outcome.SessionID)
	assert.Equal(int64(len(appended)), outcome.ConsumedBytes)
	require.Len(outcome.Messages, 2)
	assert.Equal(2, outcome.Messages[0].Ordinal)
	assert.Equal(RoleUser, outcome.Messages[0].Role)
	assert.Contains(outcome.Messages[0].Content, "follow up")
	assert.Equal(3, outcome.Messages[1].Ordinal)
	assert.Equal(RoleAssistant, outcome.Messages[1].Role)
	assert.Contains(outcome.Messages[1].Content, "got it")
}

func TestClaudeProviderParseIncrementalWebSearchResultNeedsFullParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(
		root, "-Users-dev-code-demo", "inc-web-search.jsonl")
	initial := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2026-07-30T10:00:00Z",`+
			`"uuid":"u1","message":{"content":"find something"}}`,
		`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",`+
			`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",`+
			`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",`+
			`"id":"toolu_s1","name":"WebSearch","input":{"query":"q"}}],`+
			`"usage":{"input_tokens":10,"output_tokens":5,`+
			`"server_tool_use":{"web_search_requests":0}}}}`,
	)
	writeSourceFile(t, sourcePath, initial)

	appended := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2026-07-30T10:00:04Z",` +
			`"uuid":"u2","parentUuid":"a1","message":{"content":[{"type":` +
			`"tool_result","tool_use_id":"toolu_s1","content":"hits"}]},` +
			`"toolUseResult":{"query":"q","searchCount":1}}`,
	)
	f, err := os.OpenFile(sourcePath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(appended)
	require.NoError(err)
	require.NoError(f.Close())

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "inc-web-search",
	})
	require.NoError(err)
	require.True(ok)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source: source,
			Fingerprint: SourceFingerprint{
				Key:  sourcePath,
				Size: int64(len(initial) + len(appended)),
			},
			SessionID:     "inc-web-search",
			Offset:        int64(len(initial)),
			StartOrdinal:  2,
			LastEntryUUID: "a1",
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace,
		"the stored assistant row must be rewritten with the billed search")
}

func TestClaudeProviderParseIncrementalPreservesLinkWithoutMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "inc-link-only.jsonl")
	initial := claudeProviderFixture("hello world")
	writeSourceFile(t, sourcePath, initial)

	appended := `{"type":"user","isMeta":true,"timestamp":"2024-01-01T10:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_link_only","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"linkonly"}}` + "\n"
	f, err := os.OpenFile(sourcePath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(appended)
	require.NoError(err)
	require.NoError(f.Close())

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "inc-link-only",
	})
	require.NoError(err)
	require.True(ok)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source: source,
			Fingerprint: SourceFingerprint{
				Key:  sourcePath,
				Size: int64(len(initial) + len(appended)),
			},
			SessionID:    "inc-link-only",
			Offset:       int64(len(initial)),
			StartOrdinal: 2,
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	assert.Empty(outcome.Messages)
	assert.Equal(int64(len(appended)), outcome.ConsumedBytes)
	assert.Equal([]ClaudeSubagentLink{{
		ToolUseID:         "toolu_link_only",
		SubagentSessionID: "agent-linkonly",
	}}, outcome.SubagentLinks)
}

func TestClaudeProviderParseIncrementalTruncatedNeedsFullParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "truncated.jsonl")
	initial := claudeProviderFixture("hello world")
	writeSourceFile(t, sourcePath, initial)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "truncated",
	})
	require.NoError(err)
	require.True(ok)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: SourceFingerprint{Key: sourcePath, Size: int64(len(initial) / 2)},
			SessionID:   "truncated",
			Offset:      int64(len(initial)),
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
}

func TestClaudeProviderParseIncrementalEmptyTruncationNeedsFullParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "empty-truncated.jsonl")
	initial := claudeProviderFixture("hello world")
	writeSourceFile(t, sourcePath, initial)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "empty-truncated",
	})
	require.NoError(err)
	require.True(ok)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: SourceFingerprint{Key: sourcePath, Size: 0},
			SessionID:   "empty-truncated",
			Offset:      int64(len(initial)),
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
}

func claudeProviderFixture(firstMessage string) string {
	return testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON(firstMessage, tsEarly),
		testjsonl.ClaudeAssistantJSON("Done.", tsEarlyS1),
	)
}
