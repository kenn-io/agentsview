package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOMPProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentOMP, discovered[0].Provider)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~omp:session-123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(AgentOMP, found.Provider)
	assert.Equal(sourcePath, found.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	assert.Equal("omp:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(AgentOMP, outcome.Results[0].Result.Session.Agent)
	assert.Equal("abc123", outcome.Results[0].Result.Session.File.Hash)

	require.NoError(os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(AgentOMP, changed[0].Provider)
	assert.Equal(sourcePath, changed[0].DisplayPath)
}

func TestOMPProviderFindsV1SessionByFilenameID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "v1-session.jsonl")
	writeSourceFile(t, sourcePath, strings.Join([]string{
		`{"type":"session","timestamp":"2025-01-01T10:00:00Z","cwd":"/Users/alice/code/v1-project"}`,
		`{"type":"message","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		"",
	}, "\n"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~omp:v1-session",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: found})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal("omp:v1-session", outcome.Results[0].Result.Session.ID)
}

// TestOMPProviderDiscoversTitleSlotSession reproduces issue #959: OMP
// v16.3+ writes a fixed-width title slot line before the session header,
// so discovery must look past it instead of only sniffing the first line.
func TestOMPProviderDiscoversTitleSlotSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1,
		"OMP session with leading title slot must be discovered")
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal("omp:omp-slot", sess.ID)
	assert.Equal("Fix the widget", sess.SessionName)
}

func TestPiProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	lookupOnlyPath := filepath.Join(root, "encoded-cwd", "lookup-only.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))
	writeSourceFile(t, lookupOnlyPath, `{"type":"message"}`+"\n")
	writeSourceFile(t, filepath.Join(root, "encoded-cwd", "notes.txt"), "{}\n")
	rootPath := filepath.Join(root, "root-session.jsonl")
	writeSourceFile(t, rootPath, piProviderFixture("root-session"))
	writeSourceFile(t, filepath.Join(root, "encoded-cwd", "nested", "deep.jsonl"), piProviderFixture("deep"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	assert.ElementsMatch([]string{sourcePath, rootPath},
		[]string{discovered[0].DisplayPath, discovered[1].DisplayPath})

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)
	assert.Equal([]string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~pi:session-123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "pi:lookup-only",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(lookupOnlyPath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "pi:root-session",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(rootPath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: rootPath, EventKind: "write", WatchRoot: root,
	})
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(rootPath, changed[0].DisplayPath)

	require.NoError(os.Remove(sourcePath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)
}

func TestPiProviderDiscoveryAcceptsSessionHeaderInNonSessionIDFilename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "2025.01.01.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("header-session-id"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal("pi:header-session-id", outcome.Results[0].Result.Session.ID)

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "2025.01.01",
	})
	require.NoError(err)
	assert.False(ok)
}

func TestPiProviderDiscoversSymlinkedCWDDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~pi:session-123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)
}

func TestPiProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
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
	assert.Equal("pi:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal("pi_project", outcome.Results[0].Result.Session.Project)
	assert.Equal("devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal("abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(outcome.Results[0].Result.Messages, 2)
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
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
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

// TestPiProviderDiscoversAndParsesNativeParentSession verifies that native Pi
// parentSession resolves to the parent's header identity during discovery and
// parsing, even when the normal timestamp_UUID filename does not contain it.
func TestPiProviderDiscoversAndParsesNativeParentSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	parentPath := filepath.Join(proj, "2026-07-14T06-45-53-798Z_parent-uuid.jsonl")
	childPath := filepath.Join(proj, "2026-07-14T06-48-08-907Z_child-uuid.jsonl")
	writeSourceFile(t, parentPath, strings.Join([]string{
		`{"type":"session","version":3,"id":"actual-parent-header","timestamp":"2026-07-14T06:45:53.798Z","cwd":"/home/u/repos/x"}`,
		`{"type":"message","id":"p1","timestamp":"2026-07-14T06:45:54Z","message":{"role":"user","content":"root"}}`,
		"",
	}, "\n"))
	parentPathJSON, err := json.Marshal(parentPath)
	require.NoError(err)
	writeSourceFile(t, childPath, strings.Join([]string{
		`{"type":"session","version":3,"id":"child-uuid","timestamp":"2026-07-14T06:48:08.907Z","cwd":"/home/u/repos/x","parentSession":` + string(parentPathJSON) + `}`,
		`{"type":"message","id":"c1","timestamp":"2026-07-14T06:48:09Z","message":{"role":"user","content":"child"}}`,
		"",
	}, "\n"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)

	byPath := make(map[string]ParsedSession, len(discovered))
	for _, source := range discovered {
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(err)
		require.Len(outcome.Results, 1)
		byPath[source.DisplayPath] = outcome.Results[0].Result.Session
	}

	parent := byPath[parentPath]
	child := byPath[childPath]
	assert.Equal("pi:actual-parent-header", parent.ID)
	assert.Equal(parent.ID, child.ParentSessionID)

	// No-hint FindSource (no stored path or fingerprint) must fall back to
	// scanning session headers: native filenames are timestamp-prefixed and do
	// not contain the header UUID.
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "actual-parent-header",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(parentPath, found.DisplayPath)
	assert.NotEqual(childPath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "child-uuid",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(childPath, found.DisplayPath)
	assert.NotEqual(parentPath, found.DisplayPath)

	// A stored path hint is still honored ahead of header scanning.
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: childPath,
		RawSessionID:   "actual-parent-header",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(childPath, found.DisplayPath,
		"stored path hints are preserved ahead of header lookup")

	// An unknown header UUID yields not-found rather than a wrong source.
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "missing-header-id",
	})
	require.NoError(err)
	assert.False(ok)
	assert.Empty(found.DisplayPath)
}

// TestOMPProviderDiscoversNestedSubagents verifies that OMP subagent
// transcripts, which live one directory deeper than the main session
// (<project>/<session>/<agent>.jsonl) and nest recursively, are discovered
// and parsed as subagent sessions whose parent is recovered from the sibling
// parent transcript. Non-.jsonl companions (.md, .bash.log) are ignored.
func TestOMPProviderDiscoversNestedSubagents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	paths := make([]string, len(discovered))
	for i, d := range discovered {
		paths[i] = d.DisplayPath
	}
	assert.ElementsMatch([]string{mainPath, subPath, subSubPath}, paths)

	byPath := make(map[string]ParsedSession, len(discovered))
	for _, d := range discovered {
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source:  d,
			Machine: "devbox",
		})
		require.NoError(err)
		require.Len(outcome.Results, 1)
		byPath[d.DisplayPath] = outcome.Results[0].Result.Session
	}

	main := byPath[mainPath]
	assert.Equal("omp:parent-uuid", main.ID)
	assert.Empty(main.ParentSessionID, "main session has no parent")
	assert.Empty(string(main.RelationshipType), "main session has no relationship")

	sub := byPath[subPath]
	assert.Equal("omp:child-uuid", sub.ID)
	assert.Equal("omp:parent-uuid", sub.ParentSessionID)
	assert.Equal(RelSubagent, sub.RelationshipType)
	assert.Equal("Scout", sub.SessionName, "subagent named after its transcript file")

	deep := byPath[subSubPath]
	assert.Equal("omp:grandchild-uuid", deep.ID)
	assert.Equal("omp:child-uuid", deep.ParentSessionID, "nested subagent parent")
	assert.Equal(RelSubagent, deep.RelationshipType)
	assert.Equal("DeepScout", deep.SessionName)
}

func TestOMPProviderFindSourceByNestedSubagentRawID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "child-uuid",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(subPath, found.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: found,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal("omp:child-uuid", outcome.Results[0].Result.Session.ID)
	assert.Equal("omp:parent-uuid", outcome.Results[0].Result.Session.ParentSessionID)
}

// TestOMPProviderMapsSubagentChangedPath verifies a filesystem event on a
// nested subagent transcript resolves back to that subagent source so live
// updates re-parse it.
func TestOMPProviderMapsSubagentChangedPath(t *testing.T) {
	require := require.New(t)

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
	require.True(ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: subPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(t, subPath, changed[0].DisplayPath)
}

// TestPiProviderRejectsNestedSubagents pins that the depth relaxation is
// OMP-only: upstream pi keeps the strict <project>/<session>.jsonl layout and
// never discovers a nested transcript.
func TestPiProviderRejectsNestedSubagents(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	stem := "session-123"
	writeSourceFile(t, filepath.Join(proj, stem+".jsonl"), piProviderFixture(stem))
	writeSourceFile(t, filepath.Join(proj, stem, "nested.jsonl"), piProviderFixture("nested"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1, "pi ignores nested transcripts")
	assert.Equal(t, filepath.Join(proj, stem+".jsonl"), discovered[0].DisplayPath)
}
