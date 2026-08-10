package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture lines mirror the verified on-disk shape of a Claude Code
// background handoff (issue #1370): the fork transcript replays the
// original's chain entries byte-identically except for a rewritten
// sessionId and an injected sessionKind:"bg", then appends its own
// turns. The original interactive transcript carries no sessionKind.

func lineageUserLine(uuid, parentUUID, ts, sessionID, kind, content string) string {
	parent := "null"
	if parentUUID != "" {
		parent = fmt.Sprintf("%q", parentUUID)
	}
	kindField := ""
	if kind != "" {
		kindField = fmt.Sprintf(`"sessionKind":%q,`, kind)
	}
	return fmt.Sprintf(
		`{"type":"user","uuid":%q,"parentUuid":%s,"timestamp":%q,"sessionId":%q,%s"message":{"content":%q}}`,
		uuid, parent, ts, sessionID, kindField, content,
	)
}

func lineageAssistantLine(uuid, parentUUID, ts, sessionID, kind, msgID, text string, outputTokens int) string {
	kindField := ""
	if kind != "" {
		kindField = fmt.Sprintf(`"sessionKind":%q,`, kind)
	}
	return fmt.Sprintf(
		`{"type":"assistant","uuid":%q,"parentUuid":%q,"timestamp":%q,"sessionId":%q,%s"message":{"id":%q,"content":[{"type":"text","text":%q}],"usage":{"input_tokens":10,"output_tokens":%d}}}`,
		uuid, parentUUID, ts, sessionID, kindField, msgID, text, outputTokens,
	)
}

// lineageQueuedCommandLine mirrors the real queued_command attachment
// shape: these records carry no uuid. Verified against controlled
// fork-resume reproductions that Claude Code never replays uuid-less
// records into a fork transcript — only uuid-bearing chain entries are
// re-persisted — so a queued_command in a fork file is always the
// fork's own.
func lineageQueuedCommandLine(ts, sessionID, prompt string) string {
	return fmt.Sprintf(
		`{"type":"attachment","timestamp":%q,"sessionId":%q,"attachment":{"type":"queued_command","commandMode":"prompt","prompt":%q}}`,
		ts, sessionID, prompt,
	)
}

// writeLineageFixture writes original + fork transcripts into one
// directory and returns their paths.
func writeLineageFixture(t *testing.T, origName, origContent, forkName, forkContent string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	origPath := filepath.Join(dir, origName)
	forkPath := filepath.Join(dir, forkName)
	require.NoError(t, os.WriteFile(origPath, []byte(origContent), 0o644))
	require.NoError(t, os.WriteFile(forkPath, []byte(forkContent), 0o644))
	return origPath, forkPath
}

func lineageOriginalContent() string {
	return strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "orig-1111", "", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "orig-1111", "", "msg_01", "first answer", 20),
	}, "\n") + "\n"
}

func lineageForkContent() string {
	return strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "fork-2222", "bg", "continued question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "fork-2222", "bg", "msg_02", "continued answer", 7),
	}, "\n") + "\n"
}

func TestClaudeBackgroundForkTrimsReplayAndLinksParent(t *testing.T) {
	t.Parallel()
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", lineageForkContent(),
	)

	results, excluded, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, excluded)

	sess := results[0].Session
	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "continued question", msgs[0].Content)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "continued answer", msgs[1].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)

	assert.Equal(t, "orig-1111", sess.ParentSessionID)
	assert.Equal(t, RelContinuation, sess.RelationshipType)
	assert.Equal(t, "fork-2222", sess.ID)
	assert.Equal(t, "continued question", sess.FirstMessage)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Session bounds and usage come only from retained records.
	wantStart := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 1, 1, 11, 0, 5, 0, time.UTC)
	assert.True(t, sess.StartedAt.Equal(wantStart), "StartedAt = %v", sess.StartedAt)
	assert.True(t, sess.EndedAt.Equal(wantEnd), "EndedAt = %v", sess.EndedAt)
	assert.Equal(t, 7, sess.TotalOutputTokens)
}

func TestClaudeOriginalUnaffectedBySiblingFork(t *testing.T) {
	t.Parallel()
	origPath, _ := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", lineageForkContent(),
	)

	results, excluded, err := claudeParseFile(
		origPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, excluded)

	sess := results[0].Session
	require.Len(t, results[0].Messages, 2)
	assert.Equal(t, "first question", results[0].Messages[0].Content)
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, RelNone, sess.RelationshipType)
	assert.Equal(t, 2, sess.MessageCount)
}

func TestClaudeBackgroundForkPureReplayExcluded(t *testing.T) {
	t.Parallel()
	// The fork is a byte-level replay with no post-handoff turns: it
	// duplicates the original entirely and must be excluded rather
	// than stored as a second copy of the conversation.
	forkContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
	}, "\n") + "\n"
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", forkContent,
	)

	results, excluded, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.Equal(t, []string{"fork-2222"}, excluded)
}

func TestClaudeBackgroundForkChainTrimsAgainstNearestBgAncestor(t *testing.T) {
	t.Parallel()
	// Chained backgrounding: A (interactive) -> B (bg fork of A with
	// its own turns) -> C (bg fork of B with its own turns). C must
	// trim B's full replay and link to B, not partially trim against
	// A and keep B's turns duplicated. B still trims against A.
	aContent := lineageOriginalContent()
	bContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "bbbb-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "bbbb-2222", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "bbbb-2222", "bg", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "bbbb-2222", "bg", "msg_02", "second answer", 4),
	}, "\n") + "\n"
	cContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "cccc-3333", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "cccc-3333", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "cccc-3333", "bg", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "cccc-3333", "bg", "msg_02", "second answer", 4),
		lineageUserLine("u5", "a2", "2026-01-01T12:00:00Z", "cccc-3333", "bg", "third question"),
		lineageAssistantLine("a5", "u5", "2026-01-01T12:00:05Z", "cccc-3333", "bg", "msg_03", "third answer", 3),
	}, "\n") + "\n"

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "orig-1111.jsonl"), []byte(aContent), 0o644))
	bPath := filepath.Join(dir, "bbbb-2222.jsonl")
	require.NoError(t, os.WriteFile(bPath, []byte(bContent), 0o644))
	cPath := filepath.Join(dir, "cccc-3333.jsonl")
	require.NoError(t, os.WriteFile(cPath, []byte(cContent), 0o644))

	cResults, _, err := claudeParseFile(
		cPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, cResults, 1)
	require.Len(t, cResults[0].Messages, 2)
	assert.Equal(t, "third question", cResults[0].Messages[0].Content)
	assert.Equal(t, "bbbb-2222", cResults[0].Session.ParentSessionID)

	bResults, _, err := claudeParseFile(
		bPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, bResults, 1)
	require.Len(t, bResults[0].Messages, 2)
	assert.Equal(t, "second question", bResults[0].Messages[0].Content)
	assert.Equal(t, "orig-1111", bResults[0].Session.ParentSessionID)
}

func TestClaudeBackgroundForkQueuedOnlyChainTrimsAgainstEqualBgSibling(t *testing.T) {
	t.Parallel()
	// C replays B fully and holds only a uuid-less queued prompt, so
	// its uuid content equals B's exactly. Direction between equal
	// bg twins is elected deterministically by stem — the larger stem
	// trims against the smaller — so C must link to B and keep just
	// its queued prompt, not partially trim against A and duplicate
	// B's turns.
	aContent := lineageOriginalContent()
	bContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "bbbb-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "bbbb-2222", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "bbbb-2222", "bg", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "bbbb-2222", "bg", "msg_02", "second answer", 4),
	}, "\n") + "\n"
	cContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "cccc-3333", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "cccc-3333", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "cccc-3333", "bg", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "cccc-3333", "bg", "msg_02", "second answer", 4),
		lineageQueuedCommandLine("2026-01-01T12:00:00Z", "cccc-3333", "queued in c"),
	}, "\n") + "\n"

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "orig-1111.jsonl"), []byte(aContent), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bbbb-2222.jsonl"), []byte(bContent), 0o644))
	cPath := filepath.Join(dir, "cccc-3333.jsonl")
	require.NoError(t, os.WriteFile(cPath, []byte(cContent), 0o644))

	results, excluded, err := claudeParseFile(
		cPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, excluded)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "queued in c", results[0].Messages[0].Content)
	assert.Equal(t, "bbbb-2222", results[0].Session.ParentSessionID)
}

func TestClaudeBackgroundForkEqualBgTwinsElectKeeperByStem(t *testing.T) {
	t.Parallel()
	// Reverse stem order: the queued-only fork has the smaller stem,
	// so it never trims against its equal twin. The larger-stem twin
	// trims fully against the smaller and is excluded (it has no
	// content of its own), while the smaller falls back to the
	// interactive original. Every message is stored exactly once.
	aContent := lineageOriginalContent()
	smallContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "cccc-3333", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "cccc-3333", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "cccc-3333", "bg", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "cccc-3333", "bg", "msg_02", "second answer", 4),
		lineageQueuedCommandLine("2026-01-01T12:00:00Z", "cccc-3333", "queued in c"),
	}, "\n") + "\n"
	largeContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "dddd-4444", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "dddd-4444", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "dddd-4444", "bg", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "dddd-4444", "bg", "msg_02", "second answer", 4),
	}, "\n") + "\n"

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "orig-1111.jsonl"), []byte(aContent), 0o644))
	smallPath := filepath.Join(dir, "cccc-3333.jsonl")
	require.NoError(t, os.WriteFile(smallPath, []byte(smallContent), 0o644))
	largePath := filepath.Join(dir, "dddd-4444.jsonl")
	require.NoError(t, os.WriteFile(largePath, []byte(largeContent), 0o644))

	largeResults, largeExcluded, err := claudeParseFile(
		largePath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	assert.Empty(t, largeResults)
	assert.Equal(t, []string{"dddd-4444"}, largeExcluded)

	smallResults, smallExcluded, err := claudeParseFile(
		smallPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, smallResults, 1)
	assert.Empty(t, smallExcluded)
	var contents []string
	for _, m := range smallResults[0].Messages {
		contents = append(contents, m.Content)
	}
	assert.Contains(t, contents, "second question")
	assert.Contains(t, contents, "queued in c")
	assert.NotContains(t, contents, "first question")
	assert.Equal(t, "orig-1111", smallResults[0].Session.ParentSessionID)
}

func TestClaudeBackgroundForkBoundaryRetryFailsOpen(t *testing.T) {
	t.Parallel()
	// Two retained entries parented at the replay leaf (a retry right
	// after the handoff) cannot be re-rooted without collapsing the
	// retry branches into a linear merge. The trim fails open so the
	// intact DAG keeps retry semantics: only the latest branch shows.
	forkContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
		lineageAssistantLine("x1", "a1", "2026-01-01T11:00:00Z", "fork-2222", "bg", "msg_02", "abandoned retry", 2),
		lineageAssistantLine("x2", "a1", "2026-01-01T11:00:05Z", "fork-2222", "bg", "msg_03", "final answer", 6),
	}, "\n") + "\n"
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", forkContent,
	)

	results, excluded, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, excluded)
	assert.Empty(t, results[0].Session.ParentSessionID)
	var contents []string
	for _, m := range results[0].Messages {
		contents = append(contents, m.Content)
	}
	assert.Contains(t, contents, "first question")
	assert.Contains(t, contents, "final answer")
	assert.NotContains(t, contents, "abandoned retry")
}

func TestClaudeBackgroundForkKeepsQueuedCommandWithoutNewChainEntry(t *testing.T) {
	t.Parallel()
	// A prompt queued at handoff lands as a fork-own uuid-less
	// queued_command before any new chain entry exists. Even though
	// every uuid-bearing line is replayed, the fork holds a real user
	// message and must be kept and linked, not excluded.
	forkContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
		lineageQueuedCommandLine("2026-01-01T11:00:00Z", "fork-2222", "queued at handoff"),
	}, "\n") + "\n"
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", forkContent,
	)

	results, excluded, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, excluded)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "queued at handoff", results[0].Messages[0].Content)
	assert.Equal(t, "orig-1111", results[0].Session.ParentSessionID)
	assert.Equal(t, RelContinuation, results[0].Session.RelationshipType)
	assert.Equal(t, 1, results[0].Session.MessageCount)
}

func TestClaudeBackgroundForkFailOpen(t *testing.T) {
	t.Parallel()
	// Every ambiguous case must parse untrimmed and unlinked: the
	// worst allowed outcome is the status quo duplicate, never a
	// wrongly-oriented trim.
	tests := []struct {
		name        string
		origName    string
		origContent string
		forkContent string
	}{
		{
			name:        "fork not bg marked",
			origName:    "orig-1111.jsonl",
			origContent: lineageOriginalContent(),
			forkContent: strings.Join([]string{
				lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "", "first question"),
				lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "", "msg_01", "first answer", 20),
				lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "fork-2222", "", "continued question"),
			}, "\n") + "\n",
		},
		{
			name:     "sibling has different root",
			origName: "orig-1111.jsonl",
			origContent: strings.Join([]string{
				lineageUserLine("z9", "", "2026-01-01T09:00:00Z", "orig-1111", "", "unrelated"),
			}, "\n") + "\n",
			forkContent: lineageForkContent(),
		},
		{
			// A bg transcript fully covered by a bg sibling is either
			// that sibling's ancestor or its idle copy — direction is
			// unknowable, so it must not trim.
			name:     "fork is subset of bg sibling",
			origName: "long-3333.jsonl",
			origContent: strings.Join([]string{
				lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "long-3333", "bg", "first question"),
				lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "long-3333", "bg", "msg_01", "first answer", 20),
				lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "long-3333", "bg", "continued question"),
				lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "long-3333", "bg", "msg_02", "continued answer", 7),
			}, "\n") + "\n",
			forkContent: strings.Join([]string{
				lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
				lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
				lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "fork-2222", "bg", "continued question"),
			}, "\n") + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, forkPath := writeLineageFixture(t,
				tt.origName, tt.origContent,
				"fork-2222.jsonl", tt.forkContent,
			)
			results, excluded, err := claudeParseFile(
				forkPath, "my_app", "local",
				claudeParseOptions{siblingLineage: true},
			)
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Empty(t, excluded)
			sess := results[0].Session
			assert.Empty(t, sess.ParentSessionID)
			assert.Equal(t, RelNone, sess.RelationshipType)
			// Untrimmed: the replayed head is retained.
			assert.Equal(t, "first question",
				firstMessageContent(results[0].Messages))
		})
	}
}

func TestClaudeBackgroundForkNoSiblingFailsOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forkPath := filepath.Join(dir, "fork-2222.jsonl")
	require.NoError(t, os.WriteFile(forkPath, []byte(lineageForkContent()), 0o644))

	results, excluded, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, excluded)
	assert.Empty(t, results[0].Session.ParentSessionID)
	require.Len(t, results[0].Messages, 4)
}

// firstNonEmpty returns the first message content, requiring at least
// one message.
func firstMessageContent(msgs []ParsedMessage) string {
	for _, m := range msgs {
		if m.Content != "" {
			return m.Content
		}
	}
	return ""
}

func TestClaudeBackgroundForkKeepsPostBoundaryCoincidentalMatch(t *testing.T) {
	t.Parallel()
	// Only the contiguous leading replay region is trimmed. An entry
	// after the boundary whose uuid also appears in the ancestor must
	// be retained.
	origContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "orig-1111", "", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "orig-1111", "", "msg_01", "first answer", 20),
		lineageAssistantLine("x1", "a1", "2026-01-01T10:00:06Z", "orig-1111", "", "msg_0x", "stray original tail", 3),
	}, "\n") + "\n"
	forkContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u3", "a1", "2026-01-01T11:00:00Z", "fork-2222", "bg", "own turn"),
		lineageAssistantLine("x1", "u3", "2026-01-01T11:00:05Z", "fork-2222", "bg", "msg_03", "post boundary reuse", 5),
	}, "\n") + "\n"
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", origContent,
		"fork-2222.jsonl", forkContent,
	)

	results, _, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "own turn", msgs[0].Content)
	assert.Equal(t, "post boundary reuse", msgs[1].Content)
	assert.Equal(t, "orig-1111", results[0].Session.ParentSessionID)
}

func TestClaudeBackgroundForkPicksLongestReplayCandidate(t *testing.T) {
	t.Parallel()
	// Two interactive transcripts share the same root (a manual
	// interactive fork). The bg fork replays the longer one; its
	// parent must be the candidate whose replay run is longest.
	shortContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "orig-1111", "", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "orig-1111", "", "msg_01", "first answer", 20),
	}, "\n") + "\n"
	longContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "long-3333", "", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "long-3333", "", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T10:30:00Z", "long-3333", "", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T10:30:05Z", "long-3333", "", "msg_02", "second answer", 4),
	}, "\n") + "\n"
	forkContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T10:30:00Z", "fork-2222", "bg", "second question"),
		lineageAssistantLine("a2", "u2", "2026-01-01T10:30:05Z", "fork-2222", "bg", "msg_02", "second answer", 4),
		lineageUserLine("u5", "a2", "2026-01-01T11:00:00Z", "fork-2222", "bg", "fork only turn"),
	}, "\n") + "\n"

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "orig-1111.jsonl"), []byte(shortContent), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "long-3333.jsonl"), []byte(longContent), 0o644))
	forkPath := filepath.Join(dir, "fork-2222.jsonl")
	require.NoError(t, os.WriteFile(forkPath, []byte(forkContent), 0o644))

	results, _, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "fork only turn", results[0].Messages[0].Content)
	assert.Equal(t, "long-3333", results[0].Session.ParentSessionID)
}

func TestClaudeBackgroundForkKeepsOwnUUIDLessRecords(t *testing.T) {
	t.Parallel()
	// Claude Code never replays uuid-less records into a fork
	// transcript, so every uuid-less line in a fork is the fork's own:
	// spawn-time queue-operations interleaved before the replayed
	// chain, and queued_command attachments typed into the fork later.
	// Trimming must leave all of them alone — a position-based trim
	// that discarded everything through the replay boundary would eat
	// the fork's own head records.
	origContent := strings.Join([]string{
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "orig-1111", "", "first question"),
		lineageQueuedCommandLine("2026-01-01T10:00:02Z", "orig-1111", "queued in original"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "orig-1111", "", "msg_01", "first answer", 20),
	}, "\n") + "\n"
	forkContent := strings.Join([]string{
		// Fork-own spawn-time records precede the replayed chain.
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-01-01T11:00:00Z","sessionId":"fork-2222","content":"{\"tool_use_id\":\"tu-1\",\"task_id\":\"task-1\"}"}`,
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:01Z", "fork-2222", "bg", "continued question"),
		lineageQueuedCommandLine("2026-01-01T11:00:02Z", "fork-2222", "queued in fork"),
		lineageAssistantLine("a2", "u2", "2026-01-01T11:00:05Z", "fork-2222", "bg", "msg_02", "continued answer", 7),
	}, "\n") + "\n"
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", origContent,
		"fork-2222.jsonl", forkContent,
	)

	results, _, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	var prompts []string
	for _, m := range results[0].Messages {
		prompts = append(prompts, m.Content)
	}
	assert.Contains(t, prompts, "queued in fork")
	assert.Contains(t, prompts, "continued question")
	assert.NotContains(t, prompts, "first question")
	assert.Equal(t, "orig-1111", results[0].Session.ParentSessionID)
	// The fork-own enqueue record written before the replayed chain
	// survives the trim: its spawn timestamp is the session start.
	wantStart := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	assert.True(t, results[0].Session.StartedAt.Equal(wantStart),
		"StartedAt = %v", results[0].Session.StartedAt)

	// The original still splices its own queued command.
	origResults, _, err := claudeParseFile(
		filepath.Join(filepath.Dir(forkPath), "orig-1111.jsonl"),
		"my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, origResults, 1)
	var origPrompts []string
	for _, m := range origResults[0].Messages {
		origPrompts = append(origPrompts, m.Content)
	}
	assert.Contains(t, origPrompts, "queued in original")
}

func TestClaudeUploadParseIgnoresSiblingLineage(t *testing.T) {
	t.Parallel()
	// Uploaded transcripts are parsed under a user-chosen project and
	// must never trim against whatever directory they landed in.
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", lineageForkContent(),
	)

	results, err := parseClaudeSession(forkPath, "my_app", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Session.ParentSessionID)
	require.Len(t, results[0].Messages, 4)
}

func TestClaudeProviderParseTrimsBackgroundFork(t *testing.T) {
	t.Parallel()
	// End-to-end through the local provider: project-level sources
	// resolve sibling lineage.
	root := t.TempDir()
	projDir := filepath.Join(root, "-home-user-proj")
	require.NoError(t, os.MkdirAll(projDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(projDir, "orig-1111.jsonl"),
		[]byte(lineageOriginalContent()), 0o644))
	forkPath := filepath.Join(projDir, "fork-2222.jsonl")
	require.NoError(t, os.WriteFile(forkPath, []byte(lineageForkContent()), 0o644))

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(t, ok)

	sources := newClaudeSourceSet([]string{root})
	source, ok := sources.sourceRef(root, forkPath)
	require.True(t, ok)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, "orig-1111", sess.ParentSessionID)
	assert.Equal(t, RelContinuation, sess.RelationshipType)
	assert.Equal(t, 2, len(outcome.Results[0].Result.Messages))
}

func TestClaudeBackgroundForkIncrementalAppendContinuesFromTrim(t *testing.T) {
	t.Parallel()
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", lineageForkContent(),
	)

	results, _, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 2)

	info, err := os.Stat(forkPath)
	require.NoError(t, err)
	offset := info.Size()

	appended := lineageUserLine("u9", "a2", "2026-01-01T12:00:00Z", "fork-2222", "bg", "appended turn") + "\n"
	f, err := os.OpenFile(forkPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Stored identity mirrors what the engine persists from the full
	// parse; without it the identity-fill fallback escalates every
	// bg-marked append to a full parse.
	msgs, _, _, _, err := claudeParseSessionFrom(forkPath, offset, claudeIncrementalScan{
		startOrdinal:  2,
		lastEntryUUID: "a2",
		stored:        claudeStoredIdentity{sessionKind: "bg"},
	})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "appended turn", msgs[0].Content)
	// Ordinals continue from the trimmed message count, not the raw
	// replayed line count.
	assert.Equal(t, 2, msgs[0].Ordinal)
}

func TestClaudeLineageSniffGivesUpAfterBoundedPreamble(t *testing.T) {
	t.Parallel()
	// A fork whose bg root sits past the sniff bound fails open: the
	// resolver must not scan unbounded preamble looking for a chain
	// root.
	var preamble []string
	for i := range 400 {
		preamble = append(preamble,
			fmt.Sprintf(`{"type":"summary","summary":"noise %d","leafUuid":"leaf-%d"}`, i, i))
	}
	forkLines := append(preamble,
		lineageUserLine("u1", "", "2026-01-01T10:00:00Z", "fork-2222", "bg", "first question"),
		lineageAssistantLine("a1", "u1", "2026-01-01T10:00:05Z", "fork-2222", "bg", "msg_01", "first answer", 20),
		lineageUserLine("u2", "a1", "2026-01-01T11:00:00Z", "fork-2222", "bg", "continued question"),
	)
	forkContent := strings.Join(forkLines, "\n") + "\n"
	_, forkPath := writeLineageFixture(t,
		"orig-1111.jsonl", lineageOriginalContent(),
		"fork-2222.jsonl", forkContent,
	)

	results, _, err := claudeParseFile(
		forkPath, "my_app", "local",
		claudeParseOptions{siblingLineage: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Session.ParentSessionID)
	assert.Equal(t, "first question", firstMessageContent(results[0].Messages))
}
