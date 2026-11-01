//go:build pgtest

package postgres

import (
	"fmt"
	"math"
	"testing"

	"hegel.dev/go/hegel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rolloutRow struct {
	Group  string
	Hidden bool
}

func (f projectionFixture) rolloutRows(t require.TestingT) map[string]rolloutRow {
	rows, err := f.runtime.Query(`SELECT id,raw_group_id,rollout_hidden FROM sessions WHERE provenance_kind='raw'`)
	require.NoError(t, err)
	defer rows.Close()
	state := map[string]rolloutRow{}
	for rows.Next() {
		var id string
		var row rolloutRow
		require.NoError(t, rows.Scan(&id, &row.Group, &row.Hidden))
		state[id] = row
	}
	require.NoError(t, rows.Err())
	return state
}

// Moving either first/last tenant transition outside the recompute transaction
// must fail this model comparison, as must applying shadow to only one source
// of a group that is shared across devices.
func TestRawRolloutWritesMatchSerialModel(t *testing.T) {
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, "capture-"+device, "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome(device)))
	}
	shared, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Len(t, shared.Variants, 2)
	m, _ := f.accept(t, "device-c", "unrelated", "")
	other := projectionOutcome("other")
	other.Outcome.Results[0].Result.Session.ID, other.Outcome.Results[0].Result.Session.SourceSessionID = "codex:other", "other"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, other))
	expectedRows := f.rolloutRows(t)
	require.Len(t, expectedRows, 3, "two divergent cohorts and one unrelated row")
	groups := map[string]int{}
	for _, row := range expectedRows {
		groups[row.Group]++
	}
	require.Len(t, groups, 2)
	require.Equal(t, 2, groups[shared.GroupID])
	hegel.Test(t, func(ht *hegel.T) {
		// Each generated trace starts unscoped. IDs are a finite fixture universe;
		// operations range across all scope keys, states and clear transitions.
		_, err := f.admin.Exec(`DELETE FROM raw_rollout_scopes`)
		require.NoError(ht, err)
		require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "", "", "clear"))
		model := map[[2]string]string{{"", ""}: "shadow"}
		require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
		// Active scopes are stored fixtures for the later activation gate. The API
		// under test only writes shadow or clears them.
		for _, key := range [][2]string{{"", "codex"}, {"device-a", ""}, {"device-a", "codex"}} {
			if !hegel.Draw(ht, hegel.Booleans()) {
				continue
			}
			model[key] = "active"
			_, err = f.admin.Exec(`INSERT INTO raw_rollout_scopes(tenant_id,device_id,provider,state) VALUES($1,$2,$3,'active')`, f.tenant, key[0], key[1])
			require.NoError(ht, err)
		}
		require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
		assertRows := func() {
			want := map[string]rolloutRow{}
			for _, devices := range [][]string{{"device-a", "device-b"}, {"device-c"}} {
				active := len(model) == 0
				for _, device := range devices {
					rank, state := -1, "shadow"
					for selector, value := range model {
						if selector[0] != "" && selector[0] != device || selector[1] != "" && selector[1] != "codex" {
							continue
						}
						score := 0
						if selector[0] != "" {
							score += 2
						}
						if selector[1] != "" {
							score++
						}
						if score > rank {
							rank, state = score, value
						}
					}
					active = active || state == "active"
				}
				for id, row := range expectedRows {
					if (row.Group == shared.GroupID) == (len(devices) == 2) {
						row.Hidden = !active
						want[id] = row
					}
				}
			}
			require.Equal(ht, want, f.rolloutRows(ht), "scope writes must preserve every physical row and group, including rejected writes")
		}
		assertRows()
		ops := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 11)))
		for _, op := range ops {
			key := [][2]string{{"", ""}, {"", "codex"}, {"device-a", ""}, {"device-a", "codex"}, {"", "unknown"}, {"unknown", ""}}[op%6]
			action := "shadow"
			if op >= 6 {
				action = "clear"
			}
			invalid := key != [2]string{} && model[[2]string{}] == "" && action != "clear"
			if key == [2]string{} && action == "clear" && len(model) > 1 {
				invalid = true
			}
			var revision [2]int64
			require.NoError(ht, f.runtime.QueryRow(`SELECT corpus_revision,identity_revision FROM raw_corpus_state`).Scan(&revision[0], &revision[1]))
			err := ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, key[0], key[1], action)
			if invalid {
				require.Error(ht, err)
				var after [2]int64
				require.NoError(ht, f.runtime.QueryRow(`SELECT corpus_revision,identity_revision FROM raw_corpus_state`).Scan(&after[0], &after[1]))
				require.Equal(ht, revision, after, "rejected scope writes must be atomic")
			} else {
				require.NoError(ht, err)
				after := f.revisions(t)
				require.Equal(ht, revision[0]+1, after.corpus)
				require.Equal(ht, revision[1]+1, after.identity)
				if action == "clear" {
					delete(model, key)
				} else {
					model[key] = "shadow"
				}
			}
			var scopeCount int
			require.NoError(ht, f.runtime.QueryRow(`SELECT count(*) FROM raw_rollout_scopes`).Scan(&scopeCount))
			require.Equal(ht, len(model), scopeCount)
			assertRows()
		}
	}, hegel.WithTestCases(100), hegel.SuppressHealthCheck(hegel.TooSlow)) // Database I/O occurs between generated draws.
}

func TestRawRolloutListStoredParity(t *testing.T) {
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, "capture", "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("hello")))
	}
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	hegel.Test(t, func(ht *hegel.T) {
		shadow := hegel.Draw(ht, hegel.Booleans())
		state, hidden := "active", int64(0)
		if shadow {
			state, hidden = "shadow", 1
		}
		parity := fmt.Sprintf(`{"pass":%t,"differs":%d}`, !shadow, hegel.Draw(ht, hegel.Integers(0, math.MaxInt)))
		_, err := f.admin.Exec(`UPDATE raw_rollout_scopes SET state=$1,parity=$2::jsonb`, state, parity)
		require.NoError(ht, err)
		require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "device-a", "", "clear"))
		scopes, err := ListRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "", "")
		require.NoError(ht, err)
		require.Len(ht, scopes, 1)
		require.Equal(ht, hidden, scopes[0].Shadow)
		require.Equal(ht, 1-hidden, scopes[0].Served, "coalesced branches count once, including trash")
		require.JSONEq(ht, parity, string(scopes[0].Parity))
		require.Equal(ht, state, scopes[0].State)
		require.NotEmpty(ht, scopes[0].UpdatedBy)
	}, hegel.WithTestCases(100))
	assert.Error(t, ChangeRawRollout(t.Context(), f.runtime, f.schema, f.tenant, "", "", "clear"))
	assert.Error(t, ChangeRawRollout(t.Context(), f.admin, f.schema, "wrong-tenant", "", "", "clear"))
}

func TestRawRolloutSourceMembershipChangesEligibility(t *testing.T) {
	f := newProjectionFixture(t)
	a, accepted := f.accept(t, "device-a", "active", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("shared")))
	b, _ := f.accept(t, "device-b", "shadow", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("shared")))
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	_, err := f.admin.Exec(`INSERT INTO raw_rollout_scopes(tenant_id,device_id,state) VALUES($1,'device-a','active')`, f.tenant)
	require.NoError(t, err)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	initial := f.rolloutRows(t)
	require.Len(t, initial, 1)
	for _, row := range initial {
		require.False(t, row.Hidden)
	}
	for _, withdraw := range []bool{true, false} {
		a, accepted = f.accept(t, "device-a", fmt.Sprintf("withdraw-%t", withdraw), accepted.Receipt)
		outcome := projectionOutcome("shared")
		if withdraw {
			outcome.Outcome.Results = nil // Authoritative replacement, not a tombstone.
		}
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
		want := map[string]rolloutRow{}
		for id, row := range initial {
			row.Hidden = withdraw
			want[id] = row
		}
		require.Equal(t, want, f.rolloutRows(t))
		var active int
		require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_session_branches WHERE active`).Scan(&active))
		if withdraw {
			require.Equal(t, 1, active)
		} else {
			require.Equal(t, 2, active)
		}
	}
}

func TestRawRolloutNullParityRegression(t *testing.T) {
	f := newProjectionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	scopes, err := ListRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "")
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	require.Empty(t, scopes[0].Parity)
}
