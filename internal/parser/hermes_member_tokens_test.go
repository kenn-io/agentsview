package parser

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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
	_, err = c.ExecContext(t.Context(), `ALTER TABLE sessions ADD COLUMN transcript_revision INTEGER DEFAULT 0;
 ALTER TABLE sessions ADD COLUMN last_activity_at REAL;
 ALTER TABLE sessions ADD COLUMN runtime_revision INTEGER DEFAULT 0;
 CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id,timestamp);
 CREATE TRIGGER messages_transcript_revision_update AFTER UPDATE OF role,content,api_content,tool_calls,tool_call_id,tool_name,reasoning,reasoning_content,reasoning_details ON messages BEGIN UPDATE sessions SET transcript_revision=transcript_revision+1 WHERE id=new.session_id; END;
 CREATE TRIGGER messages_display_revision_update AFTER UPDATE OF codex_reasoning_items,codex_message_items,display_kind,display_metadata ON messages BEGIN UPDATE sessions SET transcript_revision=transcript_revision+1 WHERE id=new.session_id; END;`)
	require.NoError(t, err)
	return c, path
}

func tokenMap(t *testing.T, c *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, listHermesMemberTokens(t.Context(), c, "", func(id, token string) error { out[id] = token; return nil }))
	return out
}

// The native repair predicate must move the token even for an interior timestamp.
func TestHermesMemberTokenNativeTimestampRepair(t *testing.T) {
	c, _ := hermesTokenFixture(t)
	_, err := c.ExecContext(t.Context(), `DELETE FROM messages; DELETE FROM sessions;
 INSERT INTO sessions(id,source,started_at) VALUES ('s','cli',1778767200);
 INSERT INTO messages(session_id,role,content,timestamp) VALUES ('s','user','first',1778767200),('s','user','middle',-5),('s','user','last',1778767300);`)
	require.NoError(t, err)
	before := tokenMap(t, c)["s"]
	_, err = c.ExecContext(t.Context(), `UPDATE messages SET timestamp=1778767200 WHERE NOT(timestamp BETWEEN 0.0 AND 4200000000.0)`)
	require.NoError(t, err)
	assert.NotEqual(t, before, tokenMap(t, c)["s"])
	var revision int
	require.NoError(t, c.QueryRowContext(t.Context(), `SELECT transcript_revision FROM sessions WHERE id='s'`).Scan(&revision))
	assert.Zero(t, revision)
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
	_, err = c.ExecContext(t.Context(), `UPDATE messages SET content='rewrite'`)
	require.NoError(t, err)
	assert.NotEqual(t, before, tokenMap(t, c))
}

func TestHermesRewriteTriggerCoverage(t *testing.T) {
	c, _ := hermesTokenFixture(t)
	ok, err := hermesStoreTracksRewrites(t.Context(), c)
	require.NoError(t, err)
	assert.True(t, ok)
	_, err = c.ExecContext(t.Context(), `DROP TRIGGER messages_display_revision_update`)
	require.NoError(t, err)
	ok, err = hermesStoreTracksRewrites(t.Context(), c)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestHermesStoredTokenRejectsLegacyHash(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 64), "hst1:bad:hash"} {
		_, ok := hermesStoredMemberToken(value)
		assert.False(t, ok)
	}
}

func TestHermesMemberListingFallbacksAndDeletion(t *testing.T) {
	for _, kind := range []string{"deleted", "malformed", "legacy", "suppressed", "pager error", "trigger removed"} {
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
			case "trigger removed":
				_, err = c.ExecContext(t.Context(), `DROP TRIGGER messages_display_revision_update`)
			}
			require.NoError(t, err)
			pager := memoryFreshnessPager([]StoredMemberFreshness{stored}, 1)
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
	c, _ := hermesTokenFixture(t)
	hermesfixture.CreateLedger(t, c)
	_, err := c.ExecContext(t.Context(), `INSERT INTO session_model_usage(session_id,model,input_tokens) SELECT id,'model',12 FROM sessions`)
	require.NoError(t, err)
	before := tokenMap(t, c)
	_, err = c.ExecContext(t.Context(), `UPDATE session_model_usage SET input_tokens=99`)
	require.NoError(t, err)
	after := tokenMap(t, c)
	require.NotEmpty(t, before)
	for id, token := range before {
		assert.NotEqual(t, token, after[id])
	}
}
