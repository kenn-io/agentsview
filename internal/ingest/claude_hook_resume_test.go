package ingest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	nextUser := user
	nextUser.SourceUUID = "u2"
	idlessUser := user
	idlessUser.SourceUUID = ""
	for _, subtype := range []string{"hook_success", "hook_cancelled", "hook_additional_context"} {
		t.Run(subtype+" only", func(t *testing.T) {
			t.Parallel()
			row := hook
			row.SourceSubtype = subtype
			assert.Equal(t, "a1", ingest.LastParsedSourceUUID("a1", []parser.ParsedMessage{row}))
		})
	}
	for _, tc := range []struct {
		name    string
		current string
		rows    []parser.ParsedMessage
		want    string
	}{
		{name: "no current cursor", rows: []parser.ParsedMessage{hook, secondHook}, want: ""},
		{name: "ordinary tail before hook", current: "old", rows: []parser.ParsedMessage{user, assistant, hook, secondHook}, want: "a1"},
		{name: "ordinary after hooks", current: "old", rows: []parser.ParsedMessage{assistant, hook, nextUser}, want: "u2"},
		{name: "ID-less ordinary tail", current: "old", rows: []parser.ParsedMessage{assistant, hook, idlessUser}, want: "a1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ingest.LastParsedSourceUUID(tc.current, tc.rows))
		})
	}
}
