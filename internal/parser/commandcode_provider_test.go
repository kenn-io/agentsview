package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandCodeProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "users-alice-code-sample-project")
	sourcePath := filepath.Join(projectDir, "sess_123.jsonl")
	writeSourceFile(t, sourcePath, commandCodeProviderFixture())
	writeSourceFile(t, filepath.Join(projectDir, "sess_123.checkpoints.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(projectDir, "sess_123.prompts.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentCommandCode, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Empty(t, discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~commandcode:sess_123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		FingerprintKey: sourcePath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestCommandCodeProviderWatchRootsStayBounded(t *testing.T) {
	root := t.TempDir()
	transcript := filepath.Join(root, "project", "session.jsonl")
	writeSourceFile(t, transcript, "{}\n")
	companionCalls := 0
	provider := &commandCodeProvider{
		ProviderBase: ProviderBase{
			Def:  AgentDef{Type: AgentCommandCode},
			Caps: commandCodeProviderCapabilities(),
		},
		sources: NewDirectoryJSONLSourceSet(
			AgentCommandCode,
			[]string{root},
			WithCompanionFiles(func(path string) []string {
				companionCalls++
				return []string{commandCodeMetaCompanionPath(path)}
			}),
		),
	}

	assert.Equal(t, CapabilitySupported, provider.Capabilities().Source.WatchRoots)
	assert.Implements(t, (*WatchRootPlanner)(nil), provider)
	roots, err := ResolveWatchRoots(context.Background(), provider)
	require.NoError(t, err)
	assert.Equal(t, []WatchRoot{{
		Path:        root,
		Recursive:   true,
		DebounceKey: string(AgentCommandCode) + ":jsonl:" + root,
	}}, roots)
	assert.Zero(t, companionCalls,
		"bounded root scheduling must not discover transcript companions")

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, "session.meta.json")
	assert.Equal(t, 1, companionCalls,
		"legacy WatchPlan must retain companion glob discovery")
}

func TestCommandCodeProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	realProjectDir := filepath.Join(t.TempDir(), "real-project")
	linkProjectDir := filepath.Join(root, "linked-project")
	// Populate the target directory before symlinking so Windows records a
	// directory symlink. Symlinking a not-yet-existent target yields a file
	// symlink there, which discovery cannot descend into.
	writeSourceFile(t, filepath.Join(realProjectDir, "sess_123.jsonl"), commandCodeProviderFixture())
	if err := os.Symlink(realProjectDir, linkProjectDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	sourcePath := filepath.Join(linkProjectDir, "sess_123.jsonl")

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess_123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestCommandCodeProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "sess_123.jsonl")
	transcript := commandCodeProviderFixture()
	writeSourceFile(t, sourcePath, transcript)

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "commandcode:sess_123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

// TestCommandCodeProviderParseUsesSharedFingerprintHash verifies that file_hash
// is the shared fingerprint hash, which folds the .meta.json companion via
// WithCompanionFiles, rather than a bespoke transcript-only hash. A title-only
// .meta.json change therefore moves both the fingerprint and the stored hash.
func TestCommandCodeProviderParseUsesSharedFingerprintHash(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "sess_123.jsonl")
	transcript := commandCodeProviderFixture()
	writeSourceFile(t, sourcePath, transcript)
	writeSourceFile(t, commandCodeMetaCompanionPath(sourcePath), `{"title":"Renamed"}`)

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	require.NotEmpty(t, fingerprint.Hash)
	transcriptHash := fmt.Sprintf("%x", sha256.Sum256([]byte(transcript)))
	require.NotEqual(t, transcriptHash, fingerprint.Hash,
		"the .meta.json companion must participate in the fingerprint hash")

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, fingerprint.Hash, outcome.Results[0].Result.Session.File.Hash,
		"parse threads the shared fingerprint hash, not a transcript-only hash")
}

func commandCodeProviderFixture() string {
	return `{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess_123","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"gitBranch":"feature/command-code","metadata":{"version":2,"cwd":"/Users/alice/code/sample-project"}}
{"id":"m2","timestamp":"2026-06-01T10:00:03Z","sessionId":"sess_123","role":"assistant","content":[{"type":"text","text":"The error is in the startup path."}],"gitBranch":"feature/command-code","metadata":{"version":2}}`
}

// TestCommandCodeProviderFingerprintIncludesContentHash guards that the Command
// Code provider computes a full-file content hash. The legacy per-agent parse
// stored a file_hash; without WithContentHashing the provider fingerprint hash
// is empty and a resync clears the stored file_hash to NULL. Toggle-provable:
// removing WithContentHashing from newCommandCodeSourceSet fails here.
func TestCommandCodeProviderFingerprintIncludesContentHash(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "users-alice-code-sample-project", "sess_123.jsonl")
	writeSourceFile(t, sourcePath, commandCodeProviderFixture())

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fp, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	require.NotEmpty(t, fp.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fp,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, fp.Hash, outcome.Results[0].Result.Session.File.Hash)
}
