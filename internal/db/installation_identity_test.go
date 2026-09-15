package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/export"
)

func TestInstallationAdoptionMovesOwnedArchiveState(t *testing.T) {
	database := testDB(t)
	const identity = "0123456789abcdef0123456789abcdef"
	const owner = "oldhost.example"
	root := filepath.Join(t.TempDir(), "project")
	path := filepath.Join(root, "session.jsonl")
	for _, machine := range []string{owner, "local", "unproven.example", "peer.example"} {
		require.NoError(t, database.UpsertSessionWithProjectIdentity(Session{
			ID: machine, Machine: machine, Project: "project", Agent: "claude", FilePath: &path,
		}, export.ProjectIdentityObservation{
			SessionID: machine, Machine: machine, Project: "project", RootPath: root,
			ObservedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		}, "project"))
	}
	_, err := database.CreateWorktreeProjectMapping(t.Context(), WorktreeProjectMapping{
		Machine: owner, PathPrefix: root, Project: "project", Enabled: true,
	})
	require.NoError(t, err)
	starred, err := database.StarSession(owner)
	require.NoError(t, err)
	require.True(t, starred)
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.Exec(`INSERT INTO local_session_source_baselines VALUES (?, ?, 'claude', ?)`, owner, owner, path)
		return err
	}))
	require.NoError(t, database.SetSyncState("artifact_local_machine_name", owner))
	_, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(t, err)
	for _, machine := range []string{owner, "local", "unproven.example", "peer.example"} {
		session, err := database.GetSession(t.Context(), machine)
		require.NoError(t, err)
		require.NotNil(t, session)
		want := machine
		if machine == owner || machine == "local" {
			want = identity
		}
		assert.Equal(t, want, session.Machine)
	}
	stars, err := database.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{owner}, stars)
	rules, err := database.ListWorktreeProjectMappings(t.Context(), identity)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	observations, err := database.ListProjectIdentityObservations(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, observations, 3, "the two local root observations consolidate")
	for _, observation := range observations {
		assert.NotEqual(t, owner, observation.Machine)
		assert.NotEqual(t, "local", observation.Machine)
	}
	var baselineMachine string
	require.NoError(t, database.Reader().QueryRowContext(t.Context(),
		`SELECT machine FROM local_session_source_baselines WHERE session_id = ?`, owner).Scan(&baselineMachine))
	assert.Equal(t, identity, baselineMachine)
	alias, err := database.GetSyncState("machine_alias:" + owner)
	require.NoError(t, err)
	assert.Equal(t, identity, alias)
	oldAuthority, err := database.GetSyncState("artifact_local_machine_name")
	require.NoError(t, err)
	assert.Empty(t, oldAuthority)

	// A peer using the retired hostname is not newly claimed on later starts.
	require.NoError(t, database.UpsertSession(Session{
		ID: "later-peer", Machine: owner, Project: "project", Agent: "claude",
	}))
	_, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(t, err)
	peer, err := database.GetSession(t.Context(), "later-peer")
	require.NoError(t, err)
	assert.Equal(t, owner, peer.Machine)
	assert.Equal(t, identity, rules[0].Machine)
}

func TestInstallationAdoptionLeavesUnownedHistoryInPlace(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef"
	database := testDB(t)
	require.NoError(t, database.UpsertSession(Session{
		ID: "history", Machine: "oldhost.example", Project: "project", Agent: "claude",
	}))
	unowned, err := database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(t, err)
	assert.Equal(t, []string{"oldhost.example"}, unowned)
	session, err := database.GetSession(t.Context(), "history")
	require.NoError(t, err)
	assert.Equal(t, "oldhost.example", session.Machine)
	// Later starts do not repeat the decision; explicit adoption still works.
	unowned, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(t, err)
	assert.Empty(t, unowned)
	require.NoError(t, database.AdoptMachineIdentity(t.Context(), identity, []string{"oldhost.example"}))
	session, err = database.GetSession(t.Context(), "history")
	require.NoError(t, err)
	assert.Equal(t, identity, session.Machine)
}

func TestInstallationAdoptionRollsBackConflictingRules(t *testing.T) {
	database := testDB(t)
	const identity = "0123456789abcdef0123456789abcdef"
	root := t.TempDir()
	for _, machine := range []string{"oldhost.example", identity} {
		_, err := database.CreateWorktreeProjectMapping(t.Context(), WorktreeProjectMapping{
			Machine: machine, PathPrefix: root, Project: machine, Enabled: true,
		})
		require.NoError(t, err)
	}
	require.NoError(t, database.UpsertSession(Session{
		ID: "history", Machine: "oldhost.example", Project: "project", Agent: "claude",
	}))
	require.ErrorContains(t, database.AdoptMachineIdentity(t.Context(), identity, []string{"oldhost.example"}), "conflicting worktree")
	session, err := database.GetSession(t.Context(), "history")
	require.NoError(t, err)
	assert.Equal(t, "oldhost.example", session.Machine)
	marker, err := database.GetSyncState("artifact_local_installation_id")
	require.NoError(t, err)
	assert.Empty(t, marker)
}

func TestInstallationResetDoesNotClaimPreviousIdentity(t *testing.T) {
	database := testDB(t)
	const first = "ffffffffffffffffffffffffffffffff"
	const second = "00000000000000000000000000000001"
	const former = "alpha.example"
	require.NoError(t, database.UpsertSession(Session{
		ID: "before-reset", Machine: former, Project: "project", Agent: "claude",
	}))
	require.NoError(t, database.AdoptMachineIdentity(t.Context(), first, []string{former}))
	_, err := database.EnsureInstallationIdentity(t.Context(), second)
	require.NoError(t, err)
	session, err := database.GetSession(t.Context(), "before-reset")
	require.NoError(t, err)
	assert.Equal(t, first, session.Machine)
	require.NoError(t, database.AdoptMachineIdentity(t.Context(), second, []string{first, former}))
	session, err = database.GetSession(t.Context(), "before-reset")
	require.NoError(t, err)
	assert.Equal(t, second, session.Machine)
	aliases, err := database.GetMachineAliases(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{first: second, former: second}, aliases)
}

func TestInstallationAdoptionKeepsNewestRootObservation(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef"
	for _, newestMachine := range []string{"oldhost.example", identity} {
		t.Run(newestMachine, func(t *testing.T) {
			database := testDB(t)
			for _, machine := range []string{"oldhost.example", identity} {
				observed := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
				if machine == newestMachine {
					observed = observed.Add(time.Microsecond)
				}
				require.NoError(t, database.UpsertSessionWithProjectIdentity(Session{
					ID: machine, Machine: machine, Project: "project", Agent: "claude",
				}, export.ProjectIdentityObservation{
					SessionID: machine, Machine: machine, Project: "project", RootPath: "/workspace/project",
					GitBranch: machine, ObservedAt: observed,
				}, "project"))
			}
			require.NoError(t, database.AdoptMachineIdentity(t.Context(), identity, []string{"oldhost.example"}))
			observations, err := database.ListProjectIdentityObservations(t.Context(), nil)
			require.NoError(t, err)
			require.Len(t, observations, 1)
			assert.Equal(t, newestMachine, observations[0].GitBranch)
		})
	}
}

func TestInstallationAdoptionKeepsSourceArchiveBoundaries(t *testing.T) {
	database := testDB(t)
	const identity = "0123456789abcdef0123456789abcdef"
	const former = "oldhost.example"
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		for _, archive := range []string{"archive-a", "archive-b"} {
			if _, err := tx.Exec(`INSERT INTO source_archives (source_archive_id, source_archive_salt) VALUES (?, ?)`, archive, archive+"-salt"); err != nil {
				return err
			}
		}
		for _, row := range []struct {
			archive, machine, project, branch, observed string
		}{
			{"archive-a", former, "project-a", "a-new", "2026-07-01T00:00:02Z"},
			{"archive-a", identity, "project-a", "a-old", "2026-07-01T00:00:01Z"},
			{"archive-b", former, "project-b", "b-old", "2026-07-01T00:00:00Z"},
		} {
			if _, err := tx.Exec(`INSERT INTO source_worktree_project_mappings
				(source_archive_id, machine, path_prefix, project) VALUES (?, ?, '/workspace/shared', ?)`,
				row.archive, row.machine, row.project); err != nil {
				return err
			}
			// Observations share all identity keys except archive and machine.
			// The older archive-b row must survive archive-a's newer evidence.
			if _, err := tx.Exec(`INSERT INTO source_project_identity_observations
				(source_archive_id, machine, project, root_path, git_remote, git_branch, observed_at)
				VALUES (?, ?, 'shared', '/workspace/shared', '', ?, ?)`,
				row.archive, row.machine, row.branch, row.observed); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, database.AdoptMachineIdentity(t.Context(), identity, []string{former}))
	type retainedRow struct {
		Archive string `bun:"source_archive_id"`
		Machine string
		Value   string
	}
	var rules, observations []retainedRow
	require.NoError(t, database.view(t.Context(), func(store bun.IDB) error {
		return store.NewRaw(`SELECT source_archive_id, machine, project AS value
			FROM source_worktree_project_mappings ORDER BY source_archive_id`).Scan(t.Context(), &rules)
	}))
	require.NoError(t, database.view(t.Context(), func(store bun.IDB) error {
		return store.NewRaw(`SELECT source_archive_id, machine, git_branch AS value
			FROM source_project_identity_observations ORDER BY source_archive_id`).Scan(t.Context(), &observations)
	}))
	assert.Equal(t, []retainedRow{{"archive-a", identity, "project-a"}, {"archive-b", identity, "project-b"}}, rules)
	assert.Equal(t, []retainedRow{{"archive-a", identity, "a-new"}, {"archive-b", identity, "b-old"}}, observations)
}
