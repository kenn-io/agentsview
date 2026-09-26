package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	t.Fatalf("no %s session parsed from %s", subagentAgentID, path)
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

	assert.Empty(t, claudeSubagentSiblingTranscripts(firstPath),
		"a run in another project directory is not a companion")
}
