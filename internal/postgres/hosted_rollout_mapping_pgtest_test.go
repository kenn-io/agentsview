//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// A rollout transition must never erase IDs from rows a public reader returns.
// The visible peer prevents an empty result from making this check vacuous.
func TestHostedRolloutReturnedRowsKeepPublicIDs(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "mapping", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("mapping raw")))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count,started_at)
 VALUES('legacy-visible','p','m','claude',2,1,'2026-01-01T00:00:00Z');
 INSERT INTO messages(session_id,ordinal,role,content) VALUES
 ('legacy-visible',0,'user','mapping question'),('legacy-visible',1,'assistant','mapping answer')`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	read := func() {
		t.Helper()
		page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
		require.NoError(t, err)
		var ids []string
		for _, session := range page.Sessions {
			assert.NotEmpty(t, session.ID)
			ids = append(ids, session.ID)
		}
		assert.Contains(t, ids, "legacy-visible")
		sidebar, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 10})
		require.NoError(t, err)
		ids = nil
		for _, session := range sidebar.Sessions {
			assert.NotEmpty(t, session.ID)
			ids = append(ids, session.ID)
		}
		assert.Contains(t, ids, "legacy-visible")
		search, err := h.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "mapping", IncludeOneShot: true})
		require.NoError(t, err)
		ids = nil
		for _, match := range search.Matches {
			assert.NotEmpty(t, match.SessionID)
			ids = append(ids, match.SessionID)
		}
		assert.Contains(t, ids, "legacy-visible")
	}
	read()
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	read()
}
