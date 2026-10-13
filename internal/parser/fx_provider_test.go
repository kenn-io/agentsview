package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFxProviderSessionIDs(t *testing.T) {
	for _, test := range []struct {
		id    string
		valid bool
	}{
		{"session.v3", true},
		{".hidden-session", true},
		{"session..branch", true},
		{strings.Repeat("a", 255), true},
		{strings.Repeat("a", 256), false},
		{"v2", false},
		{"V2", false},
		{"invalid id", false},
	} {
		t.Run(test.id, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, test.id)
			// Overlong IDs exercise the length rule through parent_id below.
			if len(test.id) <= 255 {
				writeSourceFile(t, filepath.Join(dir, "events.jsonl"), `{"schema_version":3,"event":{"user":{"text":"Hello"}}}`)
				provider, ok := NewProvider(AgentFx, ProviderConfig{Roots: []string{root}})
				require.True(t, ok)
				sources, err := provider.Discover(t.Context())
				require.NoError(t, err)
				_, found, err := provider.FindSource(t.Context(), FindSourceRequest{FullSessionID: "fx:" + test.id})
				require.NoError(t, err)
				assert.Equal(t, test.valid, found)
				if test.valid {
					require.Len(t, sources, 1)
				} else {
					assert.Empty(t, sources)
				}
			}
			child := filepath.Join(root, "child")
			writeSourceFile(t, filepath.Join(child, "session.json"), `{"schema_version":4}`)
			writeSourceFile(t, filepath.Join(child, "events.jsonl"), `{"schema_version":3,"event":{"user":{"text":"Child task"}}}`)
			writeSourceFile(t, filepath.Join(child, "subagent", "owner.json"), fmt.Sprintf(`{"parent_id":%q}`, test.id))
			session, _, err := parseFxSession(t.Context(), filepath.Join(child, "events.jsonl"), "local")
			require.NoError(t, err)
			require.NotNil(t, session)
			if test.valid {
				assert.Equal(t, "fx:"+test.id, session.ParentSessionID)
			} else {
				assert.Empty(t, session.ParentSessionID)
			}
		})
	}
}

func TestFxProviderDiscoveryAndParse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.a", "events.jsonl")
	writeSourceFile(t, path, `{"schema_version":3,"event":{"user":{"text":"Hello"}}}`)
	for _, ignored := range []string{"events.jsonl", "session.a/other.jsonl", "session.a/subagent/events.jsonl", "session.a/tool-results/output.jsonl"} {
		writeSourceFile(t, filepath.Join(root, filepath.FromSlash(ignored)), `{}`)
	}
	provider, ok := NewProvider(AgentFx, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)
	located, ok, err := provider.FindSource(t.Context(), FindSourceRequest{FullSessionID: "fx:session.a"})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, located.DisplayPath)
	writeSourceFile(t, filepath.Join(root, "session.a", "session.json"), `{"schema_version":4,"title":"Before","workspace_root":"/tmp/demo","created_at_ms":1000,"updated_at_ms":2000}`)
	writeSourceFile(t, filepath.Join(root, "session.a", "usage-v2.json"), `{"snapshot":{"models":[{"model":"test","input_tokens":5,"output_tokens":2}]}}`)
	writeSourceFile(t, filepath.Join(root, "session.a", "subagent", "owner.json"), `{"parent_id":"parent.a"}`)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "fx:session.a", result.Session.ID)
	assert.Equal(t, fingerprint.Hash, result.Session.File.Hash)
	assert.NotEmpty(t, fingerprint.Hash)
}

func TestFxCompanionEqualLengthRewrites(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "session.a", "events.jsonl")
	writeSourceFile(t, file, `{"schema_version":3,"event":{"user":{"text":"Hello"}}}`)
	newer := time.Unix(2000, 0)
	older := time.Unix(1000, 0)
	require.NoError(t, os.Chtimes(file, newer, newer))
	for _, test := range []struct{ path, before, after string }{
		{"session.json", `{"schema_version":4,"title":"Before"}`, `{"schema_version":4,"title":"After!"}`},
		{"usage-v2.json", `{"snapshot":{"models":[{"model":"test","input_tokens":5}]}}`, `{"snapshot":{"models":[{"model":"test","input_tokens":9}]}}`},
		{"subagent/owner.json", `{"parent_id":"parent.a"}`, `{"parent_id":"parent-b"}`},
	} {
		t.Run(test.path, func(t *testing.T) {
			companion := filepath.Join(filepath.Dir(file), filepath.FromSlash(test.path))
			writeSourceFile(t, companion, test.before)
			require.NoError(t, os.Chtimes(companion, older, older))
			provider, ok := NewProvider(AgentFx, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			before, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err)
			writeSourceFile(t, companion, test.after)
			require.NoError(t, os.Chtimes(companion, older, older))
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: companion, EventKind: "write", WatchRoot: root})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, file, changed[0].DisplayPath)
			after, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err)
			assert.Equal(t, before.Size, after.Size)
			assert.Equal(t, before.MTimeNS, after.MTimeNS)
			assert.NotEqual(t, before.Hash, after.Hash)
		})
	}
}

func TestFxParseSourceSize(t *testing.T) {
	for _, test := range []struct {
		name string
		refs string
		want int64
	}{
		{"repeated artifact", `{"schema_version":3,"event":{"tool_result":{"call_id":"one","preview":"Full","artifact_ref":"result.txt","stored_bytes":16}}}
{"schema_version":3,"event":{"tool_result":{"call_id":"two","preview":"Full","artifact_ref":"result.txt","stored_bytes":16}}}`, 32},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "session-a")
			metadata := `{"schema_version":4}`
			writeSourceFile(t, filepath.Join(dir, "session.json"), metadata)
			writeSourceFile(t, filepath.Join(dir, "events.jsonl"), test.refs)
			writeSourceFile(t, filepath.Join(dir, "tool-results", "result.txt"), "Full tool output")
			provider, ok := NewProvider(AgentFx, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			size, err := provider.(ParseSourceSizer).ParseSourceSize(t.Context(), sources[0])
			require.NoError(t, err)
			assert.Equal(t, int64(len(metadata)+len(test.refs))+test.want, size)
		})
	}
}
