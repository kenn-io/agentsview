package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parsertest/hermesfixture"
)

func hermesTokenFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	createHermesStateDB(t, root)
	path := filepath.Join(root, "state.db")
	c, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	hermesfixture.CreateWatcherSchema(t, c)
	_, err = c.ExecContext(t.Context(), `ALTER TABLE sessions ADD COLUMN last_activity_at REAL;
 ALTER TABLE sessions ADD COLUMN runtime_revision INTEGER DEFAULT 0;`)
	require.NoError(t, err)
	return c, path
}

func tokenMap(t *testing.T, c *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, listHermesMemberTokens(t.Context(), c, "", func(id, token string) error { out[id] = token; return nil }))
	return out
}

func TestHermesMemberTokensMatchDirectAndSeededFingerprint(t *testing.T) {
	c, path := hermesTokenFixture(t)
	hermesfixture.CreateLedger(t, c)
	_, err := c.ExecContext(t.Context(), `INSERT INTO session_model_usage(session_id,model,input_tokens) SELECT id,'model',12 FROM sessions; INSERT INTO sessions(id,source,started_at) VALUES ('cron_job-a_20261007_120000','cron',1778767200)`)
	require.NoError(t, err)
	tokens := tokenMap(t, c)
	ctx, cleanup, err := WithReconciliationCache(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })
	require.NoError(t, seedHermesMemberCoresLocked(ctx, path))
	set := newHermesSourceSet(AgentHermes, []string{filepath.Dir(path)})
	sources, err := set.Discover(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, sources)
	for _, source := range sources {
		src, ok := source.Opaque.(hermesSource)
		if !ok || src.SessionID == "" {
			continue
		}
		src.FingerprintSingleMember = true
		fp, err := hermesStateMemberFingerprint(t.Context(), source, src)
		require.NoError(t, err)
		token, ok := hermesStoredMemberToken(fp.Hash)
		require.True(t, ok)
		assert.Equal(t, tokens[src.SessionID], token)
		src.FingerprintSingleMember = false
		seeded, err := hermesStateMemberFingerprint(ctx, source, src)
		require.NoError(t, err)
		assert.Equal(t, fp.Hash, seeded.Hash)
	}
}

func TestHermesMemberTokensIgnoreUnusedSessionColumns(t *testing.T) {
	c, _ := hermesTokenFixture(t)
	before := tokenMap(t, c)
	_, err := c.ExecContext(t.Context(), `UPDATE sessions SET runtime_revision=8,last_activity_at=1778769999`)
	require.NoError(t, err)
	assert.Equal(t, before, tokenMap(t, c))
}

func TestHermesMemberListingFallbacksAndDeletion(t *testing.T) {
	for _, kind := range []string{"deleted", "malformed", "legacy", "suppressed", "pager error", "trigger removed", "missing index"} {
		t.Run(kind, func(t *testing.T) {
			c, path := hermesTokenFixture(t)
			root := filepath.Dir(path)
			set := newHermesSourceSet(AgentHermes, []string{root})
			// Use one valid member, with an independent transcript fallback.
			_, err := c.ExecContext(t.Context(), `DELETE FROM messages;DELETE FROM sessions; INSERT INTO sessions(id,source,started_at) VALUES ('s','cli',1778767200); INSERT INTO messages(session_id,role,content,timestamp) VALUES ('s','user','original',1778767200)`)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0700))
			writeSourceFile(t, filepath.Join(root, "sessions", "s.jsonl"), hermesProviderJSONLFixture("fallback"))
			source := set.stateMemberSourceRef(root, path, "s")
			fingerprint, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			stored := StoredMemberFreshness{Path: VirtualSourcePath(path, "s"), FingerprintHash: fingerprint.Hash}
			switch kind {
			case "deleted":
				_, err = c.ExecContext(t.Context(), `DELETE FROM messages;DELETE FROM sessions`)
			case "malformed":
				_, err = c.ExecContext(t.Context(), `UPDATE sessions SET started_at='invalid'`)
			case "legacy":
				stored.FingerprintHash = strings.Repeat("a", 64)
			case "suppressed":
				stored.Suppressed = true
				_, err = c.ExecContext(t.Context(), `UPDATE sessions SET title='changed'`)
			case "missing index":
				_, err = c.ExecContext(t.Context(), `DROP INDEX idx_messages_session_id`)
				require.NoError(t, err)
				_, err = set.Fingerprint(t.Context(), source)
			case "trigger removed":
				_, err = c.ExecContext(t.Context(), `DROP TRIGGER messages_display_revision_update`)
			}
			require.NoError(t, err)
			rows := []StoredMemberFreshness{stored}
			if kind == "suppressed" {
				// Mixed authority still skips the matching suppressed member.
				rows = append([]StoredMemberFreshness{{Path: VirtualSourcePath(path, "a")}}, rows...)
			}
			pager := memoryFreshnessPager(rows, 1)
			if kind == "pager error" {
				pager = func(context.Context, string, int) ([]StoredMemberFreshness, bool, error) {
					return nil, false, errors.New("fixture pager failure")
				}
			}
			sources, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: path, AllowWatermarkOnlySources: true, StoredMemberFreshnessPage: pager})
			require.NoError(t, err)
			switch kind {
			case "suppressed":
				assert.Empty(t, sources)
			case "deleted":
				require.Len(t, sources, 1)
				assert.Equal(t, filepath.Join(root, "sessions", "s.jsonl"), sources[0].DisplayPath)
			case "malformed":
				require.Len(t, sources, 2)
			default:
				require.Len(t, sources, 1)
				assert.Equal(t, VirtualSourcePath(path, "s"), sources[0].FingerprintKey)
			}
		})
	}
}

// Enabling an existing root through settings can dispatch a watcher event before
// its first full sync. Without stored authority, use the bulk-seeded full path.
func TestHermesMemberListingWithoutStoredAuthority(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		t.Run(fmt.Sprint(suppressed), func(t *testing.T) {
			conn, path := hermesTokenFixture(t)
			_, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at) VALUES ('new','cli',1778767200); INSERT INTO messages(session_id,role,content,timestamp) VALUES ('new','user','new member',1778767200)`)
			require.NoError(t, err)
			var stored []StoredMemberFreshness
			if suppressed {
				stored = []StoredMemberFreshness{{Path: VirtualSourcePath(path, "child"), Suppressed: true}}
			}
			set := newHermesSourceSet(AgentHermes, []string{filepath.Dir(path)})
			sources, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path: path, AllowWatermarkOnlySources: true,
				StoredMemberFreshnessPage: memoryFreshnessPager(stored, 1),
			})
			require.NoError(t, err)
			require.NotEmpty(t, sources)
			for _, source := range sources {
				assert.False(t, source.Opaque.(hermesSource).FingerprintSingleMember,
					"an unimported root must retain the bulk-seeded fingerprint path")
			}
		})
	}
}

func TestHermesMemberFreshnessContainerRoots(t *testing.T) {
	for _, profile := range []bool{false, true} {
		root := t.TempDir()
		providerRoot := root
		if profile {
			providerRoot = filepath.Join(root, ".hermes", "profiles")
			root = filepath.Join(providerRoot, "work")
		}
		require.NoError(t, os.MkdirAll(root, 0700))
		createHermesStateDB(t, root)
		p, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{providerRoot}})
		require.True(t, ok)
		path := filepath.Join(root, "state.db")
		for _, event := range []string{path, path + "-wal"} {
			container, ok := ResolveStoredMemberFreshnessContainer(p, event)
			assert.True(t, ok)
			assert.Equal(t, path, container)
		}
		_, ok = ResolveStoredMemberFreshnessContainer(p, filepath.Join(root, "sessions", "s.jsonl"))
		assert.False(t, ok)
	}
}

func TestHermesMemberTokenLedgerOnlyWrite(t *testing.T) {
	c, path := hermesTokenFixture(t)
	hermesfixture.CreateLedger(t, c)
	_, err := c.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at) VALUES ('neighbour','cli',1778767200);
 INSERT INTO messages(session_id,role,content,timestamp) VALUES ('neighbour','user','untouched',1778767200);
 CREATE UNIQUE INDEX ledger_key ON session_model_usage(session_id,model,billing_provider,billing_base_url,billing_mode,task);
 INSERT INTO session_model_usage(session_id,model,input_tokens,task) SELECT id,'model',12,'aux' FROM sessions`)
	require.NoError(t, err)
	before := tokenMap(t, c)
	sessionBefore, err := scanHermesStateSession(c.QueryRowContext(t.Context(), hermesStateSessionQuery, "child"))
	require.NoError(t, err)
	// Native auxiliary UPSERT touches only this same ledger key. Neither session
	// metadata/revision nor message history changes; the neighbour stays untouched.
	_, err = c.ExecContext(t.Context(), `INSERT INTO session_model_usage(session_id,model,input_tokens,task) VALUES ('child','model',87,'aux')
 ON CONFLICT(session_id,model,billing_provider,billing_base_url,billing_mode,task) DO UPDATE SET input_tokens=input_tokens+excluded.input_tokens`)
	require.NoError(t, err)
	after := tokenMap(t, c)
	assert.NotEqual(t, before["child"], after["child"])
	assert.Equal(t, before["neighbour"], after["neighbour"])
	// Fingerprints constructed from the pre-write tokens are the existing cursor
	// contract; the deep component is irrelevant to an unchanged token comparison.
	var stored []StoredMemberFreshness
	for id, token := range before {
		stored = append(stored, StoredMemberFreshness{Path: VirtualSourcePath(path, id), FingerprintHash: "hst1:" + token + ":" + strings.Repeat("a", 64)})
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].Path < stored[j].Path })
	set := newHermesSourceSet(AgentHermes, []string{filepath.Dir(path)})
	sources, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: path, AllowWatermarkOnlySources: true, StoredMemberFreshnessPage: memoryFreshnessPager(stored, 1)})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, VirtualSourcePath(path, "child"), sources[0].FingerprintKey)
	sessionAfter, err := scanHermesStateSession(c.QueryRowContext(t.Context(), hermesStateSessionQuery, "child"))
	require.NoError(t, err)
	assert.Equal(t, sessionBefore, sessionAfter, "all imported session metadata stays unchanged")
	var revision int
	require.NoError(t, c.QueryRowContext(t.Context(), `SELECT transcript_revision FROM sessions WHERE id='child'`).Scan(&revision))
	assert.Zero(t, revision)
}

func TestHermesMemberTokenNativeDeletes(t *testing.T) {
	c, _ := hermesTokenFixture(t)
	// Newer inactive rows surround the live row's timestamp. Deleting the live
	// interior row preserves greatest ID and both endpoints; native live count moves.
	_, err := c.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at) VALUES ('neighbour','cli',1778767200);
 INSERT INTO messages(session_id,role,content,timestamp) VALUES ('child','user','inactive',1778767200),('child','user','inactive',1778767300)`)
	require.NoError(t, err)
	for _, stmt := range []string{
		`DELETE FROM messages WHERE id=1; UPDATE sessions SET message_count=0 WHERE id='child'`,
		`DELETE FROM messages WHERE session_id='child'; UPDATE sessions SET message_count=0 WHERE id='child'`,
	} {
		before := tokenMap(t, c)
		_, err = c.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
		after := tokenMap(t, c)
		assert.NotEqual(t, before["child"], after["child"])
		assert.Equal(t, before["neighbour"], after["neighbour"])
	}
}

// Growing unchanged history must retain three covering endpoint searches, not
// materialize message aggregates. The owner and ledger cardinalities stay fixed.
func TestHermesMemberTokenHistoryGrowth(t *testing.T) {
	c, _ := hermesTokenFixture(t)
	for _, n := range []int{20, 20000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			_, err := c.ExecContext(t.Context(), `DELETE FROM messages;
WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO messages(session_id,role,content,timestamp) SELECT 'child','user','history',1778767200+x FROM n`, n)
			require.NoError(t, err)
			rows, err := c.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+hermesMemberTokenQuery(true, false))
			require.NoError(t, err)
			searches, endpoints := 0, 0
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				if strings.Contains(detail, "SEARCH messages USING COVERING INDEX idx_messages_session_id (session_id=?)") {
					searches++
				}
				if strings.Contains(detail, "SEARCH messages USING COVERING INDEX idx_messages_session (session_id=?)") {
					endpoints++
				}
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Equal(t, 1, searches, "greatest ID must use its covering seek")
			assert.Equal(t, 2, endpoints, "both timestamp endpoints must use covering seeks")
		})
	}
}

func TestHermesMemberSchemaGate(t *testing.T) {
	for _, setup := range []string{
		`UPDATE schema_version SET version=33`, `UPDATE schema_version SET version=34`,
		`UPDATE schema_version SET version=NULL`, `UPDATE schema_version SET version='bad'`,
		`DELETE FROM schema_version`, `DROP TABLE schema_version`,
	} {
		t.Run(setup, func(t *testing.T) {
			c, _ := hermesTokenFixture(t)
			_, err := c.ExecContext(t.Context(), setup)
			require.NoError(t, err)
			ok, _ := hermesStoreTracksRewrites(t.Context(), c)
			assert.Equal(t, strings.HasSuffix(setup, "=33"), ok)
		})
	}
}
