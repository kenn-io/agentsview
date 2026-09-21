package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	grokSubagentParentID   = "019f6000-0000-7000-8000-000000000010"
	grokSubagentChildID    = "019f6000-0000-7000-8000-000000000011"
	grokSubagentResumeID   = "019f6000-0000-7000-8000-000000000012"
	grokSubagentForkID     = "019f6000-0000-7000-8000-000000000013"
	grokSubagentOrphanID   = "019f6000-0000-7000-8000-000000000014"
	grokSubagentWorktreeID = "019f6000-0000-7000-8000-000000000015"
)

func parseGrokSubagentFixture(t *testing.T) map[string]ParseResult {
	t.Helper()
	root := t.TempDir()
	fixtureRoot := filepath.Join("testdata", "grok-build", "subagents")
	require.NoError(t, os.CopyFS(root, os.DirFS(fixtureRoot)))
	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 6)
	results := make(map[string]ParseResult, len(sources))
	for _, source := range sources {
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source: source,
		})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		result := outcome.Results[0].Result
		results[result.Session.SourceSessionID] = result
	}
	return results
}

func TestGrokSubagentFixtureParentsSiblingChild(t *testing.T) {
	results := parseGrokSubagentFixture(t)
	child, ok := results[grokSubagentChildID]
	require.True(t, ok)
	assert.Equal(t, RelSubagent, child.Session.RelationshipType)
	assert.Equal(t, "grok:"+grokSubagentParentID, child.Session.ParentSessionID)
	assert.Equal(t, RelNone, results[grokSubagentParentID].Session.RelationshipType)
	assert.Empty(t, results[grokSubagentParentID].Session.ParentSessionID)
}

func TestGrokSubagentResumeFromKeepsSpawningParent(t *testing.T) {
	results := parseGrokSubagentFixture(t)
	resume, ok := results[grokSubagentResumeID]
	require.True(t, ok)
	assert.Equal(t, RelSubagent, resume.Session.RelationshipType)
	assert.Equal(t, "grok:"+grokSubagentParentID, resume.Session.ParentSessionID)
	assert.NotEqual(t, "grok:"+grokSubagentChildID, resume.Session.ParentSessionID)
}

func TestGrokSubagentForkWithoutMetaStaysFork(t *testing.T) {
	results := parseGrokSubagentFixture(t)
	fork, ok := results[grokSubagentForkID]
	require.True(t, ok)
	assert.Equal(t, RelFork, fork.Session.RelationshipType)
	assert.Equal(t, "grok:"+grokSubagentParentID, fork.Session.ParentSessionID)
}

func TestGrokSubagentMissingMetadataStaysTopLevel(t *testing.T) {
	results := parseGrokSubagentFixture(t)
	orphan, ok := results[grokSubagentOrphanID]
	require.True(t, ok)
	assert.Equal(t, RelNone, orphan.Session.RelationshipType)
	assert.Empty(t, orphan.Session.ParentSessionID)
}

func TestGrokSubagentWorktreeChildFindsParentInOtherCWD(t *testing.T) {
	results := parseGrokSubagentFixture(t)
	child, ok := results[grokSubagentWorktreeID]
	require.True(t, ok)
	assert.Equal(t, RelSubagent, child.Session.RelationshipType)
	assert.Equal(t, "grok:"+grokSubagentParentID, child.Session.ParentSessionID)
	assert.Equal(t, "/workspace/worktree-child", child.Session.Cwd)
}

func TestGrokSpawnSubagentAttachesChildSessionID(t *testing.T) {
	results := parseGrokSubagentFixture(t)
	parent, ok := results[grokSubagentParentID]
	require.True(t, ok)
	var spawn *ParsedToolCall
	for i := range parent.Messages {
		for j := range parent.Messages[i].ToolCalls {
			if parent.Messages[i].ToolCalls[j].ToolName == "spawn_subagent" {
				spawn = &parent.Messages[i].ToolCalls[j]
			}
		}
	}
	require.NotNil(t, spawn)
	assert.Equal(t, "Task", spawn.Category)
	assert.Equal(t, "grok:"+grokSubagentChildID, spawn.SubagentSessionID)
}

func TestGrokSubagentParentsWhenParentSessionIsNotParsed(t *testing.T) {
	root := t.TempDir()
	fixtureRoot := filepath.Join("testdata", "grok-build", "subagents")
	require.NoError(t, os.CopyFS(root, os.DirFS(fixtureRoot)))
	parsed, err := ParseGrokSummary(
		grokSummaryPath(root, "%2Fworkspace%2Fproject", grokSubagentChildID),
		"project",
		"test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, RelSubagent, parsed.Session.RelationshipType)
	assert.Equal(t, "grok:"+grokSubagentParentID, parsed.Session.ParentSessionID)
}

func TestGrokMalformedSubagentMetaDoesNotParent(t *testing.T) {
	root := t.TempDir()
	parentID := "parent-session"
	childID := "child-session"
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", parentID), `{
		"info":{"id":"parent-session","cwd":"/workspace/project"},
		"session_summary":"parent"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(root, "cwd-key", parentID, "subagents", childID, "meta.json"),
		`{not-json`,
	)
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", childID), `{
		"info":{"id":"child-session","cwd":"/workspace/project"},
		"session_kind":"subagent",
		"session_summary":"child"
	}`)

	result, err := ParseGrokSummary(
		grokSummaryPath(root, "cwd-key", childID), "project", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, RelNone, result.Session.RelationshipType)
	assert.Empty(t, result.Session.ParentSessionID)
}

func TestGrokSubagentMetaWithoutParentIDDoesNotParent(t *testing.T) {
	root := t.TempDir()
	parentID := "parent-session"
	childID := "child-session"
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", parentID), `{
		"info":{"id":"parent-session","cwd":"/workspace/project"},
		"session_summary":"parent"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(root, "cwd-key", parentID, "subagents", childID, "meta.json"),
		`{"subagent_id":"child-session","child_session_id":"child-session","status":"running"}`,
	)
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", childID), `{
		"info":{"id":"child-session","cwd":"/workspace/project"},
		"session_kind":"subagent",
		"session_summary":"child"
	}`)

	result, err := ParseGrokSummary(
		grokSummaryPath(root, "cwd-key", childID), "project", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, RelNone, result.Session.RelationshipType)
	assert.Empty(t, result.Session.ParentSessionID)
}

func TestGrokChangedPathReparentsFromSubagentMeta(t *testing.T) {
	root := t.TempDir()
	parentID := "parent-session"
	childID := "child-session"
	childSummary := grokSummaryPath(root, "cwd-key", childID)
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", parentID), `{
		"info":{"id":"parent-session","cwd":"/workspace/project"},
		"session_summary":"parent"
	}`)
	writeGrokFixtureFile(t, childSummary, `{
		"info":{"id":"child-session","cwd":"/workspace/project"},
		"session_kind":"subagent",
		"session_summary":"child"
	}`)
	provider := newGrokTestProvider(t, root)

	before, err := ParseGrokSummary(childSummary, "project", "test-machine")
	require.NoError(t, err)
	assert.Equal(t, RelNone, before.Session.RelationshipType)

	metaPath := filepath.Join(
		root, "cwd-key", parentID, "subagents", childID, "meta.json",
	)
	writeGrokFixtureFile(t, metaPath, `{
		"subagent_id":"child-session",
		"parent_session_id":"parent-session",
		"child_session_id":"child-session",
		"status":"running"
	}`)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: metaPath,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, filepath.Clean(childSummary), filepath.Clean(changed[0].FingerprintKey))

	after, err := ParseGrokSummary(childSummary, "project", "test-machine")
	require.NoError(t, err)
	assert.Equal(t, RelSubagent, after.Session.RelationshipType)
	assert.Equal(t, "grok:"+parentID, after.Session.ParentSessionID)
}

func TestGrokFingerprintIncludesParentSubagentMeta(t *testing.T) {
	root := t.TempDir()
	parentID := "parent-session"
	childID := "child-session"
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", parentID), `{
		"info":{"id":"parent-session","cwd":"/workspace/project"},
		"session_summary":"parent"
	}`)
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", childID), `{
		"info":{"id":"child-session","cwd":"/workspace/project"},
		"session_kind":"subagent",
		"session_summary":"child"
	}`)
	provider := newGrokTestProvider(t, root)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: childID,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	writeGrokFixtureFile(
		t,
		filepath.Join(root, "cwd-key", parentID, "subagents", childID, "meta.json"),
		`{"subagent_id":"child-session","parent_session_id":"parent-session","child_session_id":"child-session","status":"running"}`,
	)
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestGrokChangedPathFindsWorktreeChildFromParentMeta(t *testing.T) {
	root := t.TempDir()
	parentID := "parent-session"
	childID := "child-session"
	parentCWD := "project-cwd"
	childCWD := "worktree-cwd"
	childSummary := grokSummaryPath(root, childCWD, childID)
	writeGrokFixtureFile(t, grokSummaryPath(root, parentCWD, parentID), `{
		"info":{"id":"parent-session","cwd":"/workspace/project"},
		"session_summary":"parent"
	}`)
	writeGrokFixtureFile(t, childSummary, `{
		"info":{"id":"child-session","cwd":"/workspace/worktree"},
		"session_kind":"subagent",
		"session_summary":"child"
	}`)
	metaPath := filepath.Join(
		root, parentCWD, parentID, "subagents", childID, "meta.json",
	)
	writeGrokFixtureFile(t, metaPath, `{
		"subagent_id":"child-session",
		"parent_session_id":"parent-session",
		"child_session_id":"child-session",
		"status":"completed"
	}`)
	provider := newGrokTestProvider(t, root)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: metaPath,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, filepath.Clean(childSummary), filepath.Clean(changed[0].FingerprintKey))
}
