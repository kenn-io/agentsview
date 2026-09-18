package parser

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandCodeProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentCommandCode, discovered[0].Provider)
	assert.Equal(sourcePath, discovered[0].DisplayPath)
	assert.Empty(discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~commandcode:sess_123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		FingerprintKey: sourcePath,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	require.NoError(os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)
}

func TestCommandCodeProviderWatchRootsStayBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	transcript := filepath.Join(root, "project", "session.jsonl")
	writeSourceFile(t, transcript, "{}\n")
	companionCalls := 0
	provider := &commandCodeProvider{
		Def:  AgentDef{Type: AgentCommandCode},
		Caps: commandCodeProviderCapabilities(),
		sources: NewDirectoryJSONLSourceSet(
			AgentCommandCode,
			[]string{root},
			WithCompanionFiles(func(path string) []string {
				companionCalls++
				return []string{commandCodeMetaCompanionPath(path)}
			}),
		),
	}

	assert.Equal(CapabilitySupported, provider.Capabilities().Source.WatchRoots)
	assert.Implements((*WatchRootPlanner)(nil), provider)
	roots, err := ResolveWatchRoots(t.Context(), provider)
	require.NoError(err)
	assert.Equal([]WatchRoot{{
		Path:        root,
		Recursive:   true,
		DebounceKey: string(AgentCommandCode) + ":jsonl:" + root,
	}}, roots)
	assert.Zero(companionCalls,
		"bounded root scheduling must not discover transcript companions")

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Contains(plan.Roots[0].IncludeGlobs, "session.meta.json")
	assert.Equal(1, companionCalls,
		"legacy WatchPlan must retain companion glob discovery")
}

func TestCommandCodeProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "sess_123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)
}

func TestCommandCodeProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "sess_123.jsonl")
	transcript := commandCodeProviderFixture()
	writeSourceFile(t, sourcePath, transcript)

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	assert.Equal(DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal("commandcode:sess_123", outcome.Results[0].Result.Session.ID)
	assert.Equal("devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(fingerprint.Hash, outcome.Results[0].Result.Session.File.Hash)
	assert.Len(outcome.Results[0].Result.Messages, 2)
}

// TestCommandCodeProviderParseUsesSharedFingerprintHash verifies that file_hash
// is the shared fingerprint hash, which folds the .meta.json companion via
// WithCompanionFiles, rather than a bespoke transcript-only hash. A title-only
// .meta.json change therefore moves both the fingerprint and the stored hash.
func TestCommandCodeProviderParseUsesSharedFingerprintHash(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "sess_123.jsonl")
	transcript := commandCodeProviderFixture()
	writeSourceFile(t, sourcePath, transcript)
	writeSourceFile(t, commandCodeMetaCompanionPath(sourcePath), `{"title":"Renamed"}`)

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	require.NotEmpty(fingerprint.Hash)
	transcriptHash := fmt.Sprintf("%x", sha256.Sum256([]byte(transcript)))
	require.NotEqual(transcriptHash, fingerprint.Hash,
		"the .meta.json companion must participate in the fingerprint hash")

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
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
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "users-alice-code-sample-project", "sess_123.jsonl")
	writeSourceFile(t, sourcePath, commandCodeProviderFixture())

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fp, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	require.NotEmpty(fp.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fp,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(t, fp.Hash, outcome.Results[0].Result.Session.File.Hash)
}
