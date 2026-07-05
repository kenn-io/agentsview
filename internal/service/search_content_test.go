package service_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

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
		"my key is AKIA7QHWN2DKR4FYPLJM ok")
	be := service.NewDirectBackend(d, nil)

	// default: secret should be redacted
	res, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "AKIA", Mode: "substring", Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.False(t, strings.Contains(res.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM"),
		"default search leaked secret: %q", res.Matches[0].Snippet)

	// reveal: full secret should be present
	rev, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "AKIA", Mode: "substring", Limit: 50, Reveal: true,
	})
	require.NoError(t, err)
	require.Len(t, rev.Matches, 1)
	assert.True(t, strings.Contains(rev.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM"),
		"reveal should show full secret: %q", rev.Matches[0].Snippet)
}

func TestDirectSearchContentFTSSourceGuard(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil)

	_, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
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
	page    db.ContentSearchPage
	windows map[string][]db.Message // keyed by contextWindowKey
}

func contextWindowKey(sessionID string, anchor int) string {
	return fmt.Sprintf("%s:%d", sessionID, anchor)
}

func (f *fakeContentStore) SearchContent(
	context.Context, db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	return f.page, nil
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

	res, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
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

	res, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
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

	_, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
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
	msgs[1].Content = "my key is AKIA7QHWN2DKR4FYPLJM ok"
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
	res, err := redacted.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "match", Context: 2,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	require.Len(t, res.Matches[0].ContextBefore, 2)
	assert.False(t,
		strings.Contains(res.Matches[0].ContextBefore[1].Content, "AKIA7QHWN2DKR4FYPLJM"),
		"default (Reveal=false) must redact a secret in a context message: %q",
		res.Matches[0].ContextBefore[1].Content)

	revealed := service.NewReadOnlyBackend(newStore())
	rev, err := revealed.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "match", Context: 2, Reveal: true,
	})
	require.NoError(t, err)
	require.Len(t, rev.Matches, 1)
	require.Len(t, rev.Matches[0].ContextBefore, 2)
	assert.True(t,
		strings.Contains(rev.Matches[0].ContextBefore[1].Content, "AKIA7QHWN2DKR4FYPLJM"),
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
	secret := "AKIA7QHWN2DKR4FYPLJM"
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

	res, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
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

	res, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "match", Context: 2,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.Nil(t, res.Matches[0].ContextBefore)
	assert.Nil(t, res.Matches[0].ContextAfter)
}
