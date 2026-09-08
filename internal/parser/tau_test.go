package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTauTestSource(t *testing.T, name, content string) (string, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project.with-hyphen_and-dots")
	require.NoError(t, os.MkdirAll(project, 0o755))
	path := filepath.Join(project, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return root, path
}

func parseTauTestSource(t *testing.T, name, content string) ParseResult {
	t.Helper()
	root, path := writeTauTestSource(t, name, content)
	provider, ok := NewProvider(AgentTau, ProviderConfig{
		Roots: []string{root}, Machine: "test-machine",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0], Fingerprint: SourceFingerprint{Key: path},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	return outcome.Results[0].Result
}

func tauLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func tauBaseEntries() []string {
	return []string{
		`{"id":"info","type":"session_info","timestamp":1700000000.25,"created_at":1700000000.25,"cwd":"/tmp/project"}`,
		`{"id":"model","parent_id":"info","type":"model_change","timestamp":1700000000.3,"model":"fallback-model","provider":"runtime"}`,
		`{"id":"u1","parent_id":"model","type":"message","timestamp":1700000001.1,"message":{"role":"user","content":"first request","timestamp":1700000001100}}`,
		`{"id":"a1","parent_id":"u1","type":"message","timestamp":1700000002.2,"message":{"role":"assistant","content":[{"type":"thinking","thinking":"think"},{"type":"text","text":"answer"},{"type":"toolCall","id":"tool-1","name":"read","arguments":{"path":"a.txt"}}],"model":"chosen-model","usage":{"input":10,"output":5,"cacheRead":2,"cacheWrite":3},"stopReason":"toolUse","timestamp":1700000002200}}`,
		`{"id":"tr1","parent_id":"a1","type":"message","timestamp":1700000003.3,"message":{"role":"toolResult","toolCallId":"tool-1","toolName":"read","content":[{"type":"text","text":"file contents"}],"timestamp":1700000003300}}`,
	}
}

func TestTauParsesActiveLeafAndCanonicalFields(t *testing.T) {
	lines := tauBaseEntries()
	lines = append(lines,
		`{"id":"old","parent_id":"u1","type":"message","timestamp":1700000004,"message":{"role":"assistant","content":"abandoned","usage":{"input":99,"output":99}}}`,
		`{"id":"u2","parent_id":"tr1","type":"message","timestamp":1700000005,"message":{"role":"user","content":[{"type":"text","text":"second"}],"timestamp":1700000005100}}`,
		`{"id":"a2","parent_id":"u2","type":"message","timestamp":1700000006,"message":{"role":"assistant","content":"final","usage":{"input":20,"output":7},"timestamp":1700000006100}}`,
		`{"id":"leaf","parent_id":"a2","type":"leaf","entry_id":"a2","timestamp":1700000007}`,
	)
	result := parseTauTestSource(t, "session.with-hyphen.jsonl", tauLines(lines...))

	assert.Equal(t, "tau:session.with-hyphen", result.Session.ID)
	assert.Equal(t, AgentTau, result.Session.Agent)
	assert.Equal(t, "test-machine", result.Session.Machine)
	assert.Equal(t, "/tmp/project", result.Session.Cwd)
	assert.Equal(t, "project", result.Session.Project)
	assert.Equal(t, "first request", result.Session.FirstMessage)
	assert.Equal(t, "first request", result.Session.SessionName)
	assert.Equal(t, 5, result.Session.MessageCount)
	assert.Equal(t, 2, result.Session.UserMessageCount)
	assert.Equal(t, 20, result.Session.PeakContextTokens)
	assert.Equal(t, 5+7, result.Session.TotalOutputTokens)
	assert.True(t, result.Session.HasPeakContextTokens)
	assert.True(t, result.Session.HasTotalOutputTokens)
	assert.Equal(t, time.UnixMilli(1700000001100).UTC(), result.Messages[0].Timestamp)
	assert.Equal(t, time.Unix(1700000000, 250000000).UTC(), result.Session.StartedAt)
	assert.Equal(t, "chosen-model", result.Messages[1].Model)
	assert.Equal(t, "toolUse", result.Messages[1].StopReason)
	assert.Equal(t, "think", result.Messages[1].ThinkingText)
	assert.True(t, result.Messages[1].HasThinking)
	assert.Equal(t, "tool-1", result.Messages[1].ToolCalls[0].ToolUseID)
	assert.JSONEq(t, `{"path":"a.txt"}`, result.Messages[1].ToolCalls[0].InputJSON)
	assert.Equal(t, "tool-1", result.Messages[2].ToolResults[0].ToolUseID)
	assert.Equal(t, "file contents", DecodeContent(result.Messages[2].ToolResults[0].ContentRaw))
	assert.Empty(t, result.Messages[1].ProviderID)
	assert.NotContains(t, result.Messages[3].Content, "abandoned")
}

func TestTauSourceDerivedReplayAndMetadata(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		want      []string
		wantCount int
		wantName  string
		wantErr   string
		wantEmpty bool
	}{
		{
			name: "no leaf replays file order",
			content: tauLines(
				`{"id":"info","type":"session_info","cwd":"/tmp/p"}`,
				`{"id":"u","type":"message","message":{"role":"user","content":"u"}}`,
				`{"id":"a","parent_id":"u","type":"message","message":{"role":"assistant","content":"a"}}`,
			),
			want: []string{"u", "a"}, wantCount: 2,
		},
		{
			name: "empty leaf clears messages",
			content: tauLines(
				`{"id":"info","type":"session_info","cwd":"/tmp/p"}`,
				`{"id":"u","type":"message","message":{"role":"user","content":"u"}}`,
				`{"id":"leaf","type":"leaf","entry_id":null}`,
			),
			wantCount: 0, wantEmpty: true,
		},
		{
			name: "missing parent detaches selected root",
			content: tauLines(
				`{"id":"info","type":"session_info","cwd":"/tmp/p"}`,
				`{"id":"u","parent_id":"external","type":"message","message":{"role":"user","content":"u"}}`,
				`{"id":"leaf","type":"leaf","entry_id":"u"}`,
			),
			want: []string{"u"}, wantCount: 1,
		},
		{
			name: "label wins over title",
			content: tauLines(
				`{"id":"info","type":"session_info","title":"title","cwd":"/tmp/p"}`,
				`{"id":"label","type":"label","label":"label"}`,
				`{"id":"u","type":"message","message":{"role":"user","content":"u"}}`,
			),
			want: []string{"u"}, wantCount: 1, wantName: "label",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseTauTestSource(t, "source.jsonl", tc.content)
			assert.Equal(t, tc.wantCount, result.Session.MessageCount)
			if tc.wantName != "" {
				assert.Equal(t, tc.wantName, result.Session.SessionName)
			}
			if tc.wantEmpty {
				assert.Empty(t, result.Messages)
				return
			}
			got := make([]string, 0, len(result.Messages))
			for _, message := range result.Messages {
				got = append(got, message.Content)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTauSelectedAncestryErrors(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{
			name: "missing leaf",
			lines: []string{
				`{"type":"session_info"}`,
				`{"type":"leaf","entry_id":"missing"}`,
			},
		},
		{
			name: "duplicate selected id",
			lines: []string{
				`{"type":"session_info"}`,
				`{"id":"same","type":"message","message":{"role":"user","content":"one"}}`,
				`{"id":"same","type":"message","message":{"role":"user","content":"two"}}`,
				`{"type":"leaf","entry_id":"same"}`,
			},
		},
		{
			name: "cycle",
			lines: []string{
				`{"type":"session_info"}`,
				`{"id":"one","parent_id":"two","type":"message","message":{"role":"user","content":"one"}}`,
				`{"id":"two","parent_id":"one","type":"message","message":{"role":"assistant","content":"two"}}`,
				`{"type":"leaf","entry_id":"one"}`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := writeTauTestSource(t, "bad.jsonl", tauLines(tc.lines...))
			provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			_, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.Error(t, err)
		})
	}
}

func TestTauMapsCompactionThinkingErrorsAndUsage(t *testing.T) {
	result := parseTauTestSource(t, "usage.jsonl", tauLines(
		`{"id":"info","type":"session_info","cwd":"/tmp/p"}`,
		`{"id":"u","type":"message","message":{"role":"user","content":"request"}}`,
		`{"id":"a","parent_id":"u","type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"text","text":""}],"usage":{"input":1,"output":2,"cacheWrite":25,"cacheWrite1h":10,"reasoning":99},"errorMessage":"failed","timestamp":1700000000000}}`,
		`{"id":"c","parent_id":"a","type":"compaction","summary":"keep this summary"}`,
		`{"id":"b","parent_id":"c","type":"branch_summary","summary":"returned branch"}`,
		`{"id":"leaf","type":"leaf","entry_id":"b"}`,
	))
	require.Len(t, result.Messages, 4)
	assert.True(t, result.Messages[1].HasThinking)
	assert.Equal(t, "failed", result.Messages[1].Content)
	assert.Equal(t, 26, result.Messages[1].ContextTokens)
	assert.NotContains(t, string(result.Messages[1].TokenUsage), "cacheWrite1h")
	assert.NotContains(t, string(result.Messages[1].TokenUsage), "reasoning")
	assert.True(t, result.Messages[2].IsSystem)
	assert.True(t, result.Messages[2].IsCompactBoundary)
	assert.Equal(t, "compact_boundary", result.Messages[2].SourceSubtype)
	assert.Contains(t, result.Messages[3].Content, "returned branch")
}

func TestTauReadValidationAndFinalRecord(t *testing.T) {
	root, path := writeTauTestSource(t, "valid.jsonl", `{"id":"info","type":"session_info"}`)
	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	_, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "malformed JSON", data: []byte(`{"type":"session_info"}` + "\n{")},
		{name: "invalid UTF-8", data: []byte{'{', '"', 0xff, '"', '}'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, tc.data, 0o644))
			_, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			assert.Error(t, err)
		})
	}
}

func TestTauLineSizeLimit(t *testing.T) {
	makeRecord := func(size int) []byte {
		prefix := `{"id":"info","type":"session_info","padding":"`
		suffix := `"}`
		return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	}
	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "boundary", size: maxLineSize},
		{name: "oversized", size: maxLineSize + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, path := writeTauTestSource(t, "size.jsonl", "")
			require.NoError(t, os.WriteFile(path, makeRecord(tc.size), 0o644))
			entries, err := readTauEntries(t.Context(), path)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, entries, 1)
			_ = root
		})
	}
}

func TestTauNonTauContentSkips(t *testing.T) {
	root, path := writeTauTestSource(t, "pi.jsonl", `{"type":"session","id":"pi"}`+"\n")
	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: SourceFingerprint{Key: path}})
	require.NoError(t, err)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
}

func TestTauDefaultIdentityAndCapabilities(t *testing.T) {
	root, path := writeTauTestSource(t, "default.jsonl", `{"type":"session_info"}`+"\n")
	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: tauSessionIDFromPath(root, path),
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, found.DisplayPath)
	assert.Equal(t, CapabilitySupported, provider.Capabilities().Source.ForceReplaceOnParse)
	assert.Equal(t, CapabilitySupported, provider.Capabilities().Content.StopReason)
	assert.Equal(t, CapabilityUnsupported, provider.Capabilities().Content.Relationships)
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)
}

func TestTauCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_info"}`), 0o644))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readTauEntries(ctx, path)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, errors.Is(err, os.ErrNotExist))
}

func TestTauSessionInfoCreatedAtUsesFractionalSeconds(t *testing.T) {
	result := parseTauTestSource(t, "fractional.jsonl", `{"id":"info","type":"session_info","created_at":1700000000.125}`+"\n")
	assert.Equal(t, time.Unix(1700000000, 125000000).UTC(), result.Session.StartedAt)
}

func TestTauStartedAtFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    time.Time
	}{
		{
			name: "session info outer timestamp",
			content: tauLines(
				`{"id":"info","type":"session_info","timestamp":1700000000.5}`,
				`{"id":"message","type":"message","message":{"role":"user","content":"request","timestamp":1700000001100}}`,
			),
			want: time.Unix(1700000000, 500000000).UTC(),
		},
		{
			name: "earliest emitted message timestamp",
			content: tauLines(
				`{"id":"info","type":"session_info"}`,
				`{"id":"late","type":"message","message":{"role":"assistant","content":"late","timestamp":1700000005000}}`,
				`{"id":"early","type":"message","message":{"role":"user","content":"early","timestamp":1700000002000}}`,
			),
			want: time.UnixMilli(1700000002000).UTC(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseTauTestSource(t, "fallback.jsonl", tc.content)
			assert.Equal(t, tc.want, result.Session.StartedAt)
		})
	}
}

func TestTauUsageJSONIsStable(t *testing.T) {
	result := parseTauTestSource(t, "tokens.jsonl", tauLines(
		`{"type":"session_info"}`,
		`{"id":"u","type":"message","message":{"role":"assistant","content":"ok","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}}`,
	))
	require.Len(t, result.Messages, 1)
	assert.JSONEq(t, `{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"input_tokens":0,"output_tokens":0}`, string(result.Messages[0].TokenUsage))
}
