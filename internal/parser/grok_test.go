package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeGrokFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

func grokSummaryPath(root, cwdKey, sessionID string) string {
	return filepath.Join(root, cwdKey, sessionID, "summary.json")
}

func newGrokTestProvider(t *testing.T, root string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentGrok, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	return provider
}

func TestGrokProviderSummarySource(t *testing.T) {
	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Fix parser regression",
		"firstPrompt": "Investigate the failing Grok session import",
		"modelId": "grok-code-fast",
		"createdAt": "2026-07-08T10:00:00Z",
		"updatedAt": "2026-07-08T10:30:00Z",
		"lastActiveAt": "2026-07-08T10:31:00Z",
		"hostname": "devbox",
		"numMessages": 6,
		"worktreeLabel": "agentsview"
	}`)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", "sess-1", "signals.json"), `{
		"tokenUsage": {
			"totalOutputTokens": 321,
			"peakContextTokens": 4096
		}
	}`)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", "sess-1", "updates.jsonl"), "{}\n")

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	session := outcome.Results[0].Result.Session
	assert.Equal(t, "grok:sess-1", session.ID)
	assert.Equal(t, AgentGrok, session.Agent)
	assert.Equal(t, "sess-1", session.SourceSessionID)
	assert.Equal(t, "grok-summary-v1", session.SourceVersion)
	assert.Equal(t, "summary", session.TranscriptFidelity)
	assert.Equal(t, "Investigate the failing Grok session import", session.FirstMessage)
	assert.Equal(t, "Fix parser regression", session.SessionName)
	assert.Equal(t, "agentsview", session.Project)
	assert.Equal(t, 6, session.MessageCount)
	assert.Equal(t, 321, session.TotalOutputTokens)
	assert.Equal(t, 4096, session.PeakContextTokens)
	assert.Equal(t, filepath.Clean(grokSummaryPath(root, "cwd-key", "sess-1")), filepath.Clean(session.File.Path))
}

func TestGrokProviderFindSource(t *testing.T) {
	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Find source",
		"firstPrompt": "Locate the Grok source",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)

	provider := newGrokTestProvider(t, root)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess-1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, filepath.Clean(grokSummaryPath(root, "cwd-key", "sess-1")), filepath.Clean(source.FingerprintKey))

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "missing",
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGrokProviderFingerprintTracksSessionFiles(t *testing.T) {
	root := t.TempDir()
	summary := grokSummaryPath(root, "cwd-key", "sess-1")
	signals := filepath.Join(root, "cwd-key", "sess-1", "signals.json")
	updates := filepath.Join(root, "cwd-key", "sess-1", "updates.jsonl")
	chat := filepath.Join(root, "cwd-key", "sess-1", "chat_history.jsonl")
	unrelated := filepath.Join(root, "cwd-key", "sess-1", "notes.txt")
	writeGrokFixtureFile(t, summary, `{"summary":"Fingerprint","firstPrompt":"hello","createdAt":"2026-07-08T10:00:00Z"}`)
	writeGrokFixtureFile(t, signals, `{"tokenUsage":{"totalOutputTokens":1}}`)
	writeGrokFixtureFile(t, updates, "{}\n")
	writeGrokFixtureFile(t, chat, "{}\n")
	writeGrokFixtureFile(t, unrelated, "ignored")

	provider := newGrokTestProvider(t, root)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess-1",
	})
	require.NoError(t, err)
	require.True(t, ok)

	base, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	writeGrokFixtureFile(t, updates, "{\"delta\":1}\n")
	afterUpdates, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, base.Hash, afterUpdates.Hash)

	writeGrokFixtureFile(t, unrelated, "still ignored")
	afterUnrelated, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, afterUpdates.Hash, afterUnrelated.Hash)

	writeGrokFixtureFile(t, signals, `{"tokenUsage":{"totalOutputTokens":2}}`)
	afterSignals, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterUpdates.Hash, afterSignals.Hash)
}

func TestGrokProviderArtifactBoundaries(t *testing.T) {
	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Valid",
		"firstPrompt": "valid prompt",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", "not.a.grok.session", "summary.json"), `{
		"summary": "Ignored",
		"firstPrompt": "ignored",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-bad"), `{not json`)

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path: filepath.Join(root, "cwd-key", "not.a.grok.session", "signals.json"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess-bad",
	})
	require.NoError(t, err)
	require.True(t, ok)
	_, err = provider.Parse(context.Background(), ParseRequest{Source: source})
	require.Error(t, err)
}

func TestGrokProviderRegistry(t *testing.T) {
	def, ok := AgentByType(AgentGrok)
	require.True(t, ok)
	assert.Equal(t, "GROK_DIR", def.EnvVar)
	assert.Equal(t, "grok_dirs", def.ConfigKey)
	assert.Equal(t, "grok:", def.IDPrefix)
	assert.Equal(t, []string{".grok/sessions"}, def.DefaultDirs)

	factory, ok := ProviderFactoryByType(AgentGrok)
	require.True(t, ok)
	assert.Equal(t, AgentGrok, factory.Definition().Type)

	mode, ok := ProviderMigrationModes()[AgentGrok]
	require.True(t, ok)
	assert.Equal(t, ProviderMigrationProviderAuthoritative, mode)
}
