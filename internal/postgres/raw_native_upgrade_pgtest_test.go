//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawNativeIdentityUpgradePreservesCuration(t *testing.T) {
	for _, mode := range []string{"curated", "trashed", "excluded", "conflicting_name", "conflicting_pin"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			old := projectionOutcome("hello")
			old.Outcome.Results[0].Result.Session.SourceSessionID = ""
			m, accepted := f.accept(t, "device-a", "old", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, old))
			before, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			oldVariant := "codex:portable~" + rawDigest("branch-v1", before.GroupID, rawSourceID(m))
			require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "display_name", "saved name"))
			require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "starred", true))
			require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 0, true, "saved note"))
			if mode == "trashed" || mode == "excluded" {
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
			}
			if mode == "excluded" {
				removed, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
				require.NoError(t, err)
				require.True(t, removed)
			}
			if mode == "conflicting_name" || mode == "conflicting_pin" {
				other, _ := f.accept(t, "device-b", "native", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, projectionOutcome("hello")))
				var branch string
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT branch_id FROM raw_session_branches WHERE source_id=$1`, rawSourceID(other)).Scan(&branch))
				if mode == "conflicting_name" {
					require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable~"+branch, "display_name", "other name"))
				} else {
					require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable~"+branch, 0, true, "other note"))
				}
			}
			m, _ = f.accept(t, "device-a", "new", accepted.Receipt)
			err = f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("hello"))
			if mode == "conflicting_name" || mode == "conflicting_pin" {
				require.ErrorContains(t, err, "native identity promotion conflicts")
				var active bool
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT active FROM raw_session_branches WHERE group_id=$1`, before.GroupID).Scan(&active))
				assert.True(t, active)
				return
			}
			require.NoError(t, err)
			after, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			variant, err := f.sink.Resolve(t.Context(), oldVariant)
			require.NoError(t, err)
			assert.Equal(t, after.State, variant.State)
			assert.Equal(t, after.SessionID, variant.SessionID)
			var retained int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_source_contributions c JOIN raw_session_branches b USING(branch_id) WHERE b.group_id=$1`, before.GroupID).Scan(&retained))
			assert.Equal(t, 1, retained)
			if mode == "excluded" {
				assert.Equal(t, RawIdentityGone, after.State)
				return
			}
			require.Equal(t, RawIdentityUnique, after.State)
			assert.NotEqual(t, before.GroupID, after.GroupID)
			session, err := (&Store{pg: f.runtime}).GetSessionFull(t.Context(), after.SessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.DisplayName)
			assert.Equal(t, "saved name", *session.DisplayName)
			assert.Equal(t, mode == "trashed", session.DeletedAt != nil)
			pins, err := f.sink.PinReferences(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.True(t, pins[0].Resolved)
			assert.Equal(t, "saved note", pins[0].Note)
			var starred bool
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=$1)`, after.SessionID).Scan(&starred))
			assert.Equal(t, mode != "trashed", starred)
			var active int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_session_branches WHERE active`).Scan(&active))
			assert.Equal(t, 1, active)
		})
	}
}

func TestRawNativeIdentityUpgradeKeepsExistingNativeTrash(t *testing.T) {
	f := newProjectionFixture(t)
	native, _ := f.accept(t, "device-b", "native", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, native), native, projectionOutcome("hello")))
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
	old := projectionOutcome("hello")
	old.Outcome.Results[0].Result.Session.SourceSessionID = ""
	m, accepted := f.accept(t, "device-a", "old", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, old))
	var group string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT group_id FROM raw_session_branches WHERE source_id=$1`, rawSourceID(m)).Scan(&group))
	variant := "codex:portable~" + rawDigest("branch-v1", group, rawSourceID(m))
	require.NoError(t, f.sink.SetCuration(t.Context(), variant, "trashed", false))
	m, _ = f.accept(t, "device-a", "new", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("hello")))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, resolved.State)
	session, err := (&Store{pg: f.runtime}).GetSessionFull(t.Context(), resolved.SessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.NotNil(t, session.DeletedAt)
}
