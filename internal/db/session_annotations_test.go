package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listSessionIDs(t *testing.T, d *DB, f SessionFilter) []string {
	t.Helper()
	page, err := d.ListSessions(t.Context(), f)
	require.NoError(t, err)
	ids := make([]string, 0, len(page.Sessions))
	for _, s := range page.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestSessionPRLinksRoundTripAndFilter(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "with-prs", "proj", func(s *Session) {
		s.PRLinks = []PRLink{
			{
				URL: "https://github.com/Owner/Repo/pull/12", Host: "github.com",
				Repository: "Owner/Repo", Number: 12, Source: "transcript",
				FirstSeenAt: "2026-10-05T03:21:20.583Z",
			},
			{
				URL: "https://github.com/owner/other/pull/3", Host: "github.com",
				Repository: "owner/other", Number: 3, Source: "transcript",
			},
		}
	})
	insertSession(t, d, "without-prs", "proj")

	got, err := d.GetSession(t.Context(), "with-prs")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "Owner/Repo", got.PRLinks[0].Repository)
	assert.Equal(t, "2026-10-05T03:21:20.583Z", got.PRLinks[0].FirstSeenAt)

	full, err := d.GetSessionFull(t.Context(), "with-prs")
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.Equal(t, got.PRLinks, full.PRLinks)

	tests := []struct {
		filter string
		want   []string
	}{
		{filter: "owner/repo", want: []string{"with-prs"}},
		{filter: "owner/repo#12", want: []string{"with-prs"}},
		{filter: "https://github.com/OWNER/repo/pull/12", want: []string{"with-prs"}},
		{filter: "owner/repo#13", want: []string{}},
		{filter: "owner/missing", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			pr, err := ParsePRFilter(tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.want, listSessionIDs(t, d, SessionFilter{PR: pr}))
		})
	}

	// A later parse without links clears them: the column is parser-owned.
	insertSession(t, d, "with-prs", "proj")
	got, err = d.GetSession(t.Context(), "with-prs")
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestParsePRFilterRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{
		"repo-without-owner", "owner/repo#", "owner/repo#zero",
		"https://github.com/owner/repo",
	} {
		_, err := ParsePRFilter(value)
		assert.Error(t, err, value)
	}
}

func TestSessionLabelsLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// Labels may be recorded before sync imports the session.
	pending, err := d.SetSessionLabels(ctx, "worker", []string{
		" role=reviewer ", "ticket=ABC-123", "role=reviewer",
	})
	require.NoError(t, err)
	assert.False(t, pending.SessionFound)
	assert.Equal(t, []string{"role=reviewer", "ticket=ABC-123"}, pending.Labels)

	insertSession(t, d, "worker", "proj")
	insertSession(t, d, "other", "proj")
	got, err := d.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"role=reviewer", "ticket=ABC-123"}, got.Labels)

	before, err := d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	updated, err := d.UpdateSessionLabels(ctx, "worker",
		[]string{"nightly"}, []string{"role=reviewer", "absent"})
	require.NoError(t, err)
	assert.True(t, updated.SessionFound)
	assert.Equal(t, []string{"nightly", "ticket=ABC-123"}, updated.Labels)
	after, err := d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	require.NotNil(t, after.LocalModifiedAt,
		"label changes must mark the session modified for mirror pushes")
	assert.NotEqual(t, before.LocalModifiedAt, after.LocalModifiedAt)

	// Parser writes never touch labels.
	insertSession(t, d, "worker", "proj")
	stored, err := d.GetSessionLabels(ctx, "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"nightly", "ticket=ABC-123"}, stored.Labels)

	assert.Equal(t, []string{"worker"},
		listSessionIDs(t, d, SessionFilter{Labels: []string{"nightly", "ticket=ABC-123"}}))
	assert.Equal(t, []string{},
		listSessionIDs(t, d, SessionFilter{Labels: []string{"nightly", "role=reviewer"}}))

	cleared, err := d.SetSessionLabels(ctx, "worker", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{}, cleared.Labels)
	got, err = d.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Empty(t, got.Labels)
}

func TestSessionLabelsRejectInvalidInput(t *testing.T) {
	d := testDB(t)
	tooMany := make([]string, MaxSessionLabels+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("label-%d", i)
	}
	for name, labels := range map[string][]string{
		"empty":       {"  "},
		"empty key":   {"=value"},
		"control":     {"bad\nlabel"},
		"too long":    {strings.Repeat("a", MaxSessionLabelBytes+1)},
		"too many":    tooMany,
		"invalid utf": {"\xff"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.SetSessionLabels(t.Context(), "s", labels)
			assert.ErrorIs(t, err, ErrSessionLabelsInvalid)
		})
	}
}

func TestSessionExternalParentApplication(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "manager", "proj")

	// A launcher can link a worker before sync imports it.
	link, err := d.SetSessionExternalParent(ctx, "worker", "manager", "")
	require.NoError(t, err)
	assert.False(t, link.SessionFound)
	assert.False(t, link.Applied)
	assert.Equal(t, DefaultExternalRelationshipType, link.RelationshipType)

	insertSession(t, d, "worker", "proj")
	worker, err := d.GetSession(ctx, "worker")
	require.NoError(t, err)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, "manager", *worker.ParentSessionID)
	assert.Equal(t, "subagent", worker.RelationshipType)

	children, err := d.GetChildSessions(ctx, "manager")
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "worker", children[0].ID)

	// Every parser rewrite clears the column; the link survives it.
	insertSession(t, d, "worker", "proj")
	worker, err = d.GetSession(ctx, "worker")
	require.NoError(t, err)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, "manager", *worker.ParentSessionID)

	link, err = d.GetSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	assert.True(t, link.Applied)

	cleared, err := d.ClearSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	assert.False(t, cleared.Applied)
	worker, err = d.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Nil(t, worker.ParentSessionID)
	assert.Empty(t, worker.RelationshipType)
	_, err = d.ClearSessionExternalParent(ctx, "worker")
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestSessionExternalParentYieldsToParserParent(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "manager", "proj")
	insertSession(t, d, "real-parent", "proj")
	parsedParent := "real-parent"
	insertSession(t, d, "child", "proj", func(s *Session) {
		s.ParentSessionID = &parsedParent
		s.RelationshipType = "continuation"
	})

	link, err := d.SetSessionExternalParent(ctx, "child", "manager", "subagent")
	require.NoError(t, err)
	assert.True(t, link.SessionFound)
	assert.False(t, link.Applied)

	child, err := d.GetSession(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "real-parent", *child.ParentSessionID)
	assert.Equal(t, "continuation", child.RelationshipType)
}

func TestSessionExternalParentRejectsInvalidLinks(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "proj")
	insertSession(t, d, "b", "proj")
	_, err := d.SetSessionExternalParent(ctx, "b", "a", "")
	require.NoError(t, err)
	_, err = d.SetSessionExternalParent(ctx, "pending-c", "b", "")
	require.NoError(t, err)

	tests := []struct {
		name, session, parent, relationship string
	}{
		{name: "self", session: "a", parent: "a"},
		{name: "cycle through effective parent", session: "a", parent: "b"},
		{name: "cycle through pending link", session: "a", parent: "pending-c"},
		{name: "unknown relationship", session: "a", parent: "x", relationship: "delegated"},
		{name: "missing parent", session: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.SetSessionExternalParent(
				ctx, tt.session, tt.parent, tt.relationship,
			)
			assert.ErrorIs(t, err, ErrSessionExternalParentInvalid)
		})
	}
}

func TestCopySessionMetadataFromPreservesLabelsAndExternalParents(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(ctx, sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	insertSession(t, source, "manager", "proj")
	insertSession(t, source, "worker", "proj")
	_, err = source.SetSessionLabels(ctx, "worker", []string{"role=reviewer"})
	require.NoError(t, err)
	_, err = source.SetSessionLabels(ctx, "not-synced-yet", []string{"queued"})
	require.NoError(t, err)
	_, err = source.SetSessionExternalParent(ctx, "worker", "manager", "")
	require.NoError(t, err)

	destinationPath := filepath.Join(dir, "destination.db")
	destination, err := Open(ctx, destinationPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = destination.Close() })
	insertSession(t, destination, "manager", "proj")
	insertSession(t, destination, "worker", "proj")

	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))

	worker, err := destination.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"role=reviewer"}, worker.Labels)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, "manager", *worker.ParentSessionID)
	pending, err := destination.GetSessionLabels(ctx, "not-synced-yet")
	require.NoError(t, err)
	assert.Equal(t, []string{"queued"}, pending.Labels)
}
