package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKimiProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	legacyPath := filepath.Join(root, "abc123", "uuid-1", "wire.jsonl")
	newPath := filepath.Join(
		root,
		"wd_kimi-code_057f5c09ee3f",
		"session_uuid-2",
		"agents",
		"main",
		"wire.jsonl",
	)
	invalidPath := filepath.Join(
		root,
		"wd_kimi-code_057f5c09ee3f",
		"session_uuid-3",
		"agents",
		"sub agent",
		"wire.jsonl",
	)
	writeSourceFile(t, legacyPath, kimiProviderFixture("legacy question"))
	writeSourceFile(t, newPath, kimiProviderFixture("new layout question"))
	writeSourceFile(t, invalidPath, kimiProviderFixture("bad agent"))
	writeSourceFile(t, filepath.Join(root, "abc123", "uuid-1", "other.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "wire.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
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
	assert.Equal(AgentKimi, discovered[0].Provider)
	assert.Equal(legacyPath, discovered[0].DisplayPath)
	assert.Equal("abc123", discovered[0].ProjectHint)
	assert.Equal(newPath, discovered[1].DisplayPath)
	assert.Equal("kimi-code", discovered[1].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~kimi:abc123:uuid-1",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(legacyPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(legacyPath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.Positive(fingerprint.MTimeNS)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "wd_kimi-code_057f5c09ee3f:main:session_uuid-2",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(newPath, found.DisplayPath)

	require.NoError(os.Remove(legacyPath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: legacyPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(legacyPath, changed[0].DisplayPath)
}

func TestKimiProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	targetRoot := t.TempDir()
	targetProject := filepath.Join(targetRoot, "abc123")
	sourceProject := filepath.Join(root, "abc123")
	sourcePath := filepath.Join(sourceProject, "uuid-1", "wire.jsonl")
	writeSourceFile(
		t,
		filepath.Join(targetProject, "uuid-1", "wire.jsonl"),
		kimiProviderFixture("from symlink"),
	)
	if err := os.Symlink(targetProject, sourceProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~kimi:abc123:uuid-1",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)
}

func TestKimiProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "abc123", "uuid-1", "wire.jsonl")
	writeSourceFile(t, sourcePath, kimiProviderFixture("provider question"))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
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
	require.Len(outcome.Results, 1)
	assert.Equal(DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal("kimi:abc123:uuid-1", outcome.Results[0].Result.Session.ID)
	assert.Equal("abc123", outcome.Results[0].Result.Session.Project)
	assert.Equal("devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal("abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(outcome.Results[0].Result.Messages, 2)
}

func TestKimiProviderCwdCapabilityAndParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(
		root, "wd_kimi-code_057f5c09ee3f", "session_uuid-cwd",
		"agents", "main", "wire.jsonl",
	)
	writeSourceFile(t, sourcePath, kimiConfigUpdateCwdLine(t)+"\n"+
		`{"type":"turn.prompt","input":[{"type":"text","text":"cwd"}]}`+"\n")

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	assert.Equal(CapabilitySupported, provider.Capabilities().Content.Cwd)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal("/Users/helix/Code/mcp-hub", outcome.Results[0].Result.Session.Cwd)
}

func TestKimiProviderParseNewLayoutRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	rawID := "wd_kimi-code_057f5c09ee3f:main:session_uuid-2"
	sourcePath := filepath.Join(
		root,
		"wd_kimi-code_057f5c09ee3f",
		"session_uuid-2",
		"agents",
		"main",
		"wire.jsonl",
	)
	writeSourceFile(t, sourcePath, kimiProviderFixture("new layout provider question"))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~kimi:" + rawID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, source.DisplayPath)
	assert.Equal("kimi-code", source.ProjectHint)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	session := outcome.Results[0].Result.Session
	assert.Equal("kimi:"+rawID, session.ID)
	assert.Equal("kimi-code", session.Project)
	assert.Equal("devbox", session.Machine)
	assert.Equal(sourcePath, session.File.Path)
	assert.Equal("abc123", session.File.Hash)
	assert.Len(outcome.Results[0].Result.Messages, 2)
}

func kimiProviderFixture(firstMessage string) string {
	return `{"type":"metadata","protocol_version":"1.3"}` + "\n" +
		`{"timestamp":1704067200.0,"message":{"type":"TurnBegin","payload":{"user_input":[{"type":"text","text":"` + firstMessage + `"}]}}}` + "\n" +
		`{"timestamp":1704067201.0,"message":{"type":"ContentPart","payload":{"type":"text","text":"Done."}}}` + "\n" +
		`{"timestamp":1704067202.0,"message":{"type":"TurnEnd","payload":{}}}` + "\n"
}

// TestKimiProviderFingerprintIncludesContentHash guards that the Kimi provider
// computes a full-file content hash. The legacy per-agent parse stored a
// file_hash; without WithContentHashing the provider fingerprint hash is empty
// and a resync clears the stored file_hash to NULL. Toggle-provable: removing
// WithContentHashing from newKimiSourceSet makes fp.Hash empty and fails here.
func TestKimiProviderFingerprintIncludesContentHash(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "abc123", "uuid-1", "wire.jsonl")
	writeSourceFile(t, sourcePath, kimiProviderFixture("inspect logs"))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{root}})
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
