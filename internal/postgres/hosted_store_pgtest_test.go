//go:build pgtest

package postgres

import (
	"encoding/base64"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"testing"
	"time"
)

// A physical Store passed through the hosted constructor cannot resolve the
// provider alias and leaks storage identities in its list and transcript.
func TestHostedPublicReadBoundary(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("public transcript")))
	raw, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	store, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	session, err := store.GetSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "codex:portable", session.ID)
	messages, err := store.GetAllMessages(t.Context(), session.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "public transcript", messages[0].Content)
	assert.Equal(t, "codex:portable", messages[0].SessionID)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "codex:portable", messages[1].ToolCalls[0].SessionID)
	hidden, err := store.GetSession(t.Context(), raw.SessionID)
	require.NoError(t, err)
	assert.Nil(t, hidden)
	page, err := store.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:portable", page.Sessions[0].ID)
}

func TestHostedPublicConflictCurationAndCursor(t *testing.T) {
	f := newProjectionFixture(t)
	a, accepted := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("first transcript")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	h.SetCursorSecret([]byte("synthetic test cursor secret"))
	_, err = h.PinMessage("codex:portable", 0, nil)
	require.NoError(t, err)
	require.NoError(t, h.RenameSession("codex:portable", new("Saved title")))
	_, err = h.StarSession("codex:portable")
	require.NoError(t, err)
	b, _ := f.accept(t, "device-b", "capture-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("second transcript")))
	_, err = h.GetSession(t.Context(), "codex:portable")
	var conflict *db.SessionIdentityError
	require.ErrorAs(t, err, &conflict)
	require.Len(t, conflict.Variants, 2)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	require.NotEmpty(t, page.NextCursor)
	assert.Equal(t, "Saved title", *page.Sessions[0].DisplayName)
	decoded, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	require.NoError(t, err)
	assert.NotContains(t, string(decoded), "raw-row-")
	next, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 1, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, next.Sessions, 1)
	assert.NotEqual(t, page.Sessions[0].ID, next.Sessions[0].ID)
	_, err = h.ListSessions(t.Context(), db.SessionFilter{Cursor: h.physical.EncodeCursor(db.SessionCursor{ID: "raw-row-forged"})})
	assert.ErrorIs(t, err, ErrHostedCursor)
	search, err := h.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "transcript", ExcludeSessionIDs: []string{"codex:portable"}, IncludeOneShot: true})
	require.NoError(t, err)
	assert.Empty(t, search.Matches)
	changed, _ := f.accept(t, "device-a", "capture-next", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, changed), changed, projectionOutcome("second transcript")))
	_, err = h.ListSessions(t.Context(), db.SessionFilter{Cursor: page.NextCursor})
	assert.ErrorIs(t, err, ErrHostedCursor)
	pins, err := h.ListPinnedMessages(t.Context(), "codex:portable", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.True(t, pins[0].Unresolved)
	require.NotEmpty(t, pins[0].MessageKey)
	allPins, e := h.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, e)
	require.Len(t, allPins, 1)
	assert.True(t, allPins[0].Unresolved)
	assert.Equal(t, pins[0].MessageKey, allPins[0].MessageKey)
	require.NotNil(t, allPins[0].SessionProject)
	assert.Equal(t, "synthetic", *allPins[0].SessionProject)
	require.NoError(t, h.RemovePinReference(t.Context(), "codex:portable", pins[0].MessageKey))
	pins, err = h.ListPinnedMessages(t.Context(), "codex:portable", "")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

func TestHostedLegacyCollisionRevisions(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("raw transcript")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	before, err := h.revision(t.Context())
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count) VALUES('codex:portable','legacy','archive','codex',2,1)`)
	require.NoError(t, err)
	after, err := h.revision(t.Context())
	require.NoError(t, err)
	assert.Greater(t, after.Identity, before.Identity)
	_, err = h.GetSession(t.Context(), "codex:portable")
	var conflict *db.SessionIdentityError
	require.ErrorAs(t, err, &conflict)
	require.Len(t, conflict.Variants, 2)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2)
	for _, s := range page.Sessions {
		assert.NotEqual(t, "codex:portable", s.ID)
		full, e := h.GetSessionFull(t.Context(), s.ID)
		require.NoError(t, e)
		require.NotNil(t, full)
		assert.Equal(t, s.ID, full.ID)
	}
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET display_name='archive title' WHERE id='codex:portable'`)
	require.NoError(t, err)
	content, err := h.revision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, after.Identity, content.Identity)
	assert.Greater(t, content.Corpus, after.Corpus)
}

func TestHostedSharedChildTraversesBothParentVariants(t *testing.T) {
	f := newProjectionFixture(t)
	outcome := func(parentText string) rawderive.ParsedManifest {
		p := projectionOutcome(parentText)
		child := projectionOutcome("shared child").Outcome.Results[0]
		child.Result.Session.ID = "codex:child"
		child.Result.Session.SourceSessionID = "child"
		child.Result.Session.ParentSessionID = "codex:portable"
		child.Result.Session.RelationshipType = "subagent"
		child.Result.Session.EndedAt = child.Result.Session.StartedAt.Add(7 * time.Second)
		p.Outcome.Results[0].Result.Messages[1].ToolCalls[0].SubagentSessionID = "codex:child"
		p.Outcome.Results[0].Result.Messages[1].HasToolUse = true
		p.Outcome.Results = append(p.Outcome.Results, child)
		return p
	}
	a, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome("parent one")))
	b, acceptedB := f.accept(t, "device-b", "capture-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, outcome("parent two")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	_, err = h.GetSession(t.Context(), "codex:portable")
	var conflict *db.SessionIdentityError
	require.ErrorAs(t, err, &conflict)
	child, err := h.GetSession(t.Context(), "codex:child")
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Nil(t, child.ParentSessionID)
	assert.Equal(t, conflict.Variants, child.ParentSessionIDs)
	sidebar, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 2, sidebar.Total)
	require.Len(t, sidebar.Sessions, 2)
	require.NotEmpty(t, sidebar.NextCursor)
	second, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 1, Cursor: sidebar.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Sessions, 2)
	for _, parent := range conflict.Variants {
		children, e := h.GetChildSessions(t.Context(), parent)
		require.NoError(t, e)
		require.Len(t, children, 1)
		assert.Equal(t, "codex:child", children[0].ID)
		require.NotNil(t, children[0].ParentSessionID)
		assert.Equal(t, parent, *children[0].ParentSessionID)
		messages, e := h.GetAllMessages(t.Context(), parent)
		require.NoError(t, e)
		require.Len(t, messages, 2)
		require.Len(t, messages[1].ToolCalls, 1)
		assert.Equal(t, "codex:child", messages[1].ToolCalls[0].SubagentSessionID)
		timing, e := h.GetSessionTiming(t.Context(), parent)
		require.NoError(t, e)
		require.NotNil(t, timing)
		require.NotNil(t, timing.SlowestCall)
		require.NotNil(t, timing.SlowestCall.DurationMs)
		assert.Equal(t, int64(7000), *timing.SlowestCall.DurationMs)
	}
	removed, _ := f.accept(t, "device-b", "capture-without-parent", acceptedB.Receipt)
	withoutParent := outcome("parent two")
	withoutParent.Outcome.Results = withoutParent.Outcome.Results[1:]
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, removed), removed, withoutParent))
	child, err = h.GetSession(t.Context(), "codex:child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "codex:portable", *child.ParentSessionID)
	assert.Empty(t, child.ParentSessionIDs)
	sidebar, err = h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, sidebar.Total)
	require.Len(t, sidebar.Sessions, 2)

}

func TestHostedReadRetriesTheWholeIdentitySnapshot(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("before")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	reads := 0
	value, err := hostedRead(t.Context(), h, func(_ hostedRevision) (string, error) {
		reads++
		target, e := h.resolve(t.Context(), "codex:portable")
		if e != nil {
			return "", e
		}
		messages, e := h.physical.GetAllMessages(t.Context(), target.SessionID)
		if e != nil {
			return "", e
		}
		if reads == 1 {
			next, _ := f.accept(t, "device-a", "capture-b", accepted.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, projectionOutcome("after")))
		}
		return messages[0].Content, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "after", value)
	assert.Equal(t, 2, reads)
	_, err = hostedRead(t.Context(), h, func(_ hostedRevision) (string, error) {
		_, e := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('revision-probe','synthetic','archive','codex') ON CONFLICT(id) DO UPDATE SET display_name='still changing'`)
		return "discard", e
	})
	assert.ErrorIs(t, err, ErrHostedIdentityChanged)
}

func TestHostedUsageAndActivityReferences(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	p := projectionOutcome("usage transcript")
	r := &p.Outcome.Results[0].Result
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.Session.EndedAt = start.Add(time.Minute)
	r.Messages[0].Timestamp = start
	r.Messages[1].Timestamp = start.Add(time.Minute)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, p))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	rows, err := h.GetSessionUsageRows(t.Context(), []string{"codex:portable"})
	require.NoError(t, err)
	require.NotNil(t, rows)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, "codex:portable", rows.Rows[0].SessionID)
	assert.Equal(t, "codex:portable", rows.Rows[0].SourceSessionID)
	assert.Equal(t, 11, rows.Rows[0].InputTokens)
	assert.Equal(t, 7, rows.Rows[0].OutputTokens)
	assert.Equal(t, map[string]int{"codex:portable": 7}, rows.RawOutputTokensBySession)
	usage, err := h.GetSessionUsage(t.Context(), "codex:portable", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, "codex:portable", usage.SessionID)
	require.Len(t, usage.Breakdown, 1)
	assert.Equal(t, 11, usage.Breakdown[0].InputTokens)
	assert.Equal(t, 7, usage.Breakdown[0].OutputTokens)
	query, err := activity.ResolveQuery(activity.QueryInput{Date: "2026-01-01", Preset: "day", Timezone: "UTC"}, start.Add(48*time.Hour))
	require.NoError(t, err)
	artifacts, err := h.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{}, query, nil)
	require.NoError(t, err)
	require.Len(t, artifacts.Sessions, 1)
	assert.Equal(t, "codex:portable", artifacts.Sessions[0].SessionID)
	assert.Contains(t, artifacts.Membership, "codex:portable")
	for _, interval := range artifacts.Report.Intervals {
		assert.Equal(t, "codex:portable", interval.SessionID)
	}
}

func TestHostedCurationGuardChecksCollisionInsideTransaction(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("raw transcript")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('codex:portable','legacy','archive','codex')`)
	require.NoError(t, err)
	// This bypasses the adapter's optimistic preflight deliberately: the core
	// write's configured public guard must still reject the colliding base.
	err = h.core.SetCuration(t.Context(), "codex:portable", "starred", true)
	var conflict *db.SessionIdentityError
	require.ErrorAs(t, err, &conflict)
	require.Len(t, conflict.Variants, 2)
	stars, err := h.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, stars)
}

func TestHostedBulkCurationResolvesEntireBatchBeforeWriting(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("visible")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	var gone *db.SessionIdentityError
	err = h.BulkStarSessions([]string{"codex:portable", "missing"})
	require.ErrorAs(t, err, &gone)
	stars, err := h.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, stars)
	count, err := h.SoftDeleteSessions([]string{"codex:portable", "missing"})
	require.ErrorAs(t, err, &gone)
	assert.Zero(t, count)
	session, err := h.GetSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, session)
}

func TestHostedConstructorRejectsMissingRevisionProtection(t *testing.T) {
	f := newHostedFixture(t, "tenant-projection")
	_, err := f.admin.ExecContext(t.Context(), `DROP TRIGGER hosted_legacy_revision ON sessions`)
	require.NoError(t, err)
	_, err = NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.ErrorContains(t, err, "owner must reprovision")
}

func TestHostedConstructorRejectsWeakenedRevisionProtection(t *testing.T) {
	for _, alteration := range []string{
		`DROP TRIGGER hosted_legacy_revision ON sessions; CREATE TRIGGER hosted_legacy_revision AFTER INSERT OR UPDATE OR DELETE ON sessions FOR EACH ROW WHEN (false) EXECUTE FUNCTION hosted_legacy_revision()`,
		`CREATE FUNCTION noop_revision() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$; DROP TRIGGER hosted_legacy_revision ON sessions; CREATE TRIGGER hosted_legacy_revision AFTER INSERT OR UPDATE OR DELETE ON sessions FOR EACH ROW EXECUTE FUNCTION noop_revision()`,
		`CREATE OR REPLACE FUNCTION hosted_legacy_revision() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`,
	} {
		t.Run(alteration[:min(35, len(alteration))], func(t *testing.T) {
			f := newHostedFixture(t, "tenant-projection")
			_, err := f.admin.ExecContext(t.Context(), alteration)
			require.NoError(t, err)
			_, err = NewHostedStore(f.dsn, f.schema, f.tenant, false)
			require.ErrorContains(t, err, "owner must reprovision")
		})
	}
}

func TestHostedUnresolvedPinRetainsCreationTime(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "pin-time-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("original pinned message")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = h.PinMessage("codex:portable", 0, new("retained note"))
	require.NoError(t, err)
	before, err := h.ListPinnedMessages(t.Context(), "codex:portable", "")
	require.NoError(t, err)
	require.Len(t, before, 1)
	next, _ := f.accept(t, "device-a", "pin-time-b", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, projectionOutcome("replacement at same ordinal")))
	after, err := h.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.True(t, after[0].Unresolved)
	assert.Equal(t, before[0].CreatedAt, after[0].CreatedAt)
	assert.NotEmpty(t, after[0].CreatedAt)
}
