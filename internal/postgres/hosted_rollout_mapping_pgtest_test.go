//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
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
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "starred", true))
	require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 0, true, "mapping pin"))
	_, err = f.runtime.Exec(`INSERT INTO starred_sessions(session_id) VALUES('legacy-visible');
 INSERT INTO pinned_messages(session_id,message_id,ordinal,note) VALUES('legacy-visible',0,0,'mapping pin');
 INSERT INTO secret_findings(session_id,rule_name,confidence,location_kind,message_ordinal,match_start,match_end,match_index,redacted_match,rules_version)
 SELECT id,'synthetic','high','message',0,0,1,0,'redacted','test-rules' FROM sessions`)
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
		stars, err := h.ListStarredSessionIDs(t.Context())
		require.NoError(t, err)
		assert.Contains(t, stars, "legacy-visible")
		assert.NotContains(t, stars, "")
		pins, err := h.ListPinnedMessages(t.Context(), "", "")
		require.NoError(t, err)
		require.NotEmpty(t, pins)
		for _, pin := range pins {
			assert.NotEmpty(t, pin.SessionID)
		}
		aliases, err := h.FindSessionIDsByPartial(t.Context(), "", 100)
		require.NoError(t, err)
		assert.Contains(t, aliases, "legacy-visible")
		assert.NotContains(t, aliases, "")
		findings, err := h.ListSecretFindings(t.Context(), db.SecretFindingFilter{})
		require.NoError(t, err)
		require.NotEmpty(t, findings.Findings)
		for _, finding := range findings.Findings {
			assert.NotEmpty(t, finding.SessionID)
		}
	}
	read()
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	read()
}

// Each report must filter hidden rows before ID mapping, including map keys.
func TestHostedRolloutReportRowsKeepPublicIDs(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "report-mapping", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("report mapping")))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count,started_at) VALUES('visible','p','m','claude',2,1,'2026-01-01T00:00:00Z');
 INSERT INTO messages(session_id,ordinal,role,content,timestamp) VALUES('visible',0,'user','question','2026-01-01T00:00:00Z'),('visible',1,'assistant','answer','2026-01-01T00:01:00Z');
 INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,cost_microdollars,cost_status,cost_source,occurred_at) VALUES('visible','api','synthetic-model',11,7,10,'known','provider','2026-01-01T00:01:00Z');
 INSERT INTO tool_calls(session_id,message_ordinal,call_index,tool_name,category,file_path) VALUES('visible',1,0,'Edit','Edit','visible.go');
 UPDATE usage_events SET cost_microdollars=10,cost_status='known',cost_source='provider',occurred_at='2026-01-01T00:01:00Z';
 UPDATE messages SET timestamp=CASE WHEN ordinal=0 THEN '2026-01-01T00:00:00Z'::timestamptz ELSE '2026-01-01T00:01:00Z'::timestamptz END;
 UPDATE tool_calls SET category='Edit',file_path='raw.go' WHERE session_id<>'visible'`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	af := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-02", Timezone: "UTC"}
	uf := db.UsageFilter{From: "2026-01-01", To: "2026-01-02", Timezone: "UTC"}
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	check := func(want []string) {
		t.Helper()
		top, err := h.GetAnalyticsTopSessions(t.Context(), af, "messages")
		require.NoError(t, err)
		var ids []string
		for _, row := range top.Sessions {
			ids = append(ids, row.ID)
		}
		assert.ElementsMatch(t, want, ids, "analytics IDs")
		costs, err := h.GetTopSessionsByCost(t.Context(), uf, 10)
		require.NoError(t, err)
		ids = nil
		for _, row := range costs {
			ids = append(ids, row.SessionID)
		}
		assert.ElementsMatch(t, want, ids, "cost IDs")
		edits, err := h.RecentEdits(t.Context(), db.RecentEditsParams{})
		require.NoError(t, err)
		ids = nil
		for _, file := range edits.Files {
			ids = append(ids, file.LastSessionID)
			for _, edit := range file.Edits {
				assert.NotEmpty(t, edit.SessionID)
			}
		}
		assert.ElementsMatch(t, want, ids, "edit IDs")
		artifacts, err := h.BuildActivityReportArtifacts(t.Context(), af, q, nil)
		require.NoError(t, err)
		ids = nil
		for id := range artifacts.Membership {
			ids = append(ids, id)
		}
		assert.ElementsMatch(t, want, ids, "activity membership keys")
	}
	check([]string{"codex:portable", "visible"})
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	check([]string{"visible"})
}
