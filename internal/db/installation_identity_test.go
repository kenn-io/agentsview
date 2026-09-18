package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestInstallationAdoptionMovesOwnedArchiveState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	const identity = "0123456789abcdef0123456789abcdef"
	const owner = "oldhost.example"
	root := filepath.Join(t.TempDir(), "project")
	path := filepath.Join(root, "session.jsonl")
	for _, machine := range []string{owner, "local", "unproven.example", "peer.example"} {
		require.NoError(database.UpsertSessionWithProjectIdentity(Session{
			ID: machine, Machine: machine, Project: "project", Agent: "claude", FilePath: &path,
		}, export.ProjectIdentityObservation{
			SessionID: machine, Machine: machine, Project: "project", RootPath: root,
			ObservedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		}, "project"))
	}
	_, err := database.CreateWorktreeProjectMapping(t.Context(), WorktreeProjectMapping{
		Machine: owner, PathPrefix: root, Project: "project", Enabled: true,
	})
	require.NoError(err)
	starred, err := database.StarSession(owner)
	require.NoError(err)
	require.True(starred)
	require.NoError(database.Update(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO local_session_source_baselines VALUES (?, ?, 'claude', ?)`, owner, owner, path)
		return err
	}))
	require.NoError(database.SetSyncState("artifact_local_machine_name", owner))
	_, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(err)
	for _, machine := range []string{owner, "local", "unproven.example", "peer.example"} {
		session, err := database.GetSession(t.Context(), machine)
		require.NoError(err)
		require.NotNil(session)
		want := machine
		if machine == owner || machine == "local" {
			want = identity
		}
		assert.Equal(want, session.Machine)
	}
	stars, err := database.ListStarredSessionIDs(t.Context())
	require.NoError(err)
	assert.Equal([]string{owner}, stars)
	rules, err := database.ListWorktreeProjectMappings(t.Context(), identity)
	require.NoError(err)
	require.Len(rules, 1)
	observations, err := database.ListProjectIdentityObservations(t.Context(), nil)
	require.NoError(err)
	require.Len(observations, 3, "the two local root observations consolidate")
	for _, observation := range observations {
		assert.NotEqual(owner, observation.Machine)
		assert.NotEqual("local", observation.Machine)
	}
	var baselineMachine string
	require.NoError(database.Reader().QueryRowContext(t.Context(),
		`SELECT machine FROM local_session_source_baselines WHERE session_id = ?`, owner).Scan(&baselineMachine))
	assert.Equal(identity, baselineMachine)
	alias, err := database.GetSyncState("machine_alias:" + owner)
	require.NoError(err)
	assert.Equal(identity, alias)
	oldAuthority, err := database.GetSyncState("artifact_local_machine_name")
	require.NoError(err)
	assert.Empty(oldAuthority)

	// A peer using the retired hostname is not newly claimed on later starts.
	require.NoError(database.UpsertSession(Session{
		ID: "later-peer", Machine: owner, Project: "project", Agent: "claude",
	}))
	_, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(err)
	peer, err := database.GetSession(t.Context(), "later-peer")
	require.NoError(err)
	assert.Equal(owner, peer.Machine)
	assert.Equal(identity, rules[0].Machine)
}

func TestInstallationAdoptionLeavesUnownedHistoryInPlace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const identity = "0123456789abcdef0123456789abcdef"
	database := testDB(t)
	require.NoError(database.UpsertSession(Session{
		ID: "history", Machine: "oldhost.example", Project: "project", Agent: "claude",
	}))
	unowned, err := database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(err)
	assert.Equal([]string{"oldhost.example"}, unowned)
	session, err := database.GetSession(t.Context(), "history")
	require.NoError(err)
	assert.Equal("oldhost.example", session.Machine)
	// Later starts do not repeat the decision; explicit adoption still works.
	unowned, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(err)
	assert.Empty(unowned)
	require.NoError(database.AdoptMachineIdentity(t.Context(), identity, []string{"oldhost.example"}))
	session, err = database.GetSession(t.Context(), "history")
	require.NoError(err)
	assert.Equal(identity, session.Machine)
}

func TestInstallationAdoptionRollsBackConflictingRules(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	const identity = "0123456789abcdef0123456789abcdef"
	root := t.TempDir()
	for _, machine := range []string{"oldhost.example", identity} {
		_, err := database.CreateWorktreeProjectMapping(t.Context(), WorktreeProjectMapping{
			Machine: machine, PathPrefix: root, Project: machine, Enabled: true,
		})
		require.NoError(err)
	}
	require.NoError(database.UpsertSession(Session{
		ID: "history", Machine: "oldhost.example", Project: "project", Agent: "claude",
	}))
	require.ErrorContains(database.AdoptMachineIdentity(t.Context(), identity, []string{"oldhost.example"}), "conflicting worktree")
	session, err := database.GetSession(t.Context(), "history")
	require.NoError(err)
	assert.Equal("oldhost.example", session.Machine)
	marker, err := database.GetSyncState("artifact_local_installation_id")
	require.NoError(err)
	assert.Empty(marker)
}

func TestInstallationResetDoesNotClaimPreviousIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	const first = "ffffffffffffffffffffffffffffffff"
	const second = "00000000000000000000000000000001"
	const former = "alpha.example"
	require.NoError(database.UpsertSession(Session{
		ID: "before-reset", Machine: former, Project: "project", Agent: "claude",
	}))
	require.NoError(database.AdoptMachineIdentity(t.Context(), first, []string{former}))
	_, err := database.EnsureInstallationIdentity(t.Context(), second)
	require.NoError(err)
	session, err := database.GetSession(t.Context(), "before-reset")
	require.NoError(err)
	assert.Equal(first, session.Machine)
	require.NoError(database.AdoptMachineIdentity(t.Context(), second, []string{first, former}))
	session, err = database.GetSession(t.Context(), "before-reset")
	require.NoError(err)
	assert.Equal(second, session.Machine)
	aliases, err := database.GetMachineAliases(t.Context())
	require.NoError(err)
	assert.Equal(map[string]string{first: second, former: second}, aliases)
}

func TestInstallationAdoptionKeepsNewestRootObservation(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef"
	for _, newestMachine := range []string{"oldhost.example", identity} {
		t.Run(newestMachine, func(t *testing.T) {
			require := require.New(t)

			database := testDB(t)
			for _, machine := range []string{"oldhost.example", identity} {
				observed := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
				if machine == newestMachine {
					observed = observed.Add(time.Nanosecond)
				}
				require.NoError(database.UpsertSessionWithProjectIdentity(Session{
					ID: machine, Machine: machine, Project: "project", Agent: "claude",
				}, export.ProjectIdentityObservation{
					SessionID: machine, Machine: machine, Project: "project", RootPath: "/workspace/project",
					GitBranch: machine, ObservedAt: observed,
				}, "project"))
			}
			require.NoError(database.AdoptMachineIdentity(t.Context(), identity, []string{"oldhost.example"}))
			observations, err := database.ListProjectIdentityObservations(t.Context(), nil)
			require.NoError(err)
			require.Len(observations, 1)
			assert.Equal(t, newestMachine, observations[0].GitBranch)
		})
	}
}
