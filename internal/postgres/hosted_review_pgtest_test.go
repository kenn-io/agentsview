//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func waitHostedLocks(t *testing.T, f projectionFixture, ctx context.Context, count int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock'`, f.role).Scan(&n)
		return err == nil && n >= count
	}, 3*time.Second, 10*time.Millisecond)
}

func TestHostedPinResponseUsesTransactionCohort(t *testing.T) {
	f := newProjectionFixture(t)
	first, accepted := f.accept(t, "device-a", "pin-race-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, first), first, projectionOutcome("before pin")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	old, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	next, _ := f.accept(t, "device-a", "pin-race-b", accepted.Receipt)
	lease := f.lease(t, next)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT group_id FROM raw_session_groups WHERE group_id=$1 FOR UPDATE`, old.GroupID)
	require.NoError(t, err)
	published := make(chan error, 1)
	go func() { published <- f.sink.Project(ctx, lease, next, projectionOutcome("after pin")) }()
	waitHostedLocks(t, f, ctx, 1)
	type response struct {
		id  int64
		err error
	}
	pinned := make(chan response, 1)
	go func() {
		id, err := h.PinMessage("codex:portable", 0, new("transaction pin"))
		pinned <- response{id, err}
	}()
	waitHostedLocks(t, f, ctx, 2)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-published)
	result := <-pinned
	require.NoError(t, result.err)
	require.Positive(t, result.id)
	var content, note string
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT m.content,p.note FROM pinned_messages p JOIN messages m ON m.session_id=p.session_id AND m.ordinal=p.ordinal WHERE p.id=$1`, result.id).Scan(&content, &note))
	assert.Equal(t, "after pin", content)
	assert.Equal(t, "transaction pin", note)
}

func TestHostedLegacyWriteFencesFirstRawPublication(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('codex:portable','archive','machine','codex')`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	manifest, _ := f.accept(t, "device-a", "first-raw-publication", "")
	lease := f.lease(t, manifest)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	require.NoError(t, lockHostedAliases(ctx, gate, []string{"codex:portable"}))
	published := make(chan error, 1)
	go func() { published <- f.sink.Project(ctx, lease, manifest, projectionOutcome("raw collision")) }()
	waitHostedLocks(t, f, ctx, 1)
	starred := make(chan error, 1)
	go func() { _, err := h.StarSession("codex:portable"); starred <- err }()
	waitHostedLocks(t, f, ctx, 2)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-published)
	var ambiguity *db.SessionIdentityError
	require.ErrorAs(t, <-starred, &ambiguity)
	var star bool
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM starred_sessions WHERE session_id='codex:portable')`).Scan(&star))
	assert.False(t, star)
	explicit := legacyPublicVariant("codex:portable")
	ok, err := h.StarSession(explicit)
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, h.RenameSession(explicit, new("legacy title")))
	require.NoError(t, h.SoftDeleteSession(explicit))
	n, err := h.RestoreSession(explicit)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	require.NoError(t, h.UnstarSession(explicit))

	_, err = f.runtime.ExecContext(ctx, `INSERT INTO messages(session_id,ordinal,role,content) VALUES('codex:portable',0,'user','legacy pinned text')`)
	require.NoError(t, err)
	pinID, err := h.PinMessage(explicit, 0, new("legacy note"))
	require.NoError(t, err)
	require.Positive(t, pinID)
	require.NoError(t, h.UnpinMessage(explicit, 0))
	legacy, err := h.GetSession(ctx, explicit)
	require.NoError(t, err)
	require.NotNil(t, legacy)
	assert.Equal(t, "legacy title", *legacy.DisplayName)

	require.NoError(t, h.SoftDeleteSession(explicit))
	removed, err := h.DeleteSessionIfTrashed(explicit)
	require.NoError(t, err)
	assert.EqualValues(t, 1, removed)
	raw, err := h.GetSession(ctx, "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, raw)
	assert.Equal(t, "codex:portable", raw.ID)
}

func TestHostedLegacyIdentityTriggerContentionIsAtomic(t *testing.T) {
	f := newProjectionFixture(t)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	before, err := h.revision(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	gate, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	require.NoError(t, lockHostedAliases(ctx, gate, []string{"codex:portable"}))
	_, err = f.admin.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,tenant_id) VALUES('codex:portable','archive','machine','codex',$1)`, f.tenant)
	var pgerr *pgconn.PgError
	require.ErrorAs(t, err, &pgerr)
	assert.Equal(t, "40001", pgerr.Code)
	after, err := h.revision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	var exists bool
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id='codex:portable')`).Scan(&exists))
	assert.False(t, exists)
	require.NoError(t, gate.Commit())
	_, err = f.admin.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,tenant_id) VALUES('codex:portable','archive','machine','codex',$1)`, f.tenant)
	require.NoError(t, err)
	after, err = h.revision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before.Identity+1, after.Identity)
}

func TestHostedLegacyTrashPassCannotDeleteNewRawTrash(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "late-trash", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("late raw trash")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	n, err := h.core.EmptyTrash(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
	require.NoError(t, h.SoftDeleteSession("codex:portable"))
	n, err = h.physical.emptyTrash(true)
	require.NoError(t, err)
	assert.Zero(t, n)
	var cause sql.NullString
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT deletion_cause FROM sessions WHERE provenance_kind='raw'`).Scan(&cause))
	assert.Equal(t, "user", cause.String)
	n, err = h.EmptyTrash()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestHostedPinInventorySkipsFalseHistoricalPayloads(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "unpinned-history", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("historical pin")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = h.PinMessage("codex:portable", 0, nil)
	require.NoError(t, err)
	require.NoError(t, h.UnpinMessage("codex:portable", 0))
	// Corruption is deliberate proof that an ineffective historical candidate is
	// never loaded or decoded by an empty inventory request.
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_content_revisions SET payload='not-json'::bytea`)
	require.NoError(t, err)
	pins, err := h.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

func TestHostedPinPageBoundsHydrationAndOrdersUnresolved(t *testing.T) {
	f := newProjectionFixture(t)
	first, accepted := f.accept(t, "device-a", "bounded-pin-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, first), first, projectionOutcome("durable old pin")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = h.PinMessage("codex:portable", 0, new("newest retained pin"))
	require.NoError(t, err)
	next, _ := f.accept(t, "device-a", "bounded-pin-b", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, projectionOutcome("replacement")))
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_pins SET created_at='2025-01-01'`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('legacy-pin-page','archive','machine','codex'); INSERT INTO messages(session_id,ordinal,role,content) SELECT 'legacy-pin-page',n,'user','legacy pin '||n FROM generate_series(1,500)n; INSERT INTO pinned_messages(session_id,message_id,ordinal,created_at) SELECT 'legacy-pin-page',n,n,'2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,500)n`)
	require.NoError(t, err)
	target, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var payload []byte
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT payload FROM raw_content_revisions WHERE session_id=$1`, target.SessionID).Scan(&payload))
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_content_revisions SET payload='not-json'::bytea WHERE session_id=$1`, target.SessionID)
	require.NoError(t, err)
	pins, err := h.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)
	require.Len(t, pins, 500)
	assert.Equal(t, "legacy pin 500", *pins[0].Content)
	assert.Equal(t, "legacy pin 1", *pins[499].Content)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_content_revisions SET payload=$2 WHERE session_id=$1`, target.SessionID, payload)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_pins SET created_at='2027-01-01'`)
	require.NoError(t, err)
	pins, err = h.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)
	require.Len(t, pins, 500)
	assert.Equal(t, "codex:portable", pins[0].SessionID)
	assert.True(t, pins[0].Unresolved)
	assert.Equal(t, "2027-01-01T00:00:00Z", pins[0].CreatedAt)
	assert.Equal(t, "newest retained pin", *pins[0].Note)
	assert.Equal(t, "legacy pin 500", *pins[1].Content)
	assert.Equal(t, "legacy pin 2", *pins[499].Content)
}

func TestHostedLegacyIdentityTriggerDoesNotWaitBehindGroup(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "group-contention", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("existing raw")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	target, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	before, err := h.revision(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	gate, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT group_id FROM raw_session_groups WHERE group_id=$1 FOR UPDATE`, target.GroupID)
	require.NoError(t, err)
	_, err = f.admin.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,tenant_id) VALUES('codex:portable','archive','machine','codex',$1)`, f.tenant)
	var pgerr *pgconn.PgError
	require.ErrorAs(t, err, &pgerr)
	assert.Equal(t, "40001", pgerr.Code)
	after, err := h.revision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, gate.Commit())
	_, err = f.admin.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,tenant_id) VALUES('codex:portable','archive','machine','codex',$1)`, f.tenant)
	require.NoError(t, err)
	_, err = h.StarSession("codex:portable")
	var ambiguity *db.SessionIdentityError
	require.ErrorAs(t, err, &ambiguity)
}

func TestHostedPinPageAppliesEffectiveCohortOverrides(t *testing.T) {
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "pin-cohort-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("cohort pin")))
	b, _ := f.accept(t, "device-b", "pin-cohort-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("cohort pin")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = h.PinMessage("codex:portable", 0, new("inherited"))
	require.NoError(t, err)
	// Retained member overrides can differ after reconvergence. A false override
	// on A must not suppress B's inherited group pin.
	aa := f.alias(t, a)
	ba := f.alias(t, b)
	// Target the durable per-member overrides directly, then materialize using
	// the normal curation path; this isolates heterogeneous membership history.
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note,created_at) SELECT p.group_id,a.anchor_branch,p.message_key,p.ordinal,p.content_revision,false,'suppressed',p.created_at FROM raw_pins p JOIN raw_session_public_aliases a ON a.group_id=p.group_id WHERE p.branch_id='' AND a.alias_id=$1`, aa)
	require.NoError(t, err)
	require.NoError(t, h.RenameSession("codex:portable", nil))
	pins, err := h.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, "inherited", *pins[0].Note)
	assert.False(t, pins[0].Unresolved)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note,created_at) SELECT p.group_id,a.anchor_branch,p.message_key,p.ordinal,p.content_revision,false,'suppressed',p.created_at FROM raw_pins p JOIN raw_session_public_aliases a ON a.group_id=p.group_id WHERE p.branch_id='' AND a.alias_id=$1`, ba)
	require.NoError(t, err)
	require.NoError(t, h.RenameSession("codex:portable", nil))
	pins, err = h.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

func TestHostedAliasBucketsBoundContentionWithoutGlobalLock(t *testing.T) {
	f := newProjectionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var same, different string
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT 'legacy-bucket-'||n FROM generate_series(1,10000)n WHERE (hashtextextended('legacy-bucket-'||n,0)&255)=(hashtextextended('codex:portable',0)&255) LIMIT 1`).Scan(&same))
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT 'legacy-bucket-'||n FROM generate_series(1,10000)n WHERE (hashtextextended('legacy-bucket-'||n,0)&255)<>(hashtextextended('codex:portable',0)&255) AND (hashtextextended(hosted_legacy_alias('legacy-bucket-'||n),0)&255)<>(hashtextextended('codex:portable',0)&255) LIMIT 1`).Scan(&different))
	gate, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	require.NoError(t, lockHostedAliases(ctx, gate, []string{"codex:portable"}))
	_, err = f.runtime.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent) VALUES($1,'archive','machine','codex')`, same)
	var pgerr *pgconn.PgError
	require.ErrorAs(t, err, &pgerr)
	assert.Equal(t, "40001", pgerr.Code)
	_, err = f.runtime.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent) VALUES($1,'independent','machine','codex')`, different)
	require.NoError(t, err)
	var project string
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT project FROM sessions WHERE id=$1`, different).Scan(&project))
	assert.Equal(t, "independent", project)
	require.NoError(t, gate.Commit())
	_, err = f.runtime.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent) VALUES($1,'retried','machine','codex')`, same)
	require.NoError(t, err)
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT project FROM sessions WHERE id=$1`, same).Scan(&project))
	assert.Equal(t, "retried", project)
}

func TestHostedLegacyWriteFencesFirstVibeFallbackPublication(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.accept(t, "device-a", "first-vibe-fallback", "", parser.AgentVibe)
	const canonical = "vibe:portable"
	group, _ := rawGroupID(manifest, db.Session{ID: canonical, Agent: "vibe", SourceSessionID: "portable"})
	branch := canonical + "~" + rawDigest("branch-v1", group, rawSourceID(manifest))
	var directory string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT 'session_fallback_'||n FROM generate_series(1,10000)n WHERE (hashtextextended('vibe:session_fallback_'||n,0)&255) NOT IN ((hashtextextended($1,0)&255),(hashtextextended($2,0)&255)) LIMIT 1`, canonical, branch).Scan(&directory))
	fallback := "vibe:" + directory
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES($1,'archive','machine','vibe')`, fallback)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	outcome := projectionOutcome("published Vibe transcript")
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentVibe
	outcome.Outcome.Results[0].Result.Session.ID = canonical
	outcome.Outcome.Results[0].Result.Session.File.Path = "logs/" + directory + "/messages.jsonl"
	lease := f.lease(t, manifest)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	require.NoError(t, lockHostedAliases(ctx, gate, []string{fallback}))
	published := make(chan error, 1)
	go func() { published <- f.sink.Project(ctx, lease, manifest, outcome) }()
	// The fallback bucket differs from both pre-existing lock candidates. Without
	// its explicit inclusion, first publication passes this gate entirely.
	waitHostedLocks(t, f, ctx, 1)
	starred := make(chan error, 1)
	go func() { _, err := h.StarSession(fallback); starred <- err }()
	waitHostedLocks(t, f, ctx, 2)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-published)
	var ambiguity *db.SessionIdentityError
	require.ErrorAs(t, <-starred, &ambiguity)
	assert.Contains(t, ambiguity.Variants, legacyPublicVariant(fallback))
	var star bool
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=$1)`, fallback).Scan(&star))
	assert.False(t, star)
	messages, err := h.GetAllMessages(ctx, canonical)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "published Vibe transcript", messages[0].Content)
	ok, err := h.StarSession(legacyPublicVariant(fallback))
	require.NoError(t, err)
	assert.True(t, ok)
}
