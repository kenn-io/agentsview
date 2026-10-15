package db

import (
	"context"
	"database/sql"
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

// Version 134 reshapes Codex session IDs, so it must rebuild every provider.
func TestCodexLegacyPageVersionRebuildsEveryProvider(t *testing.T) {
	assert.Nil(t, ScopedDataVersionProviders(133, 134))
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
			name:    "empty declaration",
			scopes:  map[int][]parser.AgentType{dataVersion: {}},
			wantErr: "lists no providers",
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
