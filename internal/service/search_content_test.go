package service_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

const fakeAWSKey = "AKIA" + "7QHWN2DKR4FYPLJM"

// seedServiceSearchSession creates a session with a single user message
// whose content contains the given text. The session has UserMessageCount=2
// so it is not excluded by the default one-shot filter.
func seedServiceSearchSession(
	t *testing.T, d *db.DB, id, project, msgContent string,
) {
	t.Helper()
	dbtest.SeedSessionWithMessages(t, d, id, project, []db.Message{
		dbtest.UserMsg(id, 0, msgContent),
		dbtest.AsstMsg(id, 1, "understood"),
	}, dbtest.WithMessageCounts(3, 2))
}

func TestDirectSearchContentRedacts(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	seedServiceSearchSession(t, d, "x1", "proj",
		"my key is "+fakeAWSKey+" ok")
	be := service.NewDirectBackend(d, nil)

	// default: secret should be redacted
	res, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "AKIA", Mode: "substring", Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.NotContains(t, res.Matches[0].Snippet, fakeAWSKey,
		"default search leaked secret: %q", res.Matches[0].Snippet)

	// reveal: full secret should be present
	rev, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "AKIA", Mode: "substring", Limit: 50, Reveal: true,
	})
	require.NoError(t, err)
	require.Len(t, rev.Matches, 1)
	assert.Contains(t, rev.Matches[0].Snippet, fakeAWSKey,
		"reveal should show full secret: %q", rev.Matches[0].Snippet)
}

func TestDirectSearchContentFTSSourceGuard(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil)

	_, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "test", Mode: "fts",
		Sources: []string{"tool_result"},
		Limit:   50,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

// fakeContentStore is a minimal db.Store fake for context-enrichment tests:
// only SearchContent and GetMessagesWindow are implemented; every other
// Store method comes from the embedded nil interface and would panic if a
// test path reached it (none of these tests exercise anything else).
type fakeContentStore struct {
	db.Store
	page           db.ContentSearchPage
	pagesByPattern map[string]db.ContentSearchPage
	lastFilter     db.ContentSearchFilter
	filters        []db.ContentSearchFilter
	windows        map[string][]db.Message // keyed by contextWindowKey
}

func contextWindowKey(sessionID string, anchor int) string {
	return fmt.Sprintf("%s:%d", sessionID, anchor)
}

func (f *fakeContentStore) SearchContent(
	_ context.Context, filter db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	f.lastFilter = filter
	f.filters = append(f.filters, filter)
	if page, ok := f.pagesByPattern[filter.Pattern]; ok {
		return page, nil
	}
	return f.page, nil
}

func TestDirectSearchContentConceptsIntersectAndRankSessions(t *testing.T) {
	store := &fakeContentStore{pagesByPattern: map[string]db.ContentSearchPage{
		"auth model": {Matches: []db.ContentMatch{
			{SessionID: "both", Ordinal: 4, OrdinalRange: [2]int{3, 5}, Snippet: "auth evidence", Score: new(0.7)},
			{SessionID: "both", Ordinal: 8, OrdinalRange: [2]int{8, 9}, Snippet: "weaker auth", Score: new(0.4)},
			{SessionID: "auth-only", Ordinal: 2, OrdinalRange: [2]int{2, 2}, Snippet: "auth only", Score: new(0.95)},
			{SessionID: "tie", Ordinal: 1, OrdinalRange: [2]int{1, 2}, Snippet: "tie auth", Score: new(0.8)},
		}},
		"retry policy": {Matches: []db.ContentMatch{
			{SessionID: "both", Project: "agentsview", Agent: "codex", Ordinal: 12, OrdinalRange: [2]int{11, 13}, Snippet: "retry evidence", Score: new(0.9)},
			{SessionID: "tie", Project: "agentsview", Agent: "codex", Ordinal: 6, OrdinalRange: [2]int{6, 7}, Snippet: "tie retry", Score: new(0.8)},
		}},
	}}
	be := service.NewReadOnlyBackend(store)

	res, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Concepts: []string{"auth model", "retry policy"},
		Project:  "agentsview", Agent: "codex", SessionID: "target",
		GitBranchExact: "feature/memory", DateFrom: "2026-08-01",
		DateTo: "2026-09-20", ExcludeSessionIDs: []string{"current"},
		Scope: "top", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 2)
	assert.Equal(t, "both", res.Matches[0].SessionID)
	assert.InDelta(t, 0.8, *res.Matches[0].Score, 0.0001)
	assert.Equal(t, "tie", res.Matches[1].SessionID)
	assert.InDelta(t, 0.8, *res.Matches[1].Score, 0.0001)
	require.Len(t, res.Matches[0].ConceptEvidence, 2)
	assert.Equal(t, "auth model", res.Matches[0].ConceptEvidence[0].Concept)
	assert.Equal(t, [2]int{3, 5}, res.Matches[0].ConceptEvidence[0].OrdinalRange)
	assert.Equal(t, "auth evidence", res.Matches[0].ConceptEvidence[0].Snippet)
	assert.Equal(t, "retry policy", res.Matches[0].ConceptEvidence[1].Concept)
	assert.Equal(t, [2]int{11, 13}, res.Matches[0].ConceptEvidence[1].OrdinalRange)

	require.Len(t, store.filters, 2)
	for _, filter := range store.filters {
		assert.Equal(t, "semantic", filter.Mode)
		assert.Equal(t, "agentsview", filter.Project)
		assert.Equal(t, "codex", filter.Agent)
		assert.Equal(t, "target", filter.SessionID)
		assert.Equal(t, "feature/memory", filter.GitBranchExact)
		assert.Equal(t, "2026-08-01", filter.DateFrom)
		assert.Equal(t, "2026-09-20", filter.DateTo)
		assert.Equal(t, []string{"current"}, filter.ExcludeSessionIDs)
		assert.Equal(t, "top", filter.Scope)
		assert.Equal(t, 50, filter.Limit)
		assert.Zero(t, filter.Cursor)
	}
}

func TestDirectSearchContentConceptsValidateAndBoundCandidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  service.ContentSearchRequest
		want string
	}{
		{"pattern", service.ContentSearchRequest{Pattern: "x", Concepts: []string{"a", "b"}}, "pattern and concepts"},
		{"one", service.ContentSearchRequest{Concepts: []string{"a"}}, "between 2 and 5"},
		{"six", service.ContentSearchRequest{Concepts: []string{"a", "b", "c", "d", "e", "f"}}, "between 2 and 5"},
		{"blank", service.ContentSearchRequest{Concepts: []string{"a", " "}}, "must not be blank"},
		{"duplicate", service.ContentSearchRequest{Concepts: []string{"a", "a"}}, "must be distinct"},
		{"mode", service.ContentSearchRequest{Concepts: []string{"a", "b"}, Mode: "hybrid"}, "mode must be semantic"},
		{"cursor", service.ContentSearchRequest{Concepts: []string{"a", "b"}, Cursor: 1}, "cursor is not supported"},
		{"limit", service.ContentSearchRequest{Concepts: []string{"a", "b"}, Limit: 51}, "limit must be between 1 and 50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeContentStore{}
			_, err := service.NewReadOnlyBackend(store).SearchContent(t.Context(), tc.req)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, store.filters)
		})
	}

	store := &fakeContentStore{pagesByPattern: map[string]db.ContentSearchPage{
		"a": {}, "b": {},
	}}
	_, err := service.NewReadOnlyBackend(store).SearchContent(t.Context(), service.ContentSearchRequest{
		Concepts: []string{"a", "b"}, Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, store.filters, 2)
	assert.Equal(t, 250, store.filters[0].Limit)
	assert.Equal(t, 250, store.filters[1].Limit)
}

func TestDirectSearchContentConceptsApplyFinalLimitAndStableTie(t *testing.T) {
	leg := func(concept string) db.ContentSearchPage {
		return db.ContentSearchPage{Matches: []db.ContentMatch{
			{SessionID: "z-session", Ordinal: 1, Snippet: concept, Score: new(0.5)},
			{SessionID: "a-session", Ordinal: 1, Snippet: concept, Score: new(0.5)},
		}}
	}
	store := &fakeContentStore{pagesByPattern: map[string]db.ContentSearchPage{
		"a": leg("a"), "b": leg("b"),
	}}
	res, err := service.NewReadOnlyBackend(store).SearchContent(t.Context(), service.ContentSearchRequest{
		Concepts: []string{"a", "b"}, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.Equal(t, "a-session", res.Matches[0].SessionID)
	assert.True(t, res.CandidateTruncated)
	assert.Equal(t, 5, store.filters[0].Limit)
}

func TestDirectSearchContentTermsAndExactFilters(t *testing.T) {
	store := &fakeContentStore{}
	be := service.NewReadOnlyBackend(store)

	_, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "alpha beta", Mode: "terms",
		SessionID: "session-1", GitBranchExact: "feature/memory",
		Scope: "top", Limit: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"messages"}, store.lastFilter.Sources)
	assert.Equal(t, "session-1", store.lastFilter.SessionID)
	assert.Equal(t, "feature/memory", store.lastFilter.GitBranchExact)
	assert.Equal(t, "top", store.lastFilter.Scope)

	_, err = be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "alpha beta", Mode: "terms",
		Sources: []string{"tool_result"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

func (f *fakeContentStore) GetMessagesWindow(
	_ context.Context, sessionID string, w db.MessageWindow,
) ([]db.Message, error) {
	return f.windows[contextWindowKey(sessionID, *w.Around)], nil
}

// contextWindowFixture builds the before/anchor/after messages
// GetMessagesWindow(Around) would return for an anchor ordinal, ascending.
func contextWindowFixture(sessionID string, anchor int) []db.Message {
	return []db.Message{
		{SessionID: sessionID, Ordinal: anchor - 2, Role: "user", Content: "before2"},
		{SessionID: sessionID, Ordinal: anchor - 1, Role: "assistant", Content: "before1"},
		{SessionID: sessionID, Ordinal: anchor, Role: "user", Content: "anchor"},
		{SessionID: sessionID, Ordinal: anchor + 1, Role: "assistant", Content: "after1"},
		{SessionID: sessionID, Ordinal: anchor + 2, Role: "user", Content: "after2"},
	}
}

func TestDirectSearchContentContextEnrichment(t *testing.T) {
	t.Parallel()
	const sess = "s1"
	matches := []db.ContentMatch{
		{SessionID: sess, Ordinal: 5, Snippet: "match one"},
		{SessionID: sess, Ordinal: 20, Snippet: "match two"},
	}
	store := &fakeContentStore{
		page: db.ContentSearchPage{Matches: matches},
		windows: map[string][]db.Message{
			contextWindowKey(sess, 5):  contextWindowFixture(sess, 5),
			contextWindowKey(sess, 20): contextWindowFixture(sess, 20),
		},
	}
	be := service.NewReadOnlyBackend(store)

	res, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "match", Context: 2,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 2)
	for _, m := range res.Matches {
		require.Len(t, m.ContextBefore, 2)
		assert.Equal(t, "before2", m.ContextBefore[0].Content)
		assert.Equal(t, "before1", m.ContextBefore[1].Content)
		require.Len(t, m.ContextAfter, 2)
		assert.Equal(t, "after1", m.ContextAfter[0].Content)
		assert.Equal(t, "after2", m.ContextAfter[1].Content)
		combined := slices.Concat(m.ContextBefore, m.ContextAfter)
		for _, cm := range combined {
			assert.NotEqual(t, m.Ordinal, cm.Ordinal, "anchor row must be excluded")
		}
	}
}

func TestDirectSearchContentContextZeroLeavesNil(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{
		page: db.ContentSearchPage{
			Matches: []db.ContentMatch{{SessionID: "s1", Ordinal: 5}},
		},
	}
	be := service.NewReadOnlyBackend(store)

	res, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "match",
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.Nil(t, res.Matches[0].ContextBefore)
	assert.Nil(t, res.Matches[0].ContextAfter)
}

func TestDirectSearchContentContextRejectsOverMax(t *testing.T) {
	t.Parallel()
	be := service.NewReadOnlyBackend(&fakeContentStore{})

	_, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "match", Context: 11,
	})
	require.Error(t, err)
	assert.Equal(t, "context: maximum is 10", err.Error())
}

// contextWindowFixtureWithSecret is contextWindowFixture but with an AWS
// access key planted in the message immediately before the anchor, so tests
// can assert that context enrichment redacts (or reveals) it independently
// of the match's own Snippet redaction.
func contextWindowFixtureWithSecret(sessionID string, anchor int) []db.Message {
	msgs := contextWindowFixture(sessionID, anchor)
	msgs[1].Content = "my key is " + fakeAWSKey + " ok"
	return msgs
}

// TestDirectSearchContentContextRedactsSecretsByDefault verifies that a
// secret-shaped span in a context message (not the match itself) is
// redacted in ContextBefore/ContextAfter when the request does not reveal,
// and left intact when it does. This must hold regardless of transport
// (HTTP, CLI, MCP) since the redaction happens once in
// directBackend.enrichContentContext.
func TestDirectSearchContentContextRedactsSecretsByDefault(t *testing.T) {
	t.Parallel()
	const sess = "s1"
	newStore := func() *fakeContentStore {
		return &fakeContentStore{
			page: db.ContentSearchPage{
				Matches: []db.ContentMatch{{SessionID: sess, Ordinal: 5, Snippet: "match one"}},
			},
			windows: map[string][]db.Message{
				contextWindowKey(sess, 5): contextWindowFixtureWithSecret(sess, 5),
			},
		}
	}

	redacted := service.NewReadOnlyBackend(newStore())
	res, err := redacted.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "match", Context: 2,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	require.Len(t, res.Matches[0].ContextBefore, 2)
	assert.NotContains(t, res.Matches[0].ContextBefore[1].Content, fakeAWSKey,
		"default (Reveal=false) must redact a secret in a context message: %q",
		res.Matches[0].ContextBefore[1].Content)

	revealed := service.NewReadOnlyBackend(newStore())
	rev, err := revealed.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "match", Context: 2, Reveal: true,
	})
	require.NoError(t, err)
	require.Len(t, rev.Matches, 1)
	require.Len(t, rev.Matches[0].ContextBefore, 2)
	assert.Contains(t, rev.Matches[0].ContextBefore[1].Content, fakeAWSKey,
		"Reveal=true must leave a context message's secret intact: %q",
		rev.Matches[0].ContextBefore[1].Content)
}

// TestDirectSearchContentContextRedactsToolPayloads verifies that a secret
// carried in a context message's tool call payloads (input_json,
// result_content, and a result event's content) is also redacted by
// default, not just the message's own Content field.
func TestDirectSearchContentContextRedactsToolPayloads(t *testing.T) {
	t.Parallel()
	const sess = "s1"
	secret := fakeAWSKey
	store := &fakeContentStore{
		page: db.ContentSearchPage{
			Matches: []db.ContentMatch{{SessionID: sess, Ordinal: 5, Snippet: "match one"}},
		},
		windows: map[string][]db.Message{
			contextWindowKey(sess, 5): {
				{
					SessionID: sess, Ordinal: 4, Role: "assistant",
					Content: "calling a tool",
					ToolCalls: []db.ToolCall{{
						ToolName:      "bash",
						InputJSON:     fmt.Sprintf(`{"cmd":"export KEY=%s"}`, secret),
						ResultContent: "key is " + secret,
						ResultEvents: []db.ToolResultEvent{
							{Source: "stdout", Content: "leaked: " + secret},
						},
					}},
				},
				{SessionID: sess, Ordinal: 5, Role: "user", Content: "anchor"},
				{SessionID: sess, Ordinal: 6, Role: "assistant", Content: "after"},
			},
		},
	}
	be := service.NewReadOnlyBackend(store)

	res, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "match", Context: 2,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	require.Len(t, res.Matches[0].ContextBefore, 1)
	tc := res.Matches[0].ContextBefore[0].ToolCalls
	require.Len(t, tc, 1)
	assert.NotContains(t, tc[0].InputJSON, secret, "tool input_json must be redacted")
	assert.NotContains(t, tc[0].ResultContent, secret, "tool result_content must be redacted")
	require.Len(t, tc[0].ResultEvents, 1)
	assert.NotContains(t, tc[0].ResultEvents[0].Content, secret,
		"tool result event content must be redacted")
}

func TestDirectSearchContentContextSkipsNegativeOrdinal(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{
		page: db.ContentSearchPage{
			Matches: []db.ContentMatch{{SessionID: "s1", Ordinal: -1}},
		},
	}
	be := service.NewReadOnlyBackend(store)

	res, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "match", Context: 2,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.Nil(t, res.Matches[0].ContextBefore)
	assert.Nil(t, res.Matches[0].ContextAfter)
}
