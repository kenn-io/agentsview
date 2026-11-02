//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"hegel.dev/go/hegel"
)

func TestHostedRolloutIdentityEligibility(t *testing.T) {
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, device, "")
		p := projectionOutcome(device)
		if device == "device-b" {
			p.Outcome.Results[0].Result.Session.SourceSessionID = "competitor"
		}
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, p))
	}
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count) VALUES('codex:portable','legacy','m','codex',1)`)
	require.NoError(t, err)
	var visible string
	require.NoError(t, f.runtime.QueryRow(`SELECT b.session_id FROM raw_session_branches b JOIN raw_source_projections s ON s.source_id=b.source_id WHERE s.device_id='device-a'`).Scan(&visible))
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=(id<>$1)`, visible)
	require.NoError(t, err)
	raw, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, raw.State)
	assert.Equal(t, visible, raw.SessionID)
	session, err := h.GetSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "codex:portable", session.ID)
	ids, err := h.resolveBatch(t.Context(), []string{"codex:portable"}, false, true)
	require.NoError(t, err)
	assert.Equal(t, []string{visible}, ids)
	_, err = h.StarSession(t.Context(), "codex:portable")
	require.NoError(t, err, "hidden legacy collision must not block raw curation")
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=true`)
	require.NoError(t, err)
	session, err = h.GetSessionFull(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Nil(t, session)
	msgs, err := h.GetAllMessages(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Empty(t, msgs)
	_, err = h.StarSession(t.Context(), "codex:portable")
	assert.Error(t, err)
}

func TestHostedRolloutPurgeProtectsExpansion(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, edge := range []string{"alias", "reverse", "codex"} {
			t.Run(fmt.Sprintf("%s/empty=%t", edge, empty), func(t *testing.T) {
				f := newHostedFixture(t, "tenant-purge")
				h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, h.Close()) })
				seed, protected := "visible", "protected"
				if edge == "codex" {
					seed = "codex:thread"
					protected = seed + "_00000000-0000-0000-0000-000000000001"
				}
				_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,owner_marker,message_count,deleted_at,rollout_hidden,trash_includes_codex_pages) VALUES($1,'p','m','codex','owner',1,NOW(),false,$3),($2,'p','m','codex','owner',1,NOW(),true,false)`, seed, protected, edge == "codex")
				require.NoError(t, err)
				if edge != "codex" {
					owner, alias := seed, protected
					if edge == "reverse" {
						owner, alias = protected, seed
					}
					_, err = f.runtime.Exec(`INSERT INTO session_aliases(session_id,alias_id) VALUES($1,$2)`, owner, alias)
					require.NoError(t, err)
				}
				if empty {
					n, err := h.EmptyTrash(t.Context())
					if edge == "reverse" {
						require.ErrorIs(t, err, db.ErrProtectedSessionPurge)
						assert.Zero(t, n)
					} else {
						require.NoError(t, err)
						assert.Equal(t, 1, n)
					}
				} else {
					n, err := h.DeleteSessionIfTrashed(t.Context(), seed)
					if edge == "reverse" {
						require.ErrorIs(t, err, db.ErrProtectedSessionPurge)
						assert.Zero(t, n)
					} else {
						require.NoError(t, err)
						assert.EqualValues(t, 1, n)
					}
				}
				var retained, excluded bool
				require.NoError(t, f.runtime.QueryRow(`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1),EXISTS(SELECT 1 FROM excluded_sessions WHERE id=$1 OR (id=$2 AND include_codex_pages))`, protected, seed).Scan(&retained, &excluded))
				assert.True(t, retained)
				assert.False(t, excluded, "protected IDs must survive durable alias/thread exclusions")
			})
		}
	}
}

func TestHostedRolloutHiddenRawTrashRemainsRecoverable(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "trash", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("saved")))
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	n, err := f.sink.EmptyTrash(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
	ok, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "clear"))
	r, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, r.State)
}

func TestHostedRolloutLegacyMembershipInvalidatesCursor(t *testing.T) {
	requireHegel(t)
	f := newHostedFixture(t, "tenant-cursor")
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count,started_at) VALUES('a','p','m','codex',1,'2026-01-01'),('b','p','m','codex',1,'2026-01-02')`)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		_, err := f.runtime.Exec(`UPDATE sessions SET rollout_hidden=false WHERE id='b'`)
		require.NoError(ht, err)
		page, err := h.ListSessions(ht.Context(), db.SessionFilter{Limit: 1})
		require.NoError(ht, err)
		require.NotEmpty(ht, page.NextCursor)
		before, err := h.revision(ht.Context())
		require.NoError(ht, err)
		hidden := hegel.Draw(ht, hegel.Booleans())
		_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=$1 WHERE id='b'`, hidden)
		require.NoError(ht, err)
		after, err := h.revision(ht.Context())
		require.NoError(ht, err)
		require.Greater(ht, after.Corpus, before.Corpus)
		_, err = h.ListSessions(ht.Context(), db.SessionFilter{Cursor: page.NextCursor})
		if hidden {
			require.Greater(ht, after.Identity, before.Identity)
			require.ErrorIs(ht, err, ErrHostedCursor)
		} else {
			require.Equal(ht, before.Identity, after.Identity)
			require.NoError(ht, err)
		}
	}, hegel.WithTestCases(1000))
}

// Hegel shrank the eligibility-model failure to one hidden, live legacy row.

func TestHostedRolloutLegacyWriteRechecksAfterLock(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('legacy','p','m','codex')`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-rollout:"+f.schema)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := h.legacyWrite(ctx, "legacy", "legacy", func(tx *sql.Tx, id string) (int64, error) {
			return legacyCurationTx(ctx, tx, id, "starred", true)
		})
		done <- err
	}()
	waitHostedLocks(t, f, ctx, 1)
	_, err = gate.ExecContext(ctx, `UPDATE sessions SET rollout_hidden=true WHERE id='legacy'`)
	require.NoError(t, err)
	require.NoError(t, gate.Commit())
	var gone *db.SessionIdentityError
	require.ErrorAs(t, <-done, &gone)
	assert.Equal(t, "gone", gone.State)
	var starred bool
	require.NoError(t, f.runtime.QueryRow(`SELECT EXISTS(SELECT 1 FROM starred_sessions)`).Scan(&starred))
	assert.False(t, starred)
}

// Removing the preflight must expose a covering global exclusion and lose the
// canonical transcript after reveal and an ordinary mirror push.
func TestHostedRolloutPurgeRejectsProtectedTombstones(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, agent := range []string{"vibe", "codex"} {
			t.Run(fmt.Sprintf("%s/empty=%t", agent, empty), func(t *testing.T) {
				f := newProjectionFixture(t)
				m, _ := f.accept(t, "device-a", "raw-trash", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("raw retained")))
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
				h, err := newHostedAdapter(f.runtime, f.tenant)
				require.NoError(t, err)
				canonical, seed := "vibe:canonical", "vibe:session_fallback"
				source := "/archive/session_fallback/messages.jsonl"
				if agent == "codex" {
					canonical = "codex:00000000-0000-0000-0000-000000000002_00000000-0000-0000-0000-000000000001"
					seed = "codex:00000000-0000-0000-0000-000000000001"
				}
				_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count,rollout_hidden,deleted_at,file_path)
      VALUES($1,'p','m',$3,1,true,NULL,$4),($2,'p','m',$3,1,false,NOW(),NULL),('ordinary','p','m','claude',1,false,NOW(),NULL)`, canonical, seed, agent, source)
				require.NoError(t, err)
				if agent == "vibe" {
					_, err = f.runtime.Exec(`INSERT INTO session_aliases(session_id,alias_id) VALUES($1,$2)`, canonical, seed)
					require.NoError(t, err)
				}
				_, err = f.runtime.Exec(`INSERT INTO messages(session_id,ordinal,role,content) VALUES($1,0,'assistant','retained')`, canonical)
				require.NoError(t, err)
				if empty {
					n, e := h.EmptyTrash(t.Context())
					require.ErrorIs(t, e, db.ErrProtectedSessionPurge)
					assert.Zero(t, n)
				} else {
					n, e := h.DeleteSessionIfTrashed(t.Context(), seed)
					require.ErrorIs(t, e, db.ErrProtectedSessionPurge)
					assert.Zero(t, n)
				}
				var count int
				require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE provenance_kind='legacy'`).Scan(&count))
				assert.Equal(t, 3, count, "conflict must be atomic, including unrelated trash")
				require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM excluded_sessions`).Scan(&count))
				assert.Zero(t, count)
				var rawExcluded int
				require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_curation WHERE field='excluded'`).Scan(&rawExcluded))
				assert.Zero(t, rawExcluded, "raw trash must survive legacy conflict")
				n, err := h.DeleteSessionIfTrashed(t.Context(), "ordinary")
				require.NoError(t, err)
				assert.EqualValues(t, 1, n)
				local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, local.Close()) })
				sess := db.Session{ID: canonical, Project: "p", Machine: "m", Agent: agent, FilePath: &source, MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"}
				require.NoError(t, local.UpsertSession(t.Context(), sess))
				require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{SessionID: canonical, Ordinal: 0, Role: "assistant", Content: "retained", ContentLength: 8}}))
				require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "ordinary", Project: "p", Machine: "m", Agent: "claude", CreatedAt: sess.CreatedAt}))
				syncer := &Sync{pg: f.runtime, local: local, machine: "m", schema: f.schema, schemaDone: true}
				for _, hidden := range []bool{true, false} {
					_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=$2 WHERE id=$1`, canonical, hidden)
					require.NoError(t, err)
					excluded, err := readPGExcludedSessionIDs(t.Context(), f.runtime, func(yield func(db.Session) bool) { yield(sess) }, "", "m", nil)
					require.NoError(t, err)
					assert.Empty(t, excluded)
					result, err := syncer.Push(t.Context(), true, nil)
					require.NoError(t, err)
					assert.Zero(t, result.Errors)
					var content string
					require.NoError(t, f.runtime.QueryRow(`SELECT content FROM messages WHERE session_id=$1 AND ordinal=0`, canonical).Scan(&content))
					assert.Equal(t, "retained", content)
					require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE id='ordinary'`).Scan(&count))
					assert.Zero(t, count, "ordinary purge must remain durable")
					require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM excluded_sessions WHERE id=ANY($1)`, []string{canonical, seed}).Scan(&count))
					assert.Zero(t, count)
				}
			})
		}
	}
}
