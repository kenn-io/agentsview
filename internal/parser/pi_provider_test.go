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
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentOMP, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~omp:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, AgentOMP, found.Provider)
	assert.Equal(t, sourcePath, found.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, AgentOMP, changed[0].Provider)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestOMPProviderFindsV1SessionByFilenameID(t *testing.T) {
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
	require.True(t, ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~omp:v1-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: found})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "omp:v1-session", outcome.Results[0].Result.Session.ID)
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

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1,
		"OMP session with leading title slot must be discovered")
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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
	rootPath := filepath.Join(root, "root-session.jsonl")
	writeSourceFile(t, rootPath, piProviderFixture("root-session"))
	deepPath := filepath.Join(root, "encoded-cwd", "nested", "deep.jsonl")
	writeSourceFile(t, deepPath, piProviderFixture("deep"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 3)
	assert.ElementsMatch(t, []string{sourcePath, rootPath, deepPath},
		[]string{discovered[0].DisplayPath, discovered[1].DisplayPath, discovered[2].DisplayPath})

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~pi:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "pi:lookup-only",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, lookupOnlyPath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "pi:root-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rootPath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: rootPath, EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, rootPath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
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

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "pi:header-session-id", outcome.Results[0].Result.Session.ID)

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
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

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
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
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fp, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	require.NotEmpty(t, fp.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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

// TestPiProviderDiscoversAndParsesNativeParentSession verifies that native Pi
// parentSession resolves to the parent's header identity during discovery and
// parsing, even when the normal timestamp_UUID filename does not contain it.
func TestPiProviderDiscoversAndParsesNativeParentSession(t *testing.T) {
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
	require.NoError(t, err)
	writeSourceFile(t, childPath, strings.Join([]string{
		`{"type":"session","version":3,"id":"child-uuid","timestamp":"2026-07-14T06:48:08.907Z","cwd":"/home/u/repos/x","parentSession":` + string(parentPathJSON) + `}`,
		`{"type":"message","id":"c1","timestamp":"2026-07-14T06:48:09Z","message":{"role":"user","content":"child"}}`,
		"",
	}, "\n"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)

	byPath := make(map[string]ParsedSession, len(discovered))
	for _, source := range discovered {
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		byPath[source.DisplayPath] = outcome.Results[0].Result.Session
	}

	parent := byPath[parentPath]
	child := byPath[childPath]
	assert.Equal(t, "pi:actual-parent-header", parent.ID)
	assert.Equal(t, parent.ID, child.ParentSessionID)

	// No-hint FindSource (no stored path or fingerprint) must fall back to
	// scanning session headers: native filenames are timestamp-prefixed and do
	// not contain the header UUID.
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "actual-parent-header",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, parentPath, found.DisplayPath)
	assert.NotEqual(t, childPath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "child-uuid",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, childPath, found.DisplayPath)
	assert.NotEqual(t, parentPath, found.DisplayPath)

	// A stored path hint is still honored ahead of header scanning.
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: childPath,
		RawSessionID:   "actual-parent-header",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, childPath, found.DisplayPath,
		"stored path hints are preserved ahead of header lookup")

	// An unknown header UUID yields not-found rather than a wrong source.
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "missing-header-id",
	})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, found.DisplayPath)
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

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	paths := make([]string, len(discovered))
	for i, d := range discovered {
		paths[i] = d.DisplayPath
	}
	assert.ElementsMatch(t, []string{mainPath, subPath, subSubPath}, paths)

	byPath := make(map[string]ParsedSession, len(discovered))
	for _, d := range discovered {
		outcome, err := provider.Parse(t.Context(), ParseRequest{
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

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "child-uuid",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, subPath, found.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
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
		t.Context(),
		ChangedPathRequest{Path: subPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, subPath, changed[0].DisplayPath)
}

// TestPiProviderDiscoversSubagentSessionsInSubdirectory reproduces issue #1894:
// the pi-subagents extension keeps a child session under the parent
// transcript's directory name (<project>/<parent>/<runId>/run-N/session.jsonl)
// instead of beside interactive chats, and Pi discovery rejected anything
// deeper than one project directory. Toggle-provable: restoring the
// root-or-one-level IncludePath drops the child and fails here.
func TestPiProviderDiscoversSubagentSessionsInSubdirectory(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	parentStem := "2026-09-21T10-00-00-000Z_parent-uuid"
	parentPath := filepath.Join(proj, parentStem+".jsonl")
	childPath := filepath.Join(proj, parentStem, "run-abc", "run-0", "session.jsonl")
	writeSourceFile(t, parentPath, piProviderFixture("parent-uuid"))
	writeSourceFile(t, childPath, piProviderFixture("child-uuid"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	paths := make([]string, len(discovered))
	for i, d := range discovered {
		paths[i] = d.DisplayPath
	}
	assert.ElementsMatch(t, []string{parentPath, childPath}, paths)

	byPath := make(map[string]ParsedSession, len(discovered))
	for _, d := range discovered {
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: d})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		byPath[d.DisplayPath] = outcome.Results[0].Result.Session
	}

	assert.Equal(t, "pi:parent-uuid", byPath[parentPath].ID)
	assert.Equal(t, AgentPi, byPath[parentPath].Agent)
	assert.Empty(t, byPath[parentPath].ParentSessionID)

	// Fresh pi-subagents children are all named session.jsonl, so the header id
	// has to win over the filename-derived id.
	assert.Equal(t, "pi:child-uuid", byPath[childPath].ID)
	assert.Equal(t, AgentPi, byPath[childPath].Agent)
	assert.Equal(t, "pi:parent-uuid", byPath[childPath].ParentSessionID)
	assert.Equal(t, RelSubagent, byPath[childPath].RelationshipType)
}

// TestPiProviderLinksForkedSubagentToParent checks that a forked child keeps
// the parent link its header records when discovery reaches it under the
// parent transcript's directory instead of beside it.
func TestPiProviderLinksForkedSubagentToParent(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	parentStem := "2026-09-21T10-00-00-000Z_parent-uuid"
	parentPath := filepath.Join(proj, parentStem+".jsonl")
	writeSourceFile(t, parentPath, piProviderFixture("parent-uuid"))
	parentPathJSON, err := json.Marshal(parentPath)
	require.NoError(t, err)
	forkPath := filepath.Join(
		proj, parentStem, "forks", "2026-09-21T10-05-00-000Z_fork-uuid.jsonl",
	)
	writeSourceFile(t, forkPath, strings.Join([]string{
		`{"type":"session","version":3,"id":"fork-uuid","timestamp":"2026-09-21T10:05:00Z","cwd":"/Users/alice/code/pi-project","parentSession":` + string(parentPathJSON) + `}`,
		`{"type":"message","id":"f1","timestamp":"2026-09-21T10:05:01Z","message":{"role":"user","content":"continue"}}`,
		"",
	}, "\n"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)

	byPath := make(map[string]SourceRef, len(discovered))
	for _, d := range discovered {
		byPath[d.DisplayPath] = d
	}
	require.Contains(t, byPath, forkPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: byPath[forkPath]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	fork := outcome.Results[0].Result.Session
	assert.Equal(t, "pi:fork-uuid", fork.ID)
	assert.Equal(t, "pi:parent-uuid", fork.ParentSessionID)
	assert.Equal(t, RelFork, fork.RelationshipType)
}

// TestPiProviderDiscoversNestedSubagentsWithoutDuplicates checks that a child
// that delegates to its own child is found at every depth exactly once. The
// nested path reuses the child's session.jsonl stem because that is what
// pi-subagents derives the next session root from.
func TestPiProviderDiscoversNestedSubagentsWithoutDuplicates(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	parentStem := "2026-09-21T10-00-00-000Z_parent-uuid"
	parentPath := filepath.Join(proj, parentStem+".jsonl")
	childPath := filepath.Join(proj, parentStem, "run-abc", "run-0", "session.jsonl")
	deepPath := filepath.Join(
		proj, parentStem, "run-abc", "run-0", "session",
		"run-def", "run-0", "session.jsonl",
	)
	writeSourceFile(t, parentPath, piProviderFixture("parent-uuid"))
	writeSourceFile(t, childPath, piProviderFixture("child-uuid"))
	writeSourceFile(t, deepPath, piProviderFixture("grandchild-uuid"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	byPath := make(map[string]SourceRef, len(discovered))
	for _, d := range discovered {
		require.NotContains(t, byPath, d.DisplayPath, "source discovered once")
		byPath[d.DisplayPath] = d
	}
	require.Len(t, byPath, 3)
	for _, path := range []string{parentPath, childPath, deepPath} {
		require.Contains(t, byPath, path)
	}

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: byPath[deepPath]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	grandchild := outcome.Results[0].Result.Session
	assert.Equal(t, "pi:grandchild-uuid", grandchild.ID)
	assert.Equal(t, "pi:child-uuid", grandchild.ParentSessionID)
	assert.Equal(t, RelSubagent, grandchild.RelationshipType)
}

// TestPiProviderLeavesUnmatchedSubagentShapesUnlinked checks that a
// session.jsonl is linked only when it sits in a run-N directory with a Pi
// transcript at <parent>.jsonl. An explicit pi-subagents sessionDir has no
// parent transcript above the run, so it must stay a standalone session.
func TestPiProviderLeavesUnmatchedSubagentShapesUnlinked(t *testing.T) {
	tests := []struct {
		name  string
		child string
		files map[string]string
	}{
		{
			name:  "explicit session dir without parent transcript",
			child: filepath.Join("run-abc", "run-0", "session.jsonl"),
		},
		{
			name:  "parent transcript is not a Pi session",
			child: filepath.Join("encoded-cwd", "notes", "run-abc", "run-0", "session.jsonl"),
			files: map[string]string{
				filepath.Join("encoded-cwd", "notes.jsonl"): `{"type":"message"}` + "\n",
			},
		},
		{
			name:  "attempt directory is not run-N",
			child: filepath.Join("encoded-cwd", "parent", "run-abc", "attempt", "session.jsonl"),
			files: map[string]string{
				filepath.Join("encoded-cwd", "parent.jsonl"): piProviderFixture("parent-uuid"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, content := range tt.files {
				writeSourceFile(t, filepath.Join(root, rel), content)
			}
			childPath := filepath.Join(root, tt.child)
			writeSourceFile(t, childPath, piProviderFixture("child-uuid"))

			provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				StoredFilePath: childPath,
			})
			require.NoError(t, err)
			require.True(t, ok)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			child := outcome.Results[0].Result.Session
			assert.Equal(t, "pi:child-uuid", child.ID)
			assert.Empty(t, child.ParentSessionID)
			assert.Empty(t, child.RelationshipType)
		})
	}
}

// TestPiProviderIgnoresTranscriptlessSubagentDirectory checks that a subagent
// run directory holding only companion files does not fail discovery or add a
// source.
func TestPiProviderIgnoresTranscriptlessSubagentDirectory(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	parentStem := "2026-09-21T10-00-00-000Z_parent-uuid"
	parentPath := filepath.Join(proj, parentStem+".jsonl")
	writeSourceFile(t, parentPath, piProviderFixture("parent-uuid"))
	runDir := filepath.Join(proj, parentStem, "run-abc", "run-0")
	writeSourceFile(t, filepath.Join(runDir, "output.md"), "no transcript here")

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, parentPath, discovered[0].DisplayPath)
}

// TestPiProviderDiscoversSubagentsUnderOverlappingRoots checks that adding a
// project directory as a second Pi root does not index the same nested
// transcript twice.
func TestPiProviderDiscoversSubagentsUnderOverlappingRoots(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "encoded-cwd")
	parentStem := "2026-09-21T10-00-00-000Z_parent-uuid"
	childPath := filepath.Join(proj, parentStem, "run-abc", "run-0", "session.jsonl")
	writeSourceFile(t, filepath.Join(proj, parentStem+".jsonl"), piProviderFixture("parent-uuid"))
	writeSourceFile(t, childPath, piProviderFixture("child-uuid"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root, proj}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)

	seen := make(map[string]int, len(discovered))
	for _, d := range discovered {
		seen[d.DisplayPath]++
	}
	assert.Equal(t, 1, seen[childPath], "overlapping roots index one child source")
}
