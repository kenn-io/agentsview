package ingest_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestClaudeHookResumeCursor(t *testing.T) {
	t.Parallel()
	user := parser.ParsedMessage{Role: parser.RoleUser, SourceType: "user", SourceUUID: "u1"}
	assistant := parser.ParsedMessage{Role: parser.RoleAssistant, SourceType: "assistant", SourceUUID: "a1", SourceParentUUID: "u1"}
	hook := parser.ParsedMessage{Role: parser.RoleUser, IsSystem: true, SourceType: "attachment", SourceSubtype: "hook_success", SourceUUID: "h1"}
	secondHook := hook
	secondHook.SourceUUID = "h2"
	idlessHook := hook
	idlessHook.SourceUUID = ""
	nextUser := user
	nextUser.SourceUUID = "u2"
	idlessUser := user
	idlessUser.SourceUUID = ""
	for _, tc := range []struct {
		name    string
		current string
		rows    []parser.ParsedMessage
		want    string
	}{
		{name: "empty window", current: "a1", want: "a1"},
		{name: "hook only", current: "a1", rows: []parser.ParsedMessage{hook}, want: "a1"},
		{name: "no current cursor", rows: []parser.ParsedMessage{hook, secondHook}, want: ""},
		{name: "ordinary tail before hook", current: "old", rows: []parser.ParsedMessage{user, assistant, hook}, want: "a1"},
		{name: "multiple hooks", current: "old", rows: []parser.ParsedMessage{user, assistant, hook, secondHook}, want: "a1"},
		{name: "ordinary after hooks", current: "old", rows: []parser.ParsedMessage{assistant, hook, nextUser}, want: "u2"},
		{name: "ID-less hook", current: "old", rows: []parser.ParsedMessage{assistant, idlessHook}, want: "a1"},
		{name: "ID-less ordinary tail", current: "old", rows: []parser.ParsedMessage{assistant, hook, idlessUser}, want: "a1"},
		{name: "only ID-less records", current: "old", rows: []parser.ParsedMessage{idlessHook, idlessUser}, want: "old"},
		{name: "another attachment subtype", current: "old", rows: []parser.ParsedMessage{hook,
			{SourceType: "attachment", SourceSubtype: "queued_command", SourceUUID: "at1"}}, want: "at1"},
		{name: "same subtype on another type", current: "old", rows: []parser.ParsedMessage{hook,
			{IsSystem: true, SourceType: "user", SourceSubtype: "hook_success", SourceUUID: "u9"}}, want: "u9"},
		{name: "another hook subtype", current: "old", rows: []parser.ParsedMessage{hook,
			{IsSystem: true, SourceType: "attachment", SourceSubtype: "hook_error", SourceUUID: "h3"}}, want: "h3"},
		{name: "other system row", current: "old", rows: []parser.ParsedMessage{hook,
			{IsSystem: true, SourceType: "system", SourceSubtype: "compact_boundary", SourceUUID: "b1"}}, want: "b1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := slices.Clone(tc.rows)
			assert.Equal(t, tc.want, ingest.LastParsedSourceUUID(tc.current, tc.rows))
			assert.Equal(t, before, tc.rows, "cursor selection must not erase source identity")
		})
	}
}

func TestClaudeHookResumeConversionIdentity(t *testing.T) {
	t.Parallel()
	userDialogue, assistantDialogue, hookDialogue := "hello", "answer", ""
	parsed := []parser.ParsedMessage{
		{Ordinal: 0, Role: parser.RoleUser, SourceType: "user", SourceUUID: "u1", Content: "hello", DialogueText: &userDialogue},
		{Ordinal: 1, Role: parser.RoleAssistant, SourceType: "assistant", SourceUUID: "a1", SourceParentUUID: "u1", Content: "answer", DialogueText: &assistantDialogue},
		{Ordinal: 2, Role: parser.RoleUser, IsSystem: true, SourceType: "attachment", SourceSubtype: "hook_success", SourceUUID: "h1", SourceParentUUID: "a1", Content: "hook output", DialogueText: &hookDialogue},
	}
	before := slices.Clone(parsed)
	for i := range before {
		before[i].DialogueText = new(*parsed[i].DialogueText)
	}
	messages, err := ingest.ConvertMessagesContext(t.Context(), "hook-resume", parser.AgentClaude, parsed, nil)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	hook := messages[2]
	assert.Equal(t, "h1", hook.SourceUUID)
	assert.Equal(t, "a1", hook.SourceParentUUID)
	assert.Equal(t, "hook output", hook.Content)
	assert.Equal(t, string(parser.RoleUser), hook.Role)
	assert.True(t, hook.IsSystem)
	require.NotNil(t, hook.DialogueText)
	assert.Empty(t, *hook.DialogueText)
	assert.Equal(t, before, parsed, "conversion must not mutate source rows or their dialogue")
}
