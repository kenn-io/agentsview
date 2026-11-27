package parser

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An unreadable saved-output folder leaves both freshness checks and the
// cutoff working, and restoring its permissions changes both checks, so the
// next sync reads the outputs.
func TestCodeBuddyCBCFreshnessTracksUnreadableSavedOutputFolders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	for _, tc := range []struct {
		name    string
		blocked string
	}{
		{"tool-results folder", filepath.Join("run-1", "tool-results")},
		{"nested folder", filepath.Join("run-1", "tool-results", "nested")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, project := cbcHome(t)
			projects := filepath.Dir(project)
			path := filepath.Join(project, "run-1.jsonl")
			writeCBC(t, path, line)
			writeCBC(t, filepath.Join(project, "run-1", "tool-results", "nested", "call-1.txt"), "saved output")
			blocked := filepath.Join(project, tc.blocked)
			require.NoError(t, os.Chmod(blocked, 0o000))
			t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

			set := newCodeBuddyCBCSourceSet([]string{projects}, "")
			source := set.sourceRef(projects, path)
			probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
			require.True(t, ok)

			before, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err, "an unreadable folder does not block the fingerprint")
			beforeStat := probe.ComputeMultiFileStatHash(path)
			assert.NotZero(t, beforeStat, "an unreadable folder keeps the stat hash")
			_, err = CodeBuddyCutoffMtime(t.Context(), path, []string{projects})
			require.NoError(t, err, "an unreadable folder does not block the cutoff")

			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = set.Fingerprint(canceled, source)
			require.ErrorIs(t, err, context.Canceled)
			_, err = CodeBuddyCutoffMtime(canceled, path, []string{projects})
			require.ErrorIs(t, err, context.Canceled)

			require.NoError(t, os.Chmod(blocked, 0o755))
			after, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, before.Hash, after.Hash, "a readable folder changes the fingerprint")
			assert.NotEqual(t, beforeStat, probe.ComputeMultiFileStatHash(path),
				"a readable folder changes the stat hash")
		})
	}
}

// A branch imported before its parent names the parent by the runtime id the
// catalog records. When the parent arrives under another filename, both
// freshness checks of the unchanged branch change, so the branch links to
// the parent's archive id.
func TestCodeBuddyCBCFreshnessFollowsForkParentArrival(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parentProject string
	}{
		{"same project", "-work-demo"},
		{"other project", "-work-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			projects := filepath.Dir(project)
			branch := filepath.Join(project, "branch-file.jsonl")
			writeCBC(t, branch,
				`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-b","content":[{"type":"input_text","text":"branch"}]}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"ok"}]}`,
			)
			writeCBCCatalog(t, home,
				`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('c-parent', 'Parent', 'run-p')`,
				`INSERT INTO conversations (id, title, runtime_session_id, fork_source_id) VALUES ('c-b', 'Branch', 'run-b', 'c-parent')`,
			)
			clearCodeBuddyCatalogCache()
			set := newCodeBuddyCBCSourceSet([]string{projects}, "")
			source := set.sourceRef(projects, branch)
			probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
			require.True(t, ok)
			before, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			beforeStat := probe.ComputeMultiFileStatHash(branch)
			require.NotZero(t, beforeStat)
			assert.Equal(t, "codebuddy:run-p",
				cbcSession(t, parseCBCHome(t, home), "codebuddy:branch-file").Session.ParentSessionID)

			writeCBC(t, filepath.Join(projects, tc.parentProject, "parent-file.jsonl"),
				`{"id":"u1","type":"message","role":"user","timestamp":900,"sessionId":"run-p","content":[{"type":"input_text","text":"first"}]}`)
			after, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, before.Hash, after.Hash, "the parent's arrival changes the branch fingerprint")
			afterStat := probe.ComputeMultiFileStatHash(branch)
			assert.NotEqual(t, beforeStat, afterStat, "the parent's arrival changes the branch stat hash")
			assert.Equal(t, afterStat, probe.ComputeMultiFileStatHash(branch), "an unchanged layout keeps the stat hash")
			assert.Equal(t, "codebuddy:parent-file",
				cbcSession(t, parseCBCHome(t, home), "codebuddy:branch-file").Session.ParentSessionID)
		})
	}
}
