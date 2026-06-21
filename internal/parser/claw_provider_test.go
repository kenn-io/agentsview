package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenClawProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	assertClawProviderReplacesLegacyAdapter(t, AgentOpenClaw)
}

func TestQClawProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	assertClawProviderReplacesLegacyAdapter(t, AgentQClaw)
}

func TestClawProvidersOwnLegacyEntrypoints(t *testing.T) {
	for _, tt := range []struct {
		parserFile   string
		providerFile string
		symbols      []string
		calls        []string
	}{
		{
			parserFile:   "openclaw.go",
			providerFile: "claw_provider.go",
			symbols: []string{
				"func DiscoverOpenClawSessions",
				"func FindOpenClawSourceFile",
				"func ParseOpenClawSession",
			},
			calls: []string{
				"DiscoverOpenClawSessions(",
				"FindOpenClawSourceFile(",
				"ParseOpenClawSession(",
			},
		},
		{
			parserFile:   "qclaw.go",
			providerFile: "claw_provider.go",
			symbols: []string{
				"func DiscoverQClawSessions",
				"func FindQClawSourceFile",
				"func ParseQClawSession",
			},
			calls: []string{
				"DiscoverQClawSessions(",
				"FindQClawSourceFile(",
				"ParseQClawSession(",
			},
		},
	} {
		parserSource, err := os.ReadFile(tt.parserFile)
		require.NoError(t, err)
		providerSource, err := os.ReadFile(tt.providerFile)
		require.NoError(t, err)

		for _, symbol := range tt.symbols {
			assert.NotContains(t, string(parserSource), symbol)
		}

		providerText := string(providerSource)
		for _, call := range tt.calls {
			assert.NotContains(
				t,
				strings.ReplaceAll(providerText, "func "+call, ""),
				call,
			)
		}
	}
}

func TestOpenClawProviderSourceMethods(t *testing.T) {
	spec := openClawProviderTestSpec()
	assertClawProviderSourceMethods(t, spec)
}

func TestQClawProviderSourceMethods(t *testing.T) {
	spec := qClawProviderTestSpec()
	assertClawProviderSourceMethods(t, spec)
}

func TestOpenClawProviderDiscoversSymlinkedAgentDirectory(t *testing.T) {
	spec := openClawProviderTestSpec()
	assertClawProviderDiscoversSymlinkedAgentDirectory(t, spec)
}

func TestQClawProviderDiscoversSymlinkedAgentDirectory(t *testing.T) {
	spec := qClawProviderTestSpec()
	assertClawProviderDiscoversSymlinkedAgentDirectory(t, spec)
}

func TestOpenClawProviderParse(t *testing.T) {
	spec := openClawProviderTestSpec()
	assertClawProviderParse(t, spec)
}

func TestQClawProviderParse(t *testing.T) {
	spec := qClawProviderTestSpec()
	assertClawProviderParse(t, spec)
}

type clawProviderTestSpec struct {
	agent       AgentType
	prefix      string
	sessionFile func(string) bool
	fixture     func(string, string) string
}

func openClawProviderTestSpec() clawProviderTestSpec {
	return clawProviderTestSpec{
		agent:       AgentOpenClaw,
		prefix:      "openclaw",
		sessionFile: IsOpenClawSessionFile,
		fixture: func(sessionID string, firstMessage string) string {
			return clawProviderFixture(sessionID, firstMessage)
		},
	}
}

func qClawProviderTestSpec() clawProviderTestSpec {
	return clawProviderTestSpec{
		agent:       AgentQClaw,
		prefix:      "qclaw",
		sessionFile: IsQClawSessionFile,
		fixture: func(sessionID string, firstMessage string) string {
			return clawProviderFixture(sessionID, firstMessage)
		},
	}
}

func assertClawProviderReplacesLegacyAdapter(t *testing.T, agent AgentType) {
	t.Helper()

	factory, ok := ProviderFactoryByType(agent)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(agent, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func assertClawProviderSourceMethods(t *testing.T, spec clawProviderTestSpec) {
	t.Helper()

	root := t.TempDir()
	activePath := filepath.Join(root, "main", "sessions", "abc-123.jsonl")
	activeArchivePath := filepath.Join(
		root, "main", "sessions",
		"abc-123.jsonl.deleted.2026-01-01T00-00-00.000Z",
	)
	oldArchivePath := filepath.Join(
		root, "main", "sessions",
		"def-456.jsonl.deleted.2026-01-01T00-00-00.000Z",
	)
	newArchivePath := filepath.Join(
		root, "main", "sessions",
		"def-456.jsonl.reset.2026-03-01T00-00-00.000Z",
	)
	writeSourceFile(t, activePath, spec.fixture("abc-123", "active question"))
	writeSourceFile(t, activeArchivePath, spec.fixture("abc-123", "archived active"))
	writeSourceFile(t, oldArchivePath, spec.fixture("def-456", "old archive"))
	writeSourceFile(t, newArchivePath, spec.fixture("def-456", "new archive"))
	writeSourceFile(t, filepath.Join(root, "main", "sessions", "notes.jsonl.tmp"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "bad agent", "sessions", "skip.jsonl"), "{}\n")

	provider, ok := NewProvider(spec.agent, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl", "*.jsonl.*"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, activePath, discovered[0].DisplayPath)
	assert.Equal(t, "main", discovered[0].ProjectHint)
	assert.Equal(t, newArchivePath, discovered[1].DisplayPath)
	assert.Equal(t, "main", discovered[1].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~" + spec.prefix + ":main:abc-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, activePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "main:def-456",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, newArchivePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: activeArchivePath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, activePath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, activePath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: newArchivePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, newArchivePath, changed[0].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: activeArchivePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)

	require.NoError(t, os.Remove(activePath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: activePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, activeArchivePath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(newArchivePath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: newArchivePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, oldArchivePath, changed[0].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      oldArchivePath,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)

	assert.True(t, spec.sessionFile(filepath.Base(activeArchivePath)))
}

func assertClawProviderDiscoversSymlinkedAgentDirectory(
	t *testing.T,
	spec clawProviderTestSpec,
) {
	t.Helper()

	root := t.TempDir()
	targetRoot := t.TempDir()
	targetAgent := filepath.Join(targetRoot, "main")
	sourceAgent := filepath.Join(root, "main")
	sourcePath := filepath.Join(sourceAgent, "sessions", "abc-123.jsonl")
	writeSourceFile(
		t,
		filepath.Join(targetAgent, "sessions", "abc-123.jsonl"),
		spec.fixture("abc-123", "from symlink"),
	)
	if err := os.Symlink(targetAgent, sourceAgent); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(spec.agent, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~" + spec.prefix + ":main:abc-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func assertClawProviderParse(t *testing.T, spec clawProviderTestSpec) {
	t.Helper()

	root := t.TempDir()
	sourcePath := filepath.Join(root, "main", "sessions", "abc-123.jsonl")
	writeSourceFile(t, sourcePath, spec.fixture("abc-123", "provider question"))

	provider, ok := NewProvider(spec.agent, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, spec.prefix+":main:abc-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "project", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func clawProviderFixture(sessionID string, firstMessage string) string {
	return `{"type":"session","version":3,"id":"` + sessionID + `","timestamp":"2026-02-25T10:00:00Z","cwd":"/home/user/project"}` + "\n" +
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"` + firstMessage + `"}],"timestamp":"2026-02-25T10:00:01Z"}}` + "\n" +
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"timestamp":"2026-02-25T10:00:02Z"}}` + "\n"
}
