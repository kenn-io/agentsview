//go:build pgtest

package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"hegel.dev/go/hegel"
)

func localEditTimestamp(t *testing.T, value string) string {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed.Local().Format(time.RFC3339)
}

// Filtering after ranking would still leak counts, timestamps and pagination.
func TestHostedRolloutRecentEdits(t *testing.T) {
	f := newHostedFixture(t, "tenant-edits")
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count) VALUES
		('visible','p','m','claude',2),('shadow','p','m','claude',2);
		INSERT INTO messages(session_id,ordinal,role,content,timestamp) VALUES
		('visible',0,'assistant','shared edit','2026-01-01T00:01:00Z'),
		('visible',1,'assistant','visible edit','2026-01-01T00:02:00Z'),
		('shadow',0,'assistant','newer shared edit','2026-01-01T00:03:00Z'),
		('shadow',1,'assistant','hidden file','2026-01-01T00:04:00Z');
		INSERT INTO tool_calls(session_id,message_ordinal,tool_use_id,tool_name,category,file_path) VALUES
		('visible',0,'v0','Edit','Edit','shared.go'),('visible',1,'v1','Write','Write','visible.go'),
		('shadow',0,'h0','Edit','Edit','shared.go'),('shadow',1,'h1','Write','Write','hidden.go');
		UPDATE sessions SET rollout_hidden=true WHERE id='shadow'`)
	require.NoError(t, err)
	edit := func(id, call, tool, ts string, ordinal int) db.RecentEdit {
		return db.RecentEdit{SessionID: id, Ordinal: ordinal, ToolUseID: call, ToolName: tool, Category: tool, Timestamp: ts}
	}
	v0 := edit("visible", "v0", "Edit", localEditTimestamp(t, "2026-01-01T00:01:00Z"), 0)
	v1 := edit("visible", "v1", "Write", localEditTimestamp(t, "2026-01-01T00:02:00Z"), 1)
	h0 := edit("shadow", "h0", "Edit", localEditTimestamp(t, "2026-01-01T00:03:00Z"), 0)
	h1 := edit("shadow", "h1", "Write", localEditTimestamp(t, "2026-01-01T00:04:00Z"), 1)
	file := func(path string, count int, edits ...db.RecentEdit) db.RecentEditFile {
		return db.RecentEditFile{Project: "p", FilePath: path, EditCount: count, LastEditedAt: edits[0].Timestamp, LastSessionID: edits[0].SessionID, Edits: edits}
	}
	read := func(p db.RecentEditsParams, want db.RecentEditsResult) {
		t.Helper()
		got, err := h.RecentEdits(t.Context(), p)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	read(db.RecentEditsParams{}, db.RecentEditsResult{Files: []db.RecentEditFile{file("visible.go", 1, v1), file("shared.go", 1, v0)}})
	read(db.RecentEditsParams{Limit: 1}, db.RecentEditsResult{Files: []db.RecentEditFile{file("visible.go", 1, v1)}, HasMore: true})
	read(db.RecentEditsParams{Limit: 1, Offset: 1}, db.RecentEditsResult{Files: []db.RecentEditFile{file("shared.go", 1, v0)}})
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=false WHERE id='shadow'`)
	require.NoError(t, err)
	read(db.RecentEditsParams{}, db.RecentEditsResult{Files: []db.RecentEditFile{file("hidden.go", 1, h1), file("shared.go", 2, h0, v0), file("visible.go", 1, v1)}})
	shared := file("shared.go", 2, h0)
	shared.EditsTruncated = true
	read(db.RecentEditsParams{Limit: 1, Offset: 1, MaxEditsPerFile: 1}, db.RecentEditsResult{Files: []db.RecentEditFile{shared}, HasMore: true})
}

// These signatures deliberately bypass the inventory's nullable-error helper.
func TestHostedRolloutDirectIdentityReads(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "direct", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("direct")))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,source_session_id,project,machine,agent,message_count) VALUES('legacy','legacy-resume','p','m','claude',2)`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	for _, id := range []string{"codex:portable", "legacy"} {
		for _, state := range []string{"visible", "hidden", "trash"} {
			_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=$1,deleted_at=CASE WHEN $2 THEN NOW() ELSE NULL END`, state == "hidden", state == "trash")
			require.NoError(t, err)
			count, version, present := h.GetSessionVersion(t.Context(), id)
			watch, err := h.GetSessionWatchState(id)
			require.NoError(t, err)
			resume, err := h.GetProviderResumeID(t.Context(), id)
			if state == "hidden" {
				assert.False(t, present)
				assert.Zero(t, count)
				assert.Zero(t, version)
				assert.Equal(t, "gone", watch.State)
				assert.Empty(t, watch.PublicID)
				var gone *db.SessionIdentityError
				require.ErrorAs(t, err, &gone)
				assert.Equal(t, "gone", gone.State)
				assert.Empty(t, resume)
			} else {
				require.NoError(t, err)
				assert.True(t, present)
				assert.Equal(t, 2, count)
				assert.NotZero(t, version)
				assert.Equal(t, db.SessionWatchResolved, watch.State)
				assert.Equal(t, id, watch.PublicID)
				want := "portable"
				if id == "legacy" {
					want = "legacy-resume"
				}
				assert.Equal(t, want, resume)
			}
		}
	}
}

func TestHostedRolloutEligibilityModel(t *testing.T) {
	requireHegel(t)
	f := newHostedFixture(t, "tenant-eligibility")
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	hegel.Test(t, func(ht *hegel.T) {
		_, err := f.runtime.Exec(`DELETE FROM sessions`)
		require.NoError(ht, err)
		states := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 3)))
		// Materialize a bounded prefix; each state independently spans all four
		// combinations of rollout and user deletion, including empty corpora.
		states = states[:min(len(states), 12)]
		var wantLive, wantTrash []string
		for i, state := range states {
			id := fmt.Sprintf("legacy-%02d", i)
			hidden, trash := state&1 != 0, state&2 != 0
			_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count,rollout_hidden,deleted_at) VALUES($1,'p','m','codex',1,2,$2,CASE WHEN $3 THEN NOW() ELSE NULL END)`, id, hidden, trash)
			require.NoError(ht, err)
			_, err = f.runtime.Exec(`INSERT INTO pinned_messages(session_id,message_id,ordinal) VALUES($1,0,0)`, id)
			require.NoError(ht, err)
			pins, err := h.core.pinReferencePage(ht.Context(), id, "")
			require.NoError(ht, err)
			identity, err := h.resolve(ht.Context(), id)
			require.NoError(ht, err)
			_, starErr := h.StarSession(ht.Context(), id)
			if hidden {
				require.Empty(ht, pins)
				require.Empty(ht, identity.SessionID)
				require.Error(ht, starErr)
			} else {
				require.Len(ht, pins, 1)
				require.Equal(ht, id, identity.SessionID)
				require.NoError(ht, starErr)
			}
			if !hidden {
				if trash {
					wantTrash = append(wantTrash, id)
				} else {
					wantLive = append(wantLive, id)
				}
			}
		}
		page, err := h.ListSessions(ht.Context(), db.SessionFilter{Limit: 100})
		require.NoError(ht, err)
		var got []string
		for _, s := range page.Sessions {
			got = append(got, s.ID)
		}
		require.ElementsMatch(ht, wantLive, got)
		require.Equal(ht, len(wantLive), page.Total)
		stats, err := h.GetStats(ht.Context(), false, false)
		require.NoError(ht, err)
		require.Equal(ht, len(wantLive), stats.SessionCount)
		trash, err := h.ListTrashedSessions(ht.Context())
		require.NoError(ht, err)
		got = nil
		for _, s := range trash {
			got = append(got, s.ID)
		}
		require.ElementsMatch(ht, wantTrash, got)
		removed, err := h.EmptyTrash(ht.Context())
		require.NoError(ht, err)
		require.Equal(ht, len(wantTrash), removed)
		var retained int
		require.NoError(ht, f.runtime.QueryRow(`SELECT count(*) FROM sessions`).Scan(&retained))
		require.Equal(ht, len(states)-len(wantTrash), retained)
	}, hegel.WithTestCases(1000))
}

// Hegel shrank the eligibility-model failure to one hidden, live legacy row.
func TestHostedRolloutHiddenLegacyDoesNotEnterPages(t *testing.T) {
	f := newHostedFixture(t, "tenant-hidden-legacy")
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count,rollout_hidden) VALUES('legacy-00','p','m','codex',1,true)`)
	require.NoError(t, err)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	assert.Empty(t, page.Sessions)
	assert.Zero(t, page.Total)
}

func TestHostedRolloutLegacyTimingHidesNestedDuration(t *testing.T) {
	f := newHostedFixture(t, "tenant-timing")
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,started_at,ended_at,message_count) VALUES
		('parent','p','m','codex','2026-01-01T00:00:00Z','2026-01-01T00:01:00Z',2),
		('child','p','m','codex','2026-01-01T00:00:01Z','2026-01-01T00:00:08Z',1);
		INSERT INTO messages(session_id,ordinal,role,content,timestamp,has_tool_use) VALUES
		('parent',0,'user','hello','2026-01-01T00:00:00Z',false),('parent',1,'assistant','','2026-01-01T00:00:01Z',true);
		INSERT INTO tool_calls(session_id,message_ordinal,tool_name,category,subagent_session_id) VALUES('parent',1,'Task','agent','child')`)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`UPDATE sessions SET parent_session_id='parent',relationship_type='subagent' WHERE id='child'`)
	require.NoError(t, err)
	timing, err := h.GetSessionTiming(t.Context(), "parent")
	require.NoError(t, err)
	require.NotNil(t, timing.SlowestCall)
	require.NotNil(t, timing.SlowestCall.DurationMs)
	assert.EqualValues(t, 7000, *timing.SlowestCall.DurationMs)
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=true WHERE id='child'`)
	require.NoError(t, err)
	timing, err = h.GetSessionTiming(t.Context(), "parent")
	require.NoError(t, err)
	for _, turn := range timing.Turns {
		for _, call := range turn.Calls {
			assert.Empty(t, call.SubagentSessionID)
			assert.Nil(t, call.DurationMs)
		}
	}
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=(id='parent')`)
	require.NoError(t, err)
	for _, limit := range []int{0, 1} {
		page, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: limit})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1, "visible children of hidden parents become roots")
		assert.Equal(t, "child", page.Sessions[0].ID)
	}
}
