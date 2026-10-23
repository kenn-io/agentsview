//go:build pgtest

package postgres

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// The first group after a successful raw-only activation must carry forward in
// its own projection transaction. No operator gate runs again for later content.
func TestRawActivationFutureGroupReplacementAndWithdrawal(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "seed", "")
	seed := projectionOutcome("seed")
	seed.Outcome.Results[0].Result.Session.ID = "codex:seed"
	seed.Outcome.Results[0].Result.Session.SourceSessionID = "seed"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, seed))
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	activateFixture(t, t.Context(), f, "", "")
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,display_name) VALUES('codex:portable','retained','device','codex','future legacy'); INSERT INTO messages(session_id,ordinal,role,content,source_uuid) VALUES('codex:portable',0,'user','new','uuid'); INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note) VALUES('codex:portable',0,0,'uuid','retained pin')`)
	require.NoError(t, err)
	legacy := activationLegacySnapshot(t, f)
	next, receipt := f.accept(t, "device-b", "first-future", "")
	out := projectionOutcome("new")
	out.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, out))
	var hidden bool
	var group, physical, name string
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
	assert.True(t, hidden)
	require.NoError(t, f.runtime.QueryRow(`SELECT group_id FROM raw_legacy_supersessions WHERE legacy_session_id='codex:portable'`).Scan(&group))
	require.NoError(t, f.runtime.QueryRow(`SELECT id,display_name FROM sessions WHERE raw_group_id=$1`, group).Scan(&physical, &name))
	assert.Equal(t, "future legacy", name)
	replacement, accepted := f.accept(t, "device-b", "replacement", receipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, replacement), replacement, projectionOutcome("replacement")))
	var replaced string
	require.NoError(t, f.runtime.QueryRow(`SELECT id FROM sessions WHERE raw_group_id=$1`, group).Scan(&replaced))
	assert.NotEqual(t, physical, replaced)
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
	assert.True(t, hidden)
	_, tombReceipt := f.tombstone(t, replacement, "tombstone", accepted.Receipt)
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
	assert.True(t, hidden, "tombstone retains logical membership")
	withdrawn, _ := f.accept(t, "device-b", "withdraw", tombReceipt.Receipt)
	empty := projectionOutcome("unused")
	empty.Outcome.Results = nil
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, withdrawn), withdrawn, empty))
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
	assert.False(t, hidden)
	var count int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&count))
	assert.Zero(t, count)
	assert.Equal(t, legacy, activationLegacySnapshot(t, f))
}

func activationLegacySnapshot(t require.TestingT, f projectionFixture) string {
	var value string
	require.NoError(t, f.runtime.QueryRow(`SELECT jsonb_build_object('session',to_jsonb(s)-'rollout_hidden','messages',(SELECT jsonb_agg(to_jsonb(m) ORDER BY ordinal) FROM messages m WHERE m.session_id=s.id),'pins',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM pinned_messages p WHERE p.session_id=s.id),'starred',EXISTS(SELECT 1 FROM starred_sessions x WHERE x.session_id=s.id))::text FROM sessions s WHERE id='codex:portable'`).Scan(&value))
	return value
}

// Frozen and interleaved traces share an independent view oracle: the original
// legacy curation in shadow, and the latest raw edit in active. IDs, pins, trash,
// revisions and retained bytes are checked through storage/public boundaries.
func TestRawActivationRoundTripModel(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ct := &activationCaseT{T: ht}
		defer ct.close()
		f, _ := supersessionFixture(ct)
		require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
		original := activationLegacySnapshot(ht, f)
		interleave := hegel.Draw(ht, hegel.Booleans())
		steps := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 5)))
		rawName := "legacy title"
		rawTrash := true
		rawPin := true
		active := false
		var previousReceipt string
		require.NoError(ht, f.runtime.QueryRow(`SELECT receipt FROM raw_source_heads`).Scan(&previousReceipt))
		for i, op := range steps {
			switch op {
			case 0, 1:
				activateFixture(ht, ht.Context(), f, "device-a", "")
				active = true
			case 2:
				require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "device-a", "", "shadow"))
				active = false
			case 3:
				if interleave && active {
					rawName = fmt.Sprintf("edited-%d", i)
					require.NoError(ht, f.sink.SetCuration(ht.Context(), "codex:portable", "display_name", rawName))
				}
			case 4:
				if interleave && active {
					rawTrash = hegel.Draw(ht, hegel.Booleans())
					require.NoError(ht, f.sink.SetCuration(ht.Context(), "codex:portable", "trashed", rawTrash))
					rawPin = hegel.Draw(ht, hegel.Booleans())
					require.NoError(ht, f.sink.SetPin(ht.Context(), "codex:portable", 0, rawPin, "latest"))
				}
			case 5:
				if interleave {
					m, receipt := f.accept(ct, "device-a", fmt.Sprintf("interleaved-%d", i), previousReceipt)
					out := projectionOutcome("retained")
					out.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
					require.NoError(ht, f.sink.Project(ht.Context(), f.lease(ct, m), m, out))
					previousReceipt = receipt.Receipt
				}
			}
			var legacyHidden bool
			require.NoError(ht, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&legacyHidden))
			require.Equal(ht, active, legacyHidden)
			if active {
				var name string
				var trash bool
				var pins int
				require.NoError(ht, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL,(SELECT count(*) FROM pinned_messages p WHERE p.session_id=s.id) FROM sessions s WHERE provenance_kind='raw'`).Scan(&name, &trash, &pins))
				require.Equal(ht, rawName, name)
				require.Equal(ht, rawTrash, trash)
				wantPins := 0
				if rawPin {
					wantPins = 1
				}
				require.Equal(ht, wantPins, pins)
				identity, err := f.sink.Resolve(ht.Context(), "codex:portable")
				require.NoError(ht, err)
				require.Equal(ht, RawIdentityUnique, identity.State)
			}
			require.Equal(ht, original, activationLegacySnapshot(ht, f))
		}
		require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "device-a", "", "clear"))
		var hidden bool
		require.NoError(ht, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
		require.False(ht, hidden)
		require.Equal(ht, original, activationLegacySnapshot(ht, f))
		// Clearing the last row restores unscoped raw+legacy service, not supersession.
		require.NoError(ht, ChangeRawRollout(ht.Context(), f.admin, f.schema, f.tenant, "", "", "clear"))
		var count int
		require.NoError(ht, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE rollout_hidden`).Scan(&count))
		require.Zero(ht, count)
	}, hegel.WithTestCases(30), hegel.SuppressHealthCheck(hegel.TooSlow))
}

func TestRawActivationNoScopeNeverSupersedes(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable~"+activationBranch(t, f), "starred", false))
	var n int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE rollout_hidden`).Scan(&n))
	assert.Zero(t, n)
}
func activationBranch(t *testing.T, f projectionFixture) string {
	t.Helper()
	var branch string
	require.NoError(t, f.runtime.QueryRow(`SELECT branch_id FROM raw_session_branches`).Scan(&branch))
	return branch
}

func TestRawActivationDestructiveCurationReleasesMap(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			f, _ := supersessionFixture(t)
			require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
			activateFixture(t, t.Context(), f, "", "")
			legacy := activationLegacySnapshot(t, f)
			if empty {
				n, err := f.sink.EmptyTrash(t.Context())
				require.NoError(t, err)
				assert.Equal(t, 1, n)
			} else {
				removed, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
				require.NoError(t, err)
				assert.True(t, removed)
			}
			var n int
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&n))
			assert.Zero(t, n)
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE id='codex:portable' AND NOT rollout_hidden`).Scan(&n))
			assert.Equal(t, 1, n)
			assert.Equal(t, legacy, activationLegacySnapshot(t, f))
		})
	}
}

func TestRawActivationScopePrecedenceAndSharedSources(t *testing.T) {
	f, _ := supersessionFixture(t)
	m, _ := f.accept(t, "device-b", "shared", "")
	out := projectionOutcome("retained")
	out.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	for _, step := range []struct {
		device, provider, action string
		hidden                   bool
	}{
		{"", "codex", "activate", true},
		{"device-a", "", "shadow", true}, // Other device still inherits provider active.
		{"device-b", "", "shadow", false},
		{"device-a", "codex", "activate", true},
		{"device-a", "codex", "clear", false},
		{"device-a", "", "clear", true},
		{"device-b", "", "clear", true},
		{"", "codex", "clear", false},
		{"", "", "activate", true},
		{"", "", "clear", false}, // No-scope restores both originals and raw.
	} {
		before := f.revisions(t)
		if step.action == "activate" {
			activateFixture(t, t.Context(), f, step.device, step.provider)
		} else {
			require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, step.device, step.provider, step.action))
		}
		var hidden bool
		require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
		assert.Equal(t, step.hidden, hidden, "%+v", step)
		after := f.revisions(t)
		assert.Greater(t, after.identity, before.identity)
		assert.Greater(t, after.corpus, before.corpus)
	}
}

// Hegel embeds testing.T; its inherited Cleanup/TempDir belong to the outer
// test. Keep every generated fixture's resources and helper failures in its case.
type activationCaseT struct {
	*hegel.T
	cleanups []func()
}

func (t *activationCaseT) Cleanup(f func()) { t.cleanups = append(t.cleanups, f) }
func (t *activationCaseT) TempDir() string {
	path, err := os.MkdirTemp("", "activation-case-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(path)) })
	return path
}
func (t *activationCaseT) close() {
	for _, cleanup := range t.cleanups {
		defer cleanup()
	}
}

// Removing the only active device must not let its excluded branch keep a
// shadow peer visible. The same decision must survive scope and source refresh.
func TestRawActivationExcludedActiveBranchCannotServeShadowPeer(t *testing.T) {
	for _, purge := range []bool{false, true} {
		for _, peerActive := range []bool{false, true} {
			t.Run(fmt.Sprintf("purge=%t/active-peer=%t", purge, peerActive), func(t *testing.T) {
				f, _ := supersessionFixture(t)
				peer, peerReceipt := f.accept(t, "device-b", "shared-peer", "")
				shared := projectionOutcome("retained")
				shared.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, peer), peer, shared))
				require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
				activateFixture(t, t.Context(), f, "device-a", "")
				if peerActive {
					activateFixture(t, t.Context(), f, "device-b", "")
				}
				var parent string
				require.NoError(t, f.runtime.QueryRow(`SELECT receipt FROM raw_source_heads WHERE device_id='device-a'`).Scan(&parent))
				next, accepted := f.accept(t, "device-a", "diverge-active", parent)
				changed := projectionOutcome("diverged")
				changed.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, changed))
				// Group service initially exposes both cohorts through A. Keep B out of
				// trash so EmptyTrash exercises removal of A alone, just like the anchored
				// exclusion, rather than legitimately purging both served cohorts.
				var peerAlias string
				require.NoError(t, f.runtime.QueryRow(`SELECT g.base_alias||'~'||b.branch_id FROM raw_session_branches b JOIN raw_session_groups g USING(group_id) JOIN raw_source_projections p USING(source_id) WHERE p.device_id='device-b' AND b.active`).Scan(&peerAlias))
				require.NoError(t, f.sink.SetCuration(t.Context(), peerAlias, "trashed", false))
				var alias string
				require.NoError(t, f.runtime.QueryRow(`SELECT g.base_alias||'~'||b.branch_id FROM raw_session_branches b JOIN raw_session_groups g USING(group_id) JOIN raw_source_projections p USING(source_id) WHERE p.device_id='device-a' AND b.active`).Scan(&alias))
				legacy := activationLegacySnapshot(t, f)
				before := f.revisions(t)
				if purge {
					n, err := f.sink.EmptyTrash(t.Context())
					require.NoError(t, err)
					require.Equal(t, 1, n)
				} else {
					removed, err := f.sink.ExcludeTrashedSession(t.Context(), alias)
					require.NoError(t, err)
					require.True(t, removed)
				}
				wantServed := peerActive
				check := func() {
					var visible, maps int
					var legacyHidden bool
					require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&legacyHidden))
					assert.Equal(t, wantServed, legacyHidden)
					require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE provenance_kind='raw' AND NOT rollout_hidden`).Scan(&visible))
					require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&maps))
					if wantServed {
						assert.Equal(t, 1, visible)
						assert.Equal(t, 1, maps)
					} else {
						assert.Zero(t, visible)
						assert.Zero(t, maps)
					}
					assert.Equal(t, legacy, activationLegacySnapshot(t, f))
				}
				check()
				assert.Greater(t, f.revisions(t).identity, before.identity)
				require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
				check()
				refresh, _ := f.accept(t, "device-a", "excluded-refresh", accepted.Receipt)
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, refresh), refresh, changed))
				check()
				refreshPeer, _ := f.accept(t, "device-b", "shadow-peer-refresh", peerReceipt.Receipt)
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, refreshPeer), refreshPeer, shared))
				check()
			})
		}
	}
}
