package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// A Claude sub-agent transcript lives at
// <project>/<parent-session>/subagents/<agent-id>.jsonl, and the same named
// sub-agent can run again under a second parent session — for instance when the
// first session is resumed and dispatches the same agent. Both transcripts are
// the same sub-agent session: the id names the agent, and the second run
// continues where the first stopped.
//
// The parser built each transcript's session from its filename alone, so both
// files produced one session id and the second file's entries never reached the
// archive. Neither file was recorded as skipped, so the loss was invisible.
//
// These tests pin the join: the sub-agent id is unchanged, and the second
// transcript's entries are added to the session it continues.

const (
	subagentParentOne = "318a7ff4-4222-499a-8684-237a96139c85"
	subagentParentTwo = "9dc29f8d-b286-4bb1-b49d-3d3915ba9b4a"
	subagentAgentID   = "agent-code-reviewer-3f2a1b4c5d6e7f80"
)

// writeSubagentTranscript writes one parent session transcript plus the
// sub-agent transcript it dispatched, and returns the sub-agent transcript path.
func writeSubagentTranscript(
	t *testing.T, project, parent, prompt, reply, firstStamp, lastStamp string,
) string {
	t.Helper()
	dir := filepath.Join(project, parent, "subagents")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(project, parent+".jsonl"),
		[]byte(`{"type":"user","uuid":"p-`+parent+`","sessionId":"`+parent+
			`","timestamp":"2026-08-05T03:40:00.000Z",`+
			`"message":{"content":"run"}}`+"\n"),
		0o600))
	path := filepath.Join(dir, subagentAgentID+".jsonl")
	content := strings.Join([]string{
		`{"type":"user","uuid":"u-` + parent + `","sessionId":"` + subagentAgentID +
			`","timestamp":"` + firstStamp + `",` +
			`"message":{"content":"` + prompt + `"}}`,
		`{"type":"assistant","uuid":"a-` + parent + `","parentUuid":"u-` + parent +
			`","sessionId":"` + subagentAgentID +
			`","timestamp":"` + lastStamp + `","message":{"content":"` + reply + `"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func mustParseStamp(t *testing.T, stamp string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	require.NoError(t, err, "parse %s", stamp)
	return parsed
}

func TestClaudeSubagentContinuationMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, earlier, later, title string
	}{
		{"rename beats later AI title", `{"type":"custom-title","customTitle":"Review"}`, `{"type":"ai-title","aiTitle":"Generated"}`, "Review"},
		{"later rename clears title", `{"type":"custom-title","customTitle":"Review"}`, "{\"type\":\"system\",\"content\":\"<command-name>/rename</command-name><command-args></command-args>\"}\n{\"type\":\"ai-title\",\"aiTitle\":\"Generated\"}", ""},
		{"later rename replaces AI title", `{"type":"ai-title","aiTitle":"Generated"}`, `{"type":"custom-title","customTitle":"Review"}`, "Review"},
		{"latest AI title", `{"type":"ai-title","aiTitle":"Early"}`, `{"type":"ai-title","aiTitle":"Late"}`, "Late"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			project := filepath.Join(root, "project")
			first := writeSubagentTranscript(t, project, subagentParentOne,
				"first", "first reply", "2026-08-05T03:40:00Z", "2026-08-05T03:41:00Z")
			second := writeSubagentTranscript(t, project, subagentParentTwo,
				"second", "second reply", "2026-08-05T03:42:00Z", "2026-08-05T03:43:00Z")
			for i, path := range []string{first, second} {
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				fields, title := `"agentSetting":"reviewer","cwd":"/project/early",`, tc.earlier
				if i == 1 {
					fields, title = `"agentSetting":"later","entrypoint":"cli","sessionKind":"bg","cwd":"/project/late","gitBranch":"feature",`, tc.later
				}
				updated := strings.Replace(string(content), `"type":"user",`, `"type":"user",`+fields, 1)
				require.NoError(t, os.WriteFile(path, []byte(updated+title+"\n"), 0o600))
			}
			for i, path := range []string{first, second} {
				result := parseSubagentTranscript(t, root, path)
				assert.Equal(t, tc.title, result.Session.SessionName)
				assert.Equal(t, "reviewer", result.Session.AgentLabel)
				assert.Equal(t, "cli", result.Session.Entrypoint)
				assert.Equal(t, "bg", result.Session.SessionKind)
				assert.Equal(t, "/project/early", result.Session.Cwd)
				assert.Equal(t, "feature", result.Session.GitBranch)
				assert.Equal(t, path, result.Session.File.Path)
				assert.Equal(t, []string{subagentParentOne, subagentParentTwo}[i], result.Session.ParentSessionID)
			}
		})
	}
}

func TestClaudeSubagentContinuationPreservesForks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, start, end string
		duplicate        bool
	}{
		{"joined", "2026-08-05T03:40:00Z", "2026-08-05T03:41:00Z", false},
		{"overlapping", "2026-08-05T03:40:00Z", "2026-08-05T03:43:00Z", false},
		{"duplicate fork", "2026-08-05T03:40:00Z", "2026-08-05T03:41:00Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			project := filepath.Join(root, "project")
			first := writeSubagentTranscript(t, project, subagentParentOne,
				"earlier", "earlier reply", tc.start, tc.end)
			second := writeSubagentTranscript(t, project, subagentParentTwo,
				"later", "later reply", "2026-08-05T03:42:00Z", "2026-08-05T03:43:00Z")
			content := testjsonl.NewSessionBuilder().
				AddClaudeUserWithUUID("2026-08-05T03:42:00Z", "start", "a", "").
				AddClaudeAssistantWithUUID("2026-08-05T03:42:01Z", "reply", "b", "a").
				AddClaudeUserWithUUID("2026-08-05T03:42:02Z", "one", "c", "b").
				AddClaudeAssistantWithUUID("2026-08-05T03:42:03Z", "one reply", "d", "c").
				AddClaudeUserWithUUID("2026-08-05T03:42:04Z", "two", "e", "d").
				AddClaudeAssistantWithUUID("2026-08-05T03:42:05Z", "two reply", "f", "e").
				AddClaudeUserWithUUID("2026-08-05T03:42:06Z", "three", "g", "f").
				AddClaudeAssistantWithUUID("2026-08-05T03:42:07Z", "three reply", "h", "g").
				AddClaudeUserWithUUID("2026-08-05T03:42:08Z", "four", "k", "h").
				AddClaudeAssistantWithUUID("2026-08-05T03:42:09Z", "four reply", "l", "k").
				AddClaudeUserWithUUID("2026-08-05T03:43:00Z", "fork question", "i", "b").
				AddClaudeAssistantWithUUID("2026-08-05T03:43:01Z", "fork answer", "j", "i").String()
			require.NoError(t, os.WriteFile(second, []byte(content), 0o600))
			if tc.duplicate {
				third := writeSubagentTranscript(t, project, "third-parent", "unused", "unused", tc.start, tc.end)
				require.NoError(t, os.WriteFile(third, []byte(content), 0o600))
			}
			provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}, Machine: "local"})
			require.True(t, ok)
			for _, path := range []string{first, second} {
				sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: path})
				require.NoError(t, err)
				require.Len(t, sources, 1)
				outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
				require.NoError(t, err)
				require.Len(t, outcome.Results, 2)
				fork := outcome.Results[1].Result
				assert.Equal(t, subagentAgentID+"-i", fork.Session.ID)
				assert.Equal(t, subagentAgentID, fork.Session.ParentSessionID)
				assert.Equal(t, RelFork, fork.Session.RelationshipType)
				assert.Equal(t, second, fork.Session.File.Path)
				require.Len(t, fork.Messages, 2)
				assert.Equal(t, "fork question", fork.Messages[0].Content)
				assert.Equal(t, "fork answer", fork.Messages[1].Content)
			}
		})
	}
}

// parseSubagentTranscript parses one transcript through the provider, which is
// where a sub-agent session's companion transcripts are joined.
func parseSubagentTranscript(t *testing.T, root, path string) ParseResult {
	t.Helper()
	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots: []string{root}, Machine: "local",
	})
	require.True(t, ok, "construct claude provider")
	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{Path: path},
	)
	require.NoError(t, err, "SourcesForChangedPath")
	require.Len(t, sources, 1, "one source for the transcript")

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Machine: "local",
	})
	require.NoError(t, err, "Parse")
	for _, result := range outcome.Results {
		if result.Result.Session.ID == subagentAgentID {
			return result.Result
		}
	}
	require.FailNowf(t, "sub-agent session not parsed",
		"no %s session parsed from %s", subagentAgentID, path)
	return ParseResult{}
}

// TestClaudeSubagentUnderTwoParentsJoinsOneSession is the reproduction for the
// upstream report: one sub-agent id running under two parent sessions lost one
// transcript's entries. The session id stays the sub-agent's own id — it is
// written both in the sub-agent's transcript and in each parent's link to it —
// and the second transcript's entries are appended to it.
func TestClaudeSubagentUnderTwoParentsJoinsOneSession(t *testing.T) {
	t.Parallel()

	projects := t.TempDir()
	project := filepath.Join(projects, "-work-example-project")

	firstPath := writeSubagentTranscript(t, project, subagentParentOne,
		"review one", "done one",
		"2026-08-05T03:47:00.000Z", "2026-08-05T03:47:50.589Z")
	secondPath := writeSubagentTranscript(t, project, subagentParentTwo,
		"review two", "done two",
		"2026-08-05T03:47:55.807Z", "2026-08-05T03:48:07.294Z")

	// Both files are discovered: the walk does enter a second parent session's
	// subagents folder, which rules out "the folder is not walked" as the
	// explanation.
	found := map[string]bool{}
	for _, file := range ClaudeProjectSessionFiles(projects) {
		found[file.Path] = true
	}
	assert.True(t, found[firstPath],
		"the first parent's sub-agent file was not discovered")
	assert.True(t, found[secondPath],
		"the second parent's sub-agent file was not discovered")

	// No id is renamed: each transcript still parses to the sub-agent's own id,
	// which is what the parents' links to it spell.
	first, _, err := claudeParseWithExclusions(firstPath, "example-project", "local")
	require.NoError(t, err)
	require.Len(t, first, 1)
	second, _, err := claudeParseWithExclusions(secondPath, "example-project", "local")
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, subagentAgentID, first[0].Session.ID,
		"the sub-agent id names the agent")
	assert.Equal(t, first[0].Session.ID, second[0].Session.ID,
		"both transcripts belong to one sub-agent session")

	// The fix: whichever transcript the sync parses, the session carries both
	// runs' entries in the order the sub-agent ran them.
	for _, parsed := range []string{firstPath, secondPath} {
		result := parseSubagentTranscript(t, projects, parsed)
		texts := make([]string, 0, len(result.Messages))
		ordinals := make([]int, 0, len(result.Messages))
		for _, message := range result.Messages {
			texts = append(texts, message.Content)
			ordinals = append(ordinals, message.Ordinal)
		}
		assert.Equal(t,
			[]string{"review one", "done one", "review two", "done two"},
			texts,
			"parsing %s must carry both runs' entries", filepath.Base(parsed))
		assert.Equal(t, []int{0, 1, 2, 3}, ordinals,
			"ordinals continue across the companion instead of restarting")
		assert.Equal(t, 4, result.Session.MessageCount,
			"the stored message count covers both runs")
		assert.True(t,
			result.Session.EndedAt.Equal(mustParseStamp(t, "2026-08-05T03:48:07.294Z")),
			"the session ends at the later transcript's last entry, got %s",
			result.Session.EndedAt)
		assert.Equal(t, parsed, result.Session.File.Path,
			"the parsed transcript stays the session's stored file")
	}
}

func TestClaudeSubagentContinuationAcrossWorkflowPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, firstWorkflow string }{
		{"flat to nested", ""},
		{"different workflows", "workflows/first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			project := filepath.Join(root, "project")
			paths := []string{
				writeSubagentTranscript(t, project, subagentParentOne,
					"first", "first reply", "2026-08-05T03:40:00Z", "2026-08-05T03:41:00Z"),
				writeSubagentTranscript(t, project, subagentParentTwo,
					"second", "second reply", "2026-08-05T03:42:00Z", "2026-08-05T03:43:00Z"),
			}
			for i, workflow := range []string{tc.firstWorkflow, "workflows/second"} {
				if workflow == "" {
					continue
				}
				nested := filepath.Join(filepath.Dir(paths[i]), workflow, filepath.Base(paths[i]))
				require.NoError(t, os.MkdirAll(filepath.Dir(nested), 0o700))
				require.NoError(t, os.Rename(paths[i], nested))
				paths[i] = nested
			}
			for _, path := range paths {
				result := parseSubagentTranscript(t, root, path)
				texts := make([]string, len(result.Messages))
				ordinals := make([]int, len(result.Messages))
				for i, message := range result.Messages {
					texts[i], ordinals[i] = message.Content, message.Ordinal
				}
				assert.Equal(t, []string{"first", "first reply", "second", "second reply"}, texts)
				assert.Equal(t, []int{0, 1, 2, 3}, ordinals)
			}
		})
	}
}

func TestClaudeSubagentDiscoveryErrorFailsParse(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permissions enforced for a non-root user")
	}
	t.Parallel()

	for _, tc := range []struct {
		name   string
		parent string
		mode   os.FileMode
	}{
		{name: "project enumeration", mode: 0o111},
		{name: "companion stat", parent: subagentParentOne, mode: 0o000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projects := t.TempDir()
			project := filepath.Join(projects, "-work-example-project")
			writeSubagentTranscript(t, project, subagentParentOne,
				"review one", "done one",
				"2026-08-05T03:47:00.000Z", "2026-08-05T03:47:50.589Z")
			path := writeSubagentTranscript(t, project, subagentParentTwo,
				"review two", "done two",
				"2026-08-05T03:47:55.807Z", "2026-08-05T03:48:07.294Z")
			require.Len(t, parseSubagentTranscript(t, projects, path).Messages, 4)
			provider, ok := NewProvider(AgentClaude, ProviderConfig{
				Roots: []string{projects}, Machine: "local",
			})
			require.True(t, ok)
			sources, err := provider.SourcesForChangedPath(
				t.Context(), ChangedPathRequest{Path: path},
			)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			info, err := os.Stat(path)
			require.NoError(t, err)
			blocked := filepath.Join(project, tc.parent)
			t.Cleanup(func() { require.NoError(t, os.Chmod(blocked, 0o700)) })
			require.NoError(t, os.Chmod(blocked, tc.mode))

			outcome, err := provider.Parse(t.Context(), ParseRequest{
				Source: sources[0], Machine: "local",
			})
			require.ErrorIs(t, err, os.ErrPermission,
				"failed discovery must not publish a partial replacement")
			assert.Empty(t, outcome.Results)
			incremental, _, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
				Source: sources[0], Fingerprint: SourceFingerprint{Size: info.Size()},
			})
			require.ErrorIs(t, err, os.ErrPermission,
				"failed discovery must not fall through to incremental parsing")
			assert.Empty(t, incremental)

			require.NoError(t, os.Chmod(blocked, 0o700))
			assert.Len(t, parseSubagentTranscript(t, projects, path).Messages, 4)
		})
	}
}

func TestClaudeSubagentUnreadableCompanionFailsParse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file read permissions are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	t.Parallel()

	projects := t.TempDir()
	project := filepath.Join(projects, "-work-example-project")
	firstPath := writeSubagentTranscript(t, project, subagentParentOne,
		"review one", "done one",
		"2026-08-05T03:47:00.000Z", "2026-08-05T03:47:50.589Z")
	secondPath := writeSubagentTranscript(t, project, subagentParentTwo,
		"review two", "done two",
		"2026-08-05T03:47:55.807Z", "2026-08-05T03:48:07.294Z")
	require.Len(t, parseSubagentTranscript(t, projects, firstPath).Messages, 4)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots: []string{projects}, Machine: "local",
	})
	require.True(t, ok)
	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{Path: firstPath},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, os.Chmod(secondPath, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(secondPath, 0o600)) })

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Machine: "local",
	})
	require.ErrorIs(t, err, os.ErrPermission,
		"a failed companion read must not publish a partial replacement")
	assert.Empty(t, outcome.Results)

	require.NoError(t, os.Chmod(secondPath, 0o600))
	assert.Len(t, parseSubagentTranscript(t, projects, firstPath).Messages, 4,
		"a retry includes both transcripts once the companion is readable")
}

// TestClaudeSubagentOverlappingRunIsNotJoined pins the guard. Two transcripts
// that were running at the same time are not one dispatch continuing, so they
// are not glued together: the parsed session keeps its own entries and the other
// file stays unexplained rather than being silently absorbed.
func TestClaudeSubagentOverlappingRunIsNotJoined(t *testing.T) {
	t.Parallel()

	projects := t.TempDir()
	project := filepath.Join(projects, "-work-overlapping-project")

	firstPath := writeSubagentTranscript(t, project, subagentParentOne,
		"review one", "done one",
		"2026-08-05T03:47:00.000Z", "2026-08-05T03:47:50.589Z")
	// The second run starts while the first is still going.
	writeSubagentTranscript(t, project, subagentParentTwo,
		"review two", "done two",
		"2026-08-05T03:47:10.000Z", "2026-08-05T03:48:07.294Z")

	result := parseSubagentTranscript(t, projects, firstPath)
	texts := make([]string, 0, len(result.Messages))
	for _, message := range result.Messages {
		texts = append(texts, message.Content)
	}
	assert.Equal(t, []string{"review one", "done one"}, texts,
		"an overlapping run is not joined")
	assert.True(t,
		result.Session.EndedAt.Equal(mustParseStamp(t, "2026-08-05T03:47:50.589Z")),
		"the refused join leaves the session's own end in place, got %s",
		result.Session.EndedAt)
}

// TestClaudeSubagentSiblingTranscriptsStayWithinTheirProject pins the other half
// of the guard: a transcript of the same sub-agent under a different project is
// not a companion.
func TestClaudeSubagentSiblingTranscriptsStayWithinTheirProject(t *testing.T) {
	t.Parallel()

	projects := t.TempDir()
	firstPath := writeSubagentTranscript(t,
		filepath.Join(projects, "-work-project-one"), subagentParentOne,
		"review one", "done one",
		"2026-08-05T03:47:00.000Z", "2026-08-05T03:47:50.589Z")
	writeSubagentTranscript(t,
		filepath.Join(projects, "-work-project-two"), subagentParentTwo,
		"review two", "done two",
		"2026-08-05T03:47:55.807Z", "2026-08-05T03:48:07.294Z")

	siblings, err := claudeSubagentSiblingTranscripts(firstPath)
	require.NoError(t, err)
	assert.Empty(t, siblings,
		"a run in another project directory is not a companion")
}
