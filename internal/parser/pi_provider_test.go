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

func TestOMPProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentOMP, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~omp:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, AgentOMP, found.Provider)
	assert.Equal(t, sourcePath, found.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "omp:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, AgentOMP, outcome.Results[0].Result.Session.Agent)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, AgentOMP, changed[0].Provider)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

// TestOMPProviderDiscoversTitleSlotSession reproduces issue #959: OMP
// v16.3+ writes a fixed-width title slot line before the session header,
// so discovery must look past it instead of only sniffing the first line.
func TestOMPProviderDiscoversTitleSlotSession(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-repos-x", "2026-07-02T09-48-32-328Z_omp-slot.jsonl")
	writeSourceFile(t, sourcePath, strings.Join([]string{
		`{"type":"title","v":1,"title":"Fix the widget","source":"auto","updatedAt":"2026-07-02T09:50:00.000Z","pad":"   "}`,
		`{"type":"session","version":3,"id":"omp-slot","timestamp":"2026-07-02T09:48:32.328Z","cwd":"/repos/x"}`,
		`{"type":"message","id":"msg-1","parentId":null,"timestamp":"2026-07-02T09:48:44.939Z","message":{"role":"user","content":[{"type":"text","text":"just response ok"}]}}`,
		"",
	}, "\n"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1,
		"OMP session with leading title slot must be discovered")
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, "omp:omp-slot", sess.ID)
	assert.Equal(t, "Fix the widget", sess.SessionName)
}

func TestPiProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	lookupOnlyPath := filepath.Join(root, "encoded-cwd", "lookup-only.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))
	writeSourceFile(t, lookupOnlyPath, `{"type":"message"}`+"\n")
	writeSourceFile(t, filepath.Join(root, "encoded-cwd", "notes.txt"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "root-session.jsonl"), piProviderFixture("root-session"))
	writeSourceFile(t, filepath.Join(root, "encoded-cwd", "nested", "deep.jsonl"), piProviderFixture("deep"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentPi, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Empty(t, discovered[0].ProjectHint)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~pi:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "pi:lookup-only",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, lookupOnlyPath, found.DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestPiProviderDiscoveryAcceptsSessionHeaderInNonSessionIDFilename(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "2025.01.01.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("header-session-id"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "pi:header-session-id", outcome.Results[0].Result.Session.ID)

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "2025.01.01",
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPiProviderDiscoversSymlinkedCWDDirectory(t *testing.T) {
	root := t.TempDir()
	targetDir := t.TempDir()
	sourcePath := filepath.Join(root, "linked-cwd", "session-123.jsonl")
	targetPath := filepath.Join(targetDir, "session-123.jsonl")
	writeSourceFile(t, targetPath, piProviderFixture("session-123"))
	if err := os.Symlink(targetDir, filepath.Join(root, "linked-cwd")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~pi:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestPiProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
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
	assert.Equal(t, "pi:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "pi_project", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func piProviderFixture(sessionID string) string {
	return strings.Join([]string{
		`{"type":"session","version":3,"id":"` + sessionID + `","timestamp":"2025-01-01T10:00:00Z","cwd":"/Users/alice/code/pi-project"}`,
		`{"type":"message","id":"msg-1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":"Inspect the Pi source."}}`,
		`{"type":"message","id":"msg-2","timestamp":"2025-01-01T10:00:02Z","message":{"role":"assistant","content":"Looks ready.","model":"claude-opus-4-5","usage":{"input_tokens":10,"output_tokens":5}}}`,
	}, "\n")
}

// TestPiProviderFingerprintIncludesContentHash guards that the Pi provider
// computes a full-file content hash. The legacy per-agent parse stored a
// file_hash; without WithContentHashing the provider fingerprint hash is empty
// and a resync clears the stored file_hash to NULL. Toggle-provable: removing
// WithContentHashing from newPiSourceSet makes fp.Hash empty and fails here.
func TestPiProviderFingerprintIncludesContentHash(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
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

func ompMainFixture(id string) string {
	return strings.Join([]string{
		`{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-07-14T06:45:53.798Z","cwd":"/home/u/repos/x","title":"Main task"}`,
		`{"type":"message","id":"m1","timestamp":"2026-07-14T06:45:54Z","message":{"role":"user","content":"do it"}}`,
	}, "\n") + "\n"
}

func ompSubagentFixture(id string) string {
	return strings.Join([]string{
		`{"type":"title","v":1,"title":"","updatedAt":"2026-07-14T06:48:08.907Z","pad":"   "}`,
		`{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-07-14T06:48:08.907Z","cwd":"/home/u/repos/x"}`,
		`{"type":"message","id":"s1","timestamp":"2026-07-14T06:48:09Z","message":{"role":"user","content":"scout task"}}`,
	}, "\n") + "\n"
}

// TestOMPProviderDiscoversNestedSubagents verifies that OMP subagent
// transcripts, which live one directory deeper than the main session
// (<project>/<session>/<agent>.jsonl) and nest recursively, are discovered
// and parsed as subagent sessions whose parent is recovered from the sibling
// parent transcript. Non-.jsonl companions (.md, .bash.log) are ignored.
func TestOMPProviderDiscoversNestedSubagents(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-repos-x")
	stem := "2026-07-14T06-45-53-798Z_parent-uuid"
	mainPath := filepath.Join(proj, stem+".jsonl")
	subPath := filepath.Join(proj, stem, "Scout.jsonl")
	subSubPath := filepath.Join(proj, stem, "Scout", "DeepScout.jsonl")
	writeSourceFile(t, mainPath, ompMainFixture("parent-uuid"))
	writeSourceFile(t, subPath, ompSubagentFixture("child-uuid"))
	writeSourceFile(t, subSubPath, ompSubagentFixture("grandchild-uuid"))
	// Companions that sit beside a subagent transcript must not be discovered.
	writeSourceFile(t, filepath.Join(proj, stem, "Scout.md"), "notes")
	writeSourceFile(t, filepath.Join(proj, stem, "0.bash.log"), "log output")

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	paths := make([]string, len(discovered))
	for i, d := range discovered {
		paths[i] = d.DisplayPath
	}
	assert.ElementsMatch(t, []string{mainPath, subPath, subSubPath}, paths)

	byPath := make(map[string]ParsedSession, len(discovered))
	for _, d := range discovered {
		outcome, err := provider.Parse(context.Background(), ParseRequest{
			Source:  d,
			Machine: "devbox",
		})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		byPath[d.DisplayPath] = outcome.Results[0].Result.Session
	}

	main := byPath[mainPath]
	assert.Equal(t, "omp:parent-uuid", main.ID)
	assert.Empty(t, main.ParentSessionID, "main session has no parent")
	assert.Empty(t, string(main.RelationshipType), "main session has no relationship")

	sub := byPath[subPath]
	assert.Equal(t, "omp:child-uuid", sub.ID)
	assert.Equal(t, "omp:parent-uuid", sub.ParentSessionID)
	assert.Equal(t, RelSubagent, sub.RelationshipType)
	assert.Equal(t, "Scout", sub.SessionName, "subagent named after its transcript file")

	deep := byPath[subSubPath]
	assert.Equal(t, "omp:grandchild-uuid", deep.ID)
	assert.Equal(t, "omp:child-uuid", deep.ParentSessionID, "nested subagent parent")
	assert.Equal(t, RelSubagent, deep.RelationshipType)
	assert.Equal(t, "DeepScout", deep.SessionName)
}

func TestOMPProviderFindSourceByNestedSubagentRawID(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-repos-x")
	stem := "2026-07-14T06-45-53-798Z_parent-uuid"
	mainPath := filepath.Join(proj, stem+".jsonl")
	subPath := filepath.Join(proj, stem, "Scout.jsonl")
	writeSourceFile(t, mainPath, ompMainFixture("parent-uuid"))
	writeSourceFile(t, subPath, ompSubagentFixture("child-uuid"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "child-uuid",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, subPath, found.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: found,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "omp:child-uuid", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "omp:parent-uuid", outcome.Results[0].Result.Session.ParentSessionID)
}

// TestOMPProviderMapsSubagentChangedPath verifies a filesystem event on a
// nested subagent transcript resolves back to that subagent source so live
// updates re-parse it.
func TestOMPProviderMapsSubagentChangedPath(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-repos-x")
	stem := "2026-07-14T06-45-53-798Z_parent-uuid"
	subPath := filepath.Join(proj, stem, "Scout.jsonl")
	writeSourceFile(t, filepath.Join(proj, stem+".jsonl"), ompMainFixture("parent-uuid"))
	writeSourceFile(t, subPath, ompSubagentFixture("child-uuid"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: subPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, subPath, changed[0].DisplayPath)
}

// TestPiProviderRejectsNestedSubagents pins that the depth relaxation is
// OMP-only: upstream pi keeps the strict <project>/<session>.jsonl layout and
// never discovers a nested transcript.
func TestPiProviderRejectsNestedSubagents(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	stem := "session-123"
	writeSourceFile(t, filepath.Join(proj, stem+".jsonl"), piProviderFixture(stem))
	writeSourceFile(t, filepath.Join(proj, stem, "nested.jsonl"), piProviderFixture("nested"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1, "pi ignores nested transcripts")
	assert.Equal(t, filepath.Join(proj, stem+".jsonl"), discovered[0].DisplayPath)
}
