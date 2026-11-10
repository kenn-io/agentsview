package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestScopedDataVersionProviders(t *testing.T) {
	t.Cleanup(InstallDataVersionScopesForTest(map[int][]parser.AgentType{
		131: {parser.AgentHermes},
		132: {parser.AgentAugureDesktop},
	}))
	assert.Nil(t, ScopedDataVersionProviders(129, 132))
	assert.ElementsMatch(t,
		[]parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop},
		ScopedDataVersionProviders(130, 132),
	)
}

func TestAntigravityCLIPromptEnvelopeUpgradeScopes(t *testing.T) {
	assert.Equal(t, []parser.AgentType{parser.AgentAntigravityCLI},
		ScopedDataVersionProviders(158, 160))
}

// Version 134 reshapes Codex session IDs, so it must rebuild every provider.
func TestCodexLegacyPageVersionRebuildsEveryProvider(t *testing.T) {
	assert.Nil(t, ScopedDataVersionProviders(133, 134))
}

func TestVersionOnlyUpgradeScope(t *testing.T) {
	assert.NotNil(t, ScopedDataVersionProviders(169, 170))
	assert.Empty(t, ScopedDataVersionProviders(169, 170))
	assert.Equal(t, []parser.AgentType{parser.AgentDeepSeekHarness}, ScopedDataVersionProviders(168, 170))
	assert.Equal(t, 164, AgentDataVersion(parser.AgentCodex))
	assert.NoError(t, validateDataVersionScopes(map[int][]parser.AgentType{170: {}}))
}

func TestVersionOnlyReadOnlyArchiveNeedsResync(t *testing.T) {
	for _, tt := range []struct {
		name        string
		version     int
		pending     bool
		archiveOnly bool
		stale       bool
	}{
		{"version only", 169, false, false, false},
		{"earlier parser change", 168, false, false, true},
		{"pending parser change", 169, true, false, true},
		{"archive only", 169, false, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if tt.pending {
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO stats(key,value) VALUES (?,168)`, scopedUpgradePendingKey); err != nil {
						return err
					}
				}
				return writeUserVersion(t.Context(), tx, tt.version)
			}))
			if tt.archiveOnly {
				require.NoError(t, d.EnableArchiveOnly(t.Context()))
			}
			path := d.Path()
			require.NoError(t, d.Close())
			readOnly, err := OpenReadOnly(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, readOnly.Close()) })
			assert.Equal(t, tt.stale, readOnly.NeedsResync())
			needs, full, err := ArchiveResyncState(t.Context(), path)
			require.NoError(t, err)
			assert.Equal(t, tt.stale, needs)
			assert.Equal(t, tt.archiveOnly, full)
		})
	}
}

func TestVersionOnlyUpgradePreservesParserState(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"current", "retry"} {
		insertSession(t, d, id, "project", func(s *Session) { s.Agent = "codex" })
	}
	require.NoError(t, d.SetSessionDataVersion(t.Context(), "current", 169))
	require.NoError(t, d.SetSessionDataVersion(t.Context(), "retry", 168))
	require.NoError(t, d.ReplaceSkippedFiles(t.Context(), map[string]int64{"source?agent=codex": 1}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		return writeUserVersion(t.Context(), tx, 169)
	}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		return advanceDataVersion(t.Context(), tx, 170)
	}))
	assert.Equal(t, 170, d.GetSessionDataVersion(t.Context(), "current"))
	assert.Equal(t, 168, d.GetSessionDataVersion(t.Context(), "retry"))
	skips, err := d.LoadSkippedFiles(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"source?agent=codex": 1}, skips)
	var pending int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT count(*) FROM stats WHERE key=?`, scopedUpgradePendingKey).Scan(&pending))
	assert.Zero(t, pending)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO stats(key,value) VALUES (?,168)`, scopedUpgradePendingKey); err != nil {
			return err
		}
		return writeUserVersion(t.Context(), tx, 169)
	}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		return advanceDataVersion(t.Context(), tx, 170)
	}))
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT value FROM stats WHERE key=?`, scopedUpgradePendingKey).Scan(&pending))
	assert.Equal(t, 168, pending)
}

func TestDialogueUpgradeReparsesClassifiedProviders(t *testing.T) {
	// The version before preserving classification, even after later upgrades.
	const completedVersion = 145
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "completed Codex upgrade")
	for _, agent := range []string{"codex", "pi", "hermes"} {
		insertSession(t, d, agent, "project", func(s *Session) { s.Agent = agent })
		require.NoError(t, d.SetSessionDataVersion(t.Context(), agent, completedVersion))
	}
	// Keep the current schema and model the completed parser upgrade on disk.
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		return writeUserVersion(t.Context(), tx, completedVersion)
	}))
	require.NoError(t, d.Close())
	reopened, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	assert.False(t, reopened.NeedsResync())
	assert.Equal(t, CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), "hermes"))
	assert.Equal(t, completedVersion, reopened.GetSessionDataVersion(t.Context(), "codex"))
	assert.Equal(t, completedVersion, reopened.GetSessionDataVersion(t.Context(), "pi"))
}

func TestValidateDataVersionScopes(t *testing.T) {
	require.NoError(t, validateDataVersionScopes(dataVersionScopes),
		"declared parser upgrades must stay within the scoped-upgrade rules")
	tests := []struct {
		name    string
		scopes  map[int][]parser.AgentType
		wantErr string
	}{
		{
			name:    "released version",
			scopes:  map[int][]parser.AgentType{130: {parser.AgentHermes}},
			wantErr: "declare only versions",
		},
		{
			name: "future version",
			scopes: map[int][]parser.AgentType{
				dataVersion + 1: {parser.AgentHermes},
			},
			wantErr: "declare only versions",
		},
		{
			name:    "stored alias",
			scopes:  map[int][]parser.AgentType{dataVersion: {parser.AgentFreebuff}},
			wantErr: `"freebuff" is not a registered provider`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorContains(t, validateDataVersionScopes(tt.scopes), tt.wantErr)
		})
	}
}

func TestAdvanceScopedDataVersion(t *testing.T) {
	t.Cleanup(InstallDataVersionScopesForTest(map[int][]parser.AgentType{131: {parser.AgentCodebuff}}))
	for _, rollback := range []bool{false, true} {
		t.Run(strconv.FormatBool(rollback), func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			seedArtifactOrigin(t, d)
			versions := map[string]int{"codebuff": 130, "freebuff": 130, "claude": 130, "retry": 129}
			for id, version := range versions {
				insertSession(t, d, id, "example", func(s *Session) {
					s.Agent = id
					if id == "retry" {
						s.Agent = "claude"
					}
				})
				require.NoError(t, d.SetSessionDataVersion(ctx, id, version))
			}
			_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET created_at='2000-01-01T00:00:00Z', local_modified_at='2000-01-01T00:00:00Z'`)
			require.NoError(t, err)
			skips := map[string]int64{"legacy": 1, "p?agent=codebuff": 1, "p?agent=freebuff": 1, "p?agent=claude": 1}
			require.NoError(t, d.ReplaceSkippedFiles(ctx, skips))
			require.NoError(t, d.ReplaceSourceFailures(ctx, map[string]SourceFailure{"p?agent=freebuff": {MTimeNS: 1}, "p?agent=claude": {MTimeNS: 1}}))
			require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error { return writeUserVersion(ctx, tx, 130) }))
			clearArtifactExportQueue(t, d)
			err = d.Update(ctx, func(tx *sql.Tx) error {
				if err := advanceDataVersion(ctx, tx, 131); err != nil {
					return err
				}
				if rollback {
					return context.Canceled
				}
				return nil
			})
			if rollback {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
				versions["claude"] = 131
			}
			for id, version := range versions {
				assert.Equal(t, version, d.GetSessionDataVersion(ctx, id))
			}
			got, err := d.LoadSkippedFiles(ctx)
			require.NoError(t, err)
			failures, err := d.LoadSourceFailures(ctx)
			require.NoError(t, err)
			if rollback {
				assert.Equal(t, skips, got)
			} else {
				var pending int
				require.NoError(t, d.getReader().QueryRow(ctx, "SELECT value FROM stats WHERE key='scoped_upgrade_pending'").Scan(&pending))
				assert.Equal(t, 131, pending)
				assert.Equal(t, map[string]int64{"p?agent=claude": 1}, got)
				assert.Len(t, failures, 1)
				assert.Equal(t, []string{"claude"}, artifactExportQueueIDs(t, d))
				candidates, err := d.ListSessionsForMirrorWindow(ctx, "2001-01-01T00:00:00Z", nil, nil)
				require.NoError(t, err)
				assert.Empty(t, candidates, "parser bookkeeping leaves unaffected mirror candidates unchanged")
			}
		})
	}
}
