package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

// A cursor repointed at a system row must not surface that
// row's content: the continuation applies the same visibility contract as
// the listing path before slicing.
func TestGetMessages_BodyCursorRejectsSystemRow(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSessionWithMessages(t, d, "sysrow", "proj", []db.Message{
		{
			SessionID: "sysrow", Ordinal: 0, Role: "system",
			Content: "secret system prompt", IsSystem: true,
			ContentLength: len("secret system prompt"),
		},
		dbtest.UserMsg("sysrow", 1, "abcdefghij"),
	}, dbtest.WithMessageCounts(2, 1))

	_, first, err := ts.getMessages(t.Context(), nil, getMessagesIn{
		SessionID: "sysrow", Limit: 2, MaxCharsPerMessage: 4,
	})
	require.NoError(t, err)
	require.Len(t, first.Messages, 1, "the system row must be filtered from the listing")
	require.NotEmpty(t, first.Messages[0].BodyCursor)

	// Re-sign a cursor that points at the hidden system row's ordinal.
	cursor, err := decodeMessageBodyCursor(first.Messages[0].BodyCursor)
	require.NoError(t, err)
	cursor.Ordinal = 0
	systemCursor := encodeMessageBodyCursor(cursor)

	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{
		SessionID: "sysrow", BodyCursor: systemCursor, MaxCharsPerMessage: 4,
	})
	require.ErrorIs(t, err, service.ErrSourceChanged)
	require.ErrorContains(t, err, "cited message is not available")
	assert.Empty(t, out.Messages)
}

// A backend that cannot bind reads to a transcript revision or evidence
// source (for example an older daemon whose responses carry neither
// binding) must not hand out body cursors: decodeMessageBodyCursor rejects
// cursors without both bindings, so the advertised continuation could never
// run. Pagination stays available instead.
func TestGetMessages_TruncatedWithoutRevisionBindingsKeepsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"session_id":"legacy","ordinal":0,` +
			`"role":"user","content":"abcdefghij","content_length":10}],` +
			`"count":1,"first_ordinal":0,"last_ordinal":0}`))
	}))
	t.Cleanup(srv.Close)

	ts := &toolset{
		svc: servicehttp.NewHTTPBackend(srv.URL, "", false, ""),
		now: func() time.Time { return fixedNow },
	}
	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{
		SessionID: "legacy", Limit: 1, MaxCharsPerMessage: 4,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	require.True(t, out.Messages[0].Truncated)
	assert.Empty(t, out.Messages[0].BodyCursor,
		"no cursor without a transcript revision and evidence source")
	require.NotNil(t, out.NextFrom,
		"pagination must remain available when no cursor is issued")
	assert.Equal(t, 1, *out.NextFrom)
}

// A cursor issued under explicit roles must continue under exactly those
// roles: the issuing roles ride inside the cursor, so an explicit-role
// listing (here roles: ["tool"]) gets a working continuation even though the
// continuation request itself carries no Roles.
func TestGetMessages_BodyCursorContinuesUnderIssuingRoles(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSessionWithMessages(t, d, "toolroles", "proj", []db.Message{
		dbtest.UserMsg("toolroles", 0, "abcdefghij"),
		{
			SessionID: "toolroles", Ordinal: 1, Role: "tool",
			Content:       "remainder text",
			ContentLength: len("remainder text"),
		},
	}, dbtest.WithMessageCounts(2, 2))

	_, first, err := ts.getMessages(t.Context(), nil, getMessagesIn{
		SessionID: "toolroles", Roles: []string{"tool"},
		Limit: 2, MaxCharsPerMessage: 4,
	})
	require.NoError(t, err)
	require.Len(t, first.Messages, 1)
	require.Equal(t, "tool", first.Messages[0].Role)
	require.Equal(t, "remainder text"[:4], first.Messages[0].Content)
	require.NotEmpty(t, first.Messages[0].BodyCursor)

	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{
		SessionID: "toolroles", BodyCursor: first.Messages[0].BodyCursor,
		MaxCharsPerMessage: 4,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	assert.Equal(t, "remainder text"[4:8], out.Messages[0].Content)

	// Drain the rest through the follow-up cursors.
	parts := []string{out.Messages[0].Content}
	for out.Messages[0].Truncated {
		_, out, err = ts.getMessages(t.Context(), nil, getMessagesIn{
			SessionID: "toolroles", BodyCursor: out.Messages[0].BodyCursor,
			MaxCharsPerMessage: 4,
		})
		require.NoError(t, err)
		require.Len(t, out.Messages, 1)
		parts = append(parts, out.Messages[0].Content)
	}
	assert.Equal(t, "remainder text"[4:], strings.Join(parts, ""))
}
