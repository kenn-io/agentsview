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

// --- OMP -------------------------------------------------------------------

// TestOMPProviderParsesWithOMPIdentity verifies the OhMyPi agent is served by
// the parameterized Pi provider: it discovers the same JSONL layout but stamps
// the omp agent type and omp: session ID prefix.
func TestOMPProviderParsesWithOMPIdentity(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-omp.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-omp"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentOMP, discovered[0].Provider)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, "omp:session-omp", sess.ID)
	assert.Equal(t, AgentOMP, sess.Agent)
}

// --- Reasonix --------------------------------------------------------------

func writeReasonixSession(t *testing.T, dir, sessionID string) string {
	t.Helper()
	transcript := filepath.Join(dir, sessionID+".jsonl")
	writeSourceFile(t, transcript, strings.Join([]string{
		`{"role":"user","content":"explain the bug"}`,
		`{"role":"assistant","content":"here is the fix","reasoning_content":"think"}`,
	}, "\n"))
	meta := transcript + ".meta"
	writeSourceFile(t, meta, `{"id":"`+sessionID+
		`","model":"claude","topic_title":"Bug fix","workspace_root":"/home/u/proj",`+
		`"created_at":"2026-02-01T10:00:00Z","updated_at":"2026-02-01T10:05:00Z"}`)
	return transcript
}

func TestReasonixProviderDiscoverAndParse(t *testing.T) {
	root := t.TempDir()
	transcript := writeReasonixSession(
		t, filepath.Join(root, "projects", "proj", "sessions"), "session-123",
	)

	provider, ok := NewProvider(AgentReasonix, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentReasonix, discovered[0].Provider)
	assert.Equal(t, transcript, discovered[0].DisplayPath)
	assert.Equal(t, "proj", discovered[0].ProjectHint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: SourceFingerprint{Hash: "deadbeef"},
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, "reasonix:session-123", sess.ID)
	assert.Equal(t, AgentReasonix, sess.Agent)
	assert.Equal(t, "Bug fix", sess.SessionName)
	assert.Equal(t, "deadbeef", sess.File.Hash)
}

// TestReasonixProviderFingerprintFoldsSidecar verifies the composite
// fingerprint sums the transcript and its .jsonl.meta sidecar sizes and takes
// the later mtime, mirroring the legacy reasonixEffectiveInfo.
func TestReasonixProviderFingerprintFoldsSidecar(t *testing.T) {
	root := t.TempDir()
	transcript := writeReasonixSession(
		t, filepath.Join(root, "sessions"), "session-fp",
	)

	provider, ok := NewProvider(AgentReasonix, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	transcriptInfo, err := os.Stat(transcript)
	require.NoError(t, err)
	metaInfo, err := os.Stat(transcript + ".meta")
	require.NoError(t, err)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	fp, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	assert.Equal(t, transcriptInfo.Size()+metaInfo.Size(), fp.Size,
		"composite size must include the sidecar")
	assert.NotEmpty(t, fp.Hash)
}

func TestReasonixProviderFingerprintHashChangesForSidecarOnlyChange(t *testing.T) {
	root := t.TempDir()
	transcript := writeReasonixSession(
		t, filepath.Join(root, "sessions"), "session-fp-hash",
	)

	provider, ok := NewProvider(AgentReasonix, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	before, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)

	writeSourceFile(t, transcript+".meta", `{"id":"session-fp-hash","model":"gpt-4.1",`+
		`"topic_title":"Updated","workspace_root":"/home/u/other",`+
		`"created_at":"2026-02-01T10:00:00Z","updated_at":"2026-02-01T10:10:00Z"}`)

	after, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash,
		"metadata-only changes must affect the composite fingerprint hash")
}

// TestReasonixProviderChangedPathSidecar verifies a .jsonl.meta sidecar event
// classifies against its sibling transcript.
func TestReasonixProviderChangedPathSidecar(t *testing.T) {
	root := t.TempDir()
	transcript := writeReasonixSession(
		t, filepath.Join(root, "projects", "proj", "sessions"), "session-cp",
	)

	provider, ok := NewProvider(AgentReasonix, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	sources, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:      transcript + ".meta",
		EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, transcript, sources[0].DisplayPath)
	assert.Equal(t, "proj", sources[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "session-cp",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, transcript, found.DisplayPath)
}

func TestReasonixProviderChangedPathLayouts(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name        string
		dir         string
		sessionID   string
		wantProject string
	}{
		{
			name:        "project bare",
			dir:         filepath.Join(root, "projects", "proj", "sessions"),
			sessionID:   "project-bare",
			wantProject: "proj",
		},
		{
			name:        "project nested",
			dir:         filepath.Join(root, "projects", "proj", "sessions", "project-nested"),
			sessionID:   "project-nested",
			wantProject: "proj",
		},
		{
			name:      "global",
			dir:       filepath.Join(root, "sessions"),
			sessionID: "global-session",
		},
		{
			name:      "archive",
			dir:       filepath.Join(root, "archive"),
			sessionID: "archive-session",
		},
		{
			name:      "subagent",
			dir:       filepath.Join(root, "sessions", "subagents"),
			sessionID: "subagent-session",
		},
	}

	provider, ok := NewProvider(AgentReasonix, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transcript := writeReasonixSession(t, tt.dir, tt.sessionID)
			sources, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{Path: transcript, EventKind: "write"},
			)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, transcript, sources[0].DisplayPath)
			assert.Equal(t, tt.wantProject, sources[0].ProjectHint)
		})
	}
}

func TestReasonixProviderChangedPathDeletedSidecarAndTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := writeReasonixSession(
		t, filepath.Join(root, "projects", "proj", "sessions"), "session-delete",
	)
	meta := transcript + ".meta"
	provider, ok := NewProvider(AgentReasonix, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	require.NoError(t, os.Remove(meta))
	sources, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:      meta,
		EventKind: "remove",
	})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, transcript, sources[0].DisplayPath,
		"deleted sidecar events must reparse the live transcript")

	require.NoError(t, os.Remove(transcript))
	sources, err = provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:      transcript,
		EventKind: "remove",
	})
	require.NoError(t, err)
	require.Len(t, sources, 1,
		"deleted transcripts remain candidates for the engine's remove filter")
	assert.Equal(t, transcript, sources[0].DisplayPath)
}

// --- Aider -----------------------------------------------------------------

func writeAiderProviderHistory(t *testing.T, repo string) string {
	t.Helper()
	path := filepath.Join(repo, AiderHistoryFileName())
	content := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n" +
		"# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"
	writeSourceFile(t, path, content)
	return path
}

func TestAiderProviderDiscoverAndFanOut(t *testing.T) {
	root := t.TempDir()
	historyPath := writeAiderProviderHistory(t, filepath.Join(root, "myrepo"))

	provider, ok := NewProvider(AgentAider, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentAider, discovered[0].Provider)
	assert.Equal(t, historyPath, discovered[0].DisplayPath)

	fp, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: fp,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ForceReplace, "aider fan-out force-replaces")
	require.Len(t, outcome.Results, 2, "two content runs produce two sessions")
	for i, r := range outcome.Results {
		hp, idx, ok := ParseAiderVirtualPath(r.Result.Session.File.Path)
		require.True(t, ok)
		assert.Equal(t, historyPath, hp)
		assert.Equal(t, i, idx)
		assert.True(t, strings.HasPrefix(r.Result.Session.ID, "aider:"))
	}
}

// TestAiderProviderFindSourceByRawID resolves a per-run session ID back to its
// virtual run source, then parses just that run.
func TestAiderProviderFindSourceByRawID(t *testing.T) {
	root := t.TempDir()
	writeAiderProviderHistory(t, filepath.Join(root, "myrepo"))

	provider, ok := NewProvider(AgentAider, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: discovered[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 2)

	rawID := strings.TrimPrefix(outcome.Results[1].Result.Session.ID, "aider:")
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: rawID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	_, idx, ok := ParseAiderVirtualPath(found.DisplayPath)
	require.True(t, ok)
	assert.Equal(t, 1, idx, "the second run resolves to run index 1")

	single, err := provider.Parse(context.Background(), ParseRequest{Source: found})
	require.NoError(t, err)
	require.Len(t, single.Results, 1)
	assert.Equal(t, outcome.Results[1].Result.Session.ID,
		single.Results[0].Result.Session.ID)
}

// TestAiderProviderRemoteIdentityStable verifies the PathRewriter-seeded
// identity keeps per-run session IDs stable when the same history file is read
// from different (temp) locations, mirroring SSH remote sync.
func TestAiderProviderRemoteIdentityStable(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeAiderProviderHistory(t, filepath.Join(rootA, "myrepo"))
	writeAiderProviderHistory(t, filepath.Join(rootB, "myrepo"))

	canonical := "host:/home/wes/myrepo/" + AiderHistoryFileName()
	rewriter := func(string) string { return canonical }

	idsFor := func(root string) []string {
		provider, ok := NewProvider(AgentAider, ProviderConfig{
			Roots:        []string{root},
			PathRewriter: rewriter,
		})
		require.True(t, ok)
		discovered, err := provider.Discover(context.Background())
		require.NoError(t, err)
		require.Len(t, discovered, 1)
		outcome, err := provider.Parse(context.Background(), ParseRequest{Source: discovered[0]})
		require.NoError(t, err)
		ids := make([]string, 0, len(outcome.Results))
		for _, r := range outcome.Results {
			ids = append(ids, r.Result.Session.ID)
		}
		return ids
	}

	assert.Equal(t, idsFor(rootA), idsFor(rootB),
		"the canonical identity must keep run IDs stable across extraction dirs")
}
