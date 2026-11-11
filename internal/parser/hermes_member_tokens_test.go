package parser

import (
	"context"
	"database/sql"
	"errors"
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
	require.NoError(t, listHermesMemberTokens(t.Context(), c, "", func(id, token string, _ bool) error { out[id] = token; return nil }))
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
	checked := 0
	for _, source := range sources {
		src, ok := source.Opaque.(hermesSource)
		if !ok || src.SessionID == "" {
			continue
		}
		checked++
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
	assert.Equal(t, len(tokens), checked)
}

func TestHermesMemberTokenChanges(t *testing.T) {
	for _, tc := range []struct {
		name, setup, statement string
		changed                bool
	}{
		{"insert", "", `INSERT INTO messages(session_id,role,content,timestamp) VALUES ('child','assistant','reply',1778767220)`, true},
		{"title", "", `UPDATE sessions SET title='new title' WHERE id='child'`, true},
		{"content", "", `UPDATE messages SET content='rewritten' WHERE session_id='child'`, true},
		{"usage", "", `UPDATE sessions SET input_tokens=777 WHERE id='child'`, true},
		{"display", "", `UPDATE messages SET codex_reasoning_items='[]' WHERE session_id='child'`, true},
		{"timestamp repair", `UPDATE messages SET timestamp=-5 WHERE session_id='child'`, `UPDATE messages SET timestamp=1778767210 WHERE session_id='child'`, true},
		{"live count after interior delete", `INSERT INTO messages(session_id,role,content,timestamp) VALUES ('child','user','inactive',1778767200),('child','user','inactive',1778767300)`, `DELETE FROM messages WHERE id=1; UPDATE sessions SET message_count=0 WHERE id='child'`, true},
		{"delete all endpoints", `UPDATE sessions SET message_count=0 WHERE id='child'`, `DELETE FROM messages WHERE session_id='child'`, true},
		{"unused session fields", "", `UPDATE sessions SET runtime_revision=8,last_activity_at=1778769999 WHERE id='child'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := hermesTokenFixture(t)
			_, err := c.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at) VALUES ('neighbour','cli',1778767200)`)
			require.NoError(t, err)
			if tc.setup != "" {
				_, err = c.ExecContext(t.Context(), tc.setup)
				require.NoError(t, err)
			}
			before := tokenMap(t, c)
			_, err = c.ExecContext(t.Context(), tc.statement)
			require.NoError(t, err)
			after := tokenMap(t, c)
			if tc.changed {
				assert.NotEqual(t, before["child"], after["child"])
			} else {
				assert.Equal(t, before["child"], after["child"])
			}
			assert.Equal(t, before["neighbour"], after["neighbour"])
		})
	}
}

func TestHermesMemberListingFallbacksAndDeletion(t *testing.T) {
	for _, kind := range []string{"deleted", "malformed", "legacy", "suppressed", "pager error", "trigger removed", "empty authority", "suppressed authority"} {
		t.Run(kind, func(t *testing.T) {
			c, path := hermesTokenFixture(t)
			root := filepath.Dir(path)
			set := newHermesSourceSet(AgentHermes, []string{root})
			// Use one valid member, with an independent transcript fallback.
			_, err := c.ExecContext(t.Context(), `DELETE FROM messages;DELETE FROM sessions; INSERT INTO sessions(id,source,started_at) VALUES ('s','cli',1778767200); INSERT INTO messages(session_id,role,content,timestamp) VALUES ('s','user','original',1778767200)`)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0o700))
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
			case "trigger removed":
				_, err = c.ExecContext(t.Context(), `DROP TRIGGER messages_display_revision_update`)
			}
			require.NoError(t, err)
			rows := []StoredMemberFreshness{stored}
			switch kind {
			case "empty authority":
				rows = nil
			case "suppressed authority":
				rows[0].Suppressed = true
			}
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
				assert.Equal(t, kind == "legacy", sources[0].Opaque.(hermesSource).FingerprintSingleMember)
			}
		})
	}
}

func TestHermesMemberFreshnessContainerRoots(t *testing.T) {
	root := t.TempDir()
	providerRoot := filepath.Join(root, ".hermes", "profiles")
	root = filepath.Join(providerRoot, "work")
	require.NoError(t, os.MkdirAll(root, 0o700))
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

func TestHermesMemberTokenLedgerOnlyWrite(t *testing.T) {
	c, path := hermesTokenFixture(t)
	hermesfixture.CreateLedger(t, c)
	_, err := c.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at) VALUES ('neighbour','cli',1778767200);
 INSERT INTO messages(session_id,role,content,timestamp) VALUES ('neighbour','user','untouched',1778767200);
 CREATE UNIQUE INDEX ledger_key ON session_model_usage(session_id,model,billing_provider,billing_base_url,billing_mode,task);
 INSERT INTO session_model_usage(session_id,model,input_tokens,task) SELECT id,'model',12,'aux' FROM sessions`)
	require.NoError(t, err)
	before := tokenMap(t, c)
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
}

func TestHermesMemberTokenHistoryGrowth(t *testing.T) {
	c, _ := hermesTokenFixture(t)
	rows, err := c.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+hermesMemberTokenQuery(true, false))
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
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
	assert.Equal(t, 1, searches, "greatest ID must use its covering seek")
	assert.Equal(t, 2, endpoints, "both timestamp endpoints must use covering seeks")
}

func TestHermesMemberSchemaGate(t *testing.T) {
	for _, setup := range []string{
		`UPDATE schema_version SET version=33`, `UPDATE schema_version SET version=34`,
		`UPDATE schema_version SET version=NULL`, `UPDATE schema_version SET version='bad'`,
		`DELETE FROM schema_version`, `DROP TABLE schema_version`,
		`DROP INDEX idx_messages_session_id`, `DROP INDEX idx_messages_session`,
		`DROP TRIGGER messages_display_revision_update`,
		`DROP TRIGGER messages_display_revision_update; DROP TRIGGER messages_transcript_revision_update; ALTER TABLE sessions DROP COLUMN transcript_revision`,
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

// A changed ancestor can change a cron child's job without changing its token.
func TestHermesWatcherSelectsCronChildAfterAncestorChanges(t *testing.T) {
	c, path := hermesTokenFixture(t)
	_, err := c.ExecContext(t.Context(), `UPDATE sessions SET source='cron', parent_session_id='parent' WHERE id='child';
 INSERT INTO sessions(id,source,parent_session_id,started_at) VALUES ('parent','cron','middle',1)`)
	require.NoError(t, err)
	set := newHermesSourceSet(AgentHermes, []string{filepath.Dir(path)})
	child := set.stateMemberSourceRef(filepath.Dir(path), path, "child")
	fp, err := set.Fingerprint(t.Context(), child)
	require.NoError(t, err)
	stored := []StoredMemberFreshness{{Path: VirtualSourcePath(path, "child"), FingerprintHash: fp.Hash}}
	for _, stmt := range []string{
		`INSERT INTO sessions(id,source,parent_session_id,started_at) VALUES ('middle','cron','cron_job-a_20261009_120000',1)`,
		`UPDATE sessions SET parent_session_id='cron_job-b_20261009_120000' WHERE id='middle'`,
	} {
		_, err = c.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
		sources, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: path, AllowWatermarkOnlySources: true, StoredMemberFreshnessPage: memoryFreshnessPager(stored, 1)})
		require.NoError(t, err)
		found := false
		for _, source := range sources {
			if source.FingerprintKey == child.FingerprintKey {
				found = true
			}
		}
		assert.True(t, found, "cron child must be checked after ancestor changes")
		next, err := set.Fingerprint(t.Context(), child)
		require.NoError(t, err)
		assert.NotEqual(t, fp.Hash, next.Hash)
		fp = next
		stored[0].FingerprintHash = fp.Hash
	}
}
