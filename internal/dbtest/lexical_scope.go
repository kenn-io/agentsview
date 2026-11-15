package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// SeedLexicalScope writes each lineage and anchor class with searchable sources.
func SeedLexicalScope(t *testing.T, d *db.DB) {
	t.Helper()
	for _, tc := range []struct {
		id, relationship   string
		sidechain, oneShot bool
	}{
		{id: "a-child", relationship: "subagent", oneShot: true},
		{id: "b-fork", relationship: "fork"},
		{id: "c-sidechain", sidechain: true},
		{id: "d-continuation", relationship: "continuation"},
		{id: "e-parent"},
		{id: "f-one-shot", oneShot: true},
	} {
		SeedSession(t, d, tc.id, "lexical-scope", func(s *db.Session) {
			s.UserMessageCount = 2
			s.MessageCount = 4
			if tc.oneShot {
				s.UserMessageCount = 1
				s.MessageCount = 3
			}
			s.StartedAt = new("2024-06-15T10:00:00Z")
			s.EndedAt = new("2024-06-15T11:00:00Z")
			s.RelationshipType = tc.relationship
			if tc.relationship != "" {
				s.ParentSessionID = new("e-parent")
			}
		})
		messages := []db.Message{
			{SessionID: tc.id, Ordinal: 0, Role: "user", Content: tc.id + " first prompt"},
			{
				SessionID: tc.id, Ordinal: 1, Role: "assistant", IsSidechain: tc.sidechain,
				Content: "scopeanchor", DialogueText: new("scopeanchor"), ThinkingText: "scopeanchor", HasThinking: true, HasToolUse: true,
				ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"scopeanchor"}`, ResultContent: "scopeanchor", ResultContentLength: len("scopeanchor")}},
			},
			{
				SessionID: tc.id, Ordinal: 2, Role: "assistant", IsSidechain: tc.sidechain, HasToolUse: true,
				ToolCalls: []db.ToolCall{{ToolName: "Task", Category: "Task", ToolUseID: "event", ResultEvents: []db.ToolResultEvent{{
					ToolUseID: "event", Source: "test", Status: "completed", Content: "eventanchor", ContentLength: len("eventanchor"),
				}}}},
			},
		}
		if !tc.oneShot {
			messages = append(messages, db.Message{SessionID: tc.id, Ordinal: 3, Role: "user", Content: "follow-up prompt"})
		}
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), tc.id, messages))
	}
}

// AssertLexicalScope checks scope before paging for every lexical source.
func AssertLexicalScope(t *testing.T, store db.Store) {
	t.Helper()
	for _, mode := range []string{"substring", "regex"} {
		for _, scope := range []string{"all", "top", "subordinate"} {
			for _, source := range []string{"messages", "thinking", "tool_input", "tool_result", "events"} {
				t.Run(mode+"/"+scope+"/"+source, func(t *testing.T) {
					want := []string{"a-child", "b-fork", "c-sidechain", "d-continuation", "e-parent"}
					switch scope {
					case "top":
						want = []string{"d-continuation", "e-parent"}
					case "subordinate":
						want = []string{"a-child", "b-fork", "c-sidechain"}
					}
					pattern := "scopeanchor"
					if source == "events" {
						pattern, source = "eventanchor", "tool_result"
					}
					f := db.ContentSearchFilter{Pattern: pattern, Mode: mode, Scope: scope, Sources: []string{source}, Project: "lexical-scope", Limit: 1}
					var got []string
					for range want {
						page, err := store.SearchContent(t.Context(), f)
						require.NoError(t, err)
						require.Len(t, page.Matches, 1)
						got = append(got, page.Matches[0].SessionID)
						if len(got) == len(want) {
							assert.Zero(t, page.NextCursor)
						} else {
							require.Equal(t, len(got), page.NextCursor)
						}
						f.Cursor = page.NextCursor
					}
					assert.Equal(t, want, got)
					f.Cursor, f.SessionID = 0, "a-child"
					page, err := store.SearchContent(t.Context(), f)
					require.NoError(t, err)
					if scope == "top" {
						assert.Empty(t, page.Matches)
					} else {
						require.Len(t, page.Matches, 1)
						assert.Equal(t, "a-child", page.Matches[0].SessionID)
					}
				})
			}
		}
	}
	for _, mode := range []string{"substring", "regex"} {
		for _, include := range []bool{false, true} {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "scopeanchor", Mode: mode, Project: "lexical-scope", Sources: []string{"messages"},
				IncludeOneShot: true, IncludeChildren: include, Limit: 50,
			})
			require.NoError(t, err)
			ids := make([]string, 0, len(page.Matches))
			for _, match := range page.Matches {
				ids = append(ids, match.SessionID)
			}
			if include {
				assert.Contains(t, ids, "a-child")
			} else {
				assert.NotContains(t, ids, "a-child")
			}
		}
	}

	_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "scopeanchor", Scope: "bogus"})
	require.Error(t, err)
}

// AssertHybridKeywordOnlyScope checks archive scope and citations without a vector index.
func AssertHybridKeywordOnlyScope(t *testing.T, store db.Store) {
	t.Helper()
	for _, tc := range []struct {
		scope string
		want  []string
	}{
		{"all", []string{"a-child", "b-fork", "c-sidechain", "d-continuation", "e-parent", "f-one-shot"}},
		{"top", []string{"d-continuation", "e-parent", "f-one-shot"}},
		{"subordinate", []string{"a-child", "b-fork", "c-sidechain"}},
	} {
		t.Run("hybrid_keyword/"+tc.scope, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "scopeanchor", Mode: "hybrid", KeywordOnly: true, Scope: tc.scope,
				Project: "lexical-scope", IncludeOneShot: true, Limit: 50,
			})
			require.NoError(t, err)
			var ids []string
			for _, match := range page.Matches {
				ids = append(ids, match.SessionID)
				assert.Equal(t, [2]int{1, 2}, match.OrdinalRange)
				assert.Equal(t, 1, match.Ordinal)
				assert.Equal(t, match.SessionID <= "c-sidechain", match.Subordinate)
			}
			assert.ElementsMatch(t, tc.want, ids)
			assert.Zero(t, page.NextCursor)
		})
	}
}
