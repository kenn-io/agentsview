//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func (f projectionFixture) hosted(t *testing.T) *HostedStore {
	t.Helper()
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}

func TestRawProjectionTombstoneRetainsArchivedSession(t *testing.T) {
	appended := projectionOutcome("needle")
	appended.Outcome.Results[0].Result.Messages = append(appended.Outcome.Results[0].Result.Messages, parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "follow-up"})
	tests := []struct {
		name         string
		returned     rawderive.ParsedManifest
		wantMessages int
	}{
		{"source returns unchanged", projectionOutcome("needle"), 2},
		{"source returns with appended history", appended, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			h := f.hosted(t)
			m, accepted := f.accept(t, "device-a", "visible", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("needle")))
			_, err := h.StarSession(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 0, true, "retained"))
			matches, err := h.Search(t.Context(), db.SearchFilter{Query: "needle", Limit: 10})
			require.NoError(t, err)
			require.Len(t, matches.Results, 1)
			before, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)

			tomb, removed := f.tombstone(t, m, "tombstone", accepted.Receipt)

			page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, "codex:portable", page.Sessions[0].ID)
			messages, err := h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "needle", messages[0].Content)
			matches, err = h.Search(t.Context(), db.SearchFilter{Query: "needle", Limit: 10})
			require.NoError(t, err)
			require.Len(t, matches.Results, 1)
			assert.Equal(t, "codex:portable", matches.Results[0].SessionID)
			trash, err := h.ListTrashedSessions(t.Context())
			require.NoError(t, err)
			assert.Empty(t, trash)
			retained, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, before.SessionID, retained.SessionID)
			assert.Equal(t, before.CorpusRevision, retained.CorpusRevision, "a departed source must not republish the corpus")
			assert.Equal(t, before.IdentityRevision, retained.IdentityRevision)

			// The session keeps the proof of the snapshot that supplied it,
			// while the source itself is recorded as gone.
			var proof, head string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT manifest_id FROM session_sources WHERE physical_session_id=$1`, retained.SessionID).Scan(&proof))
			assert.Equal(t, m.ManifestID, proof)
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT successful_manifest_id FROM raw_source_projections WHERE source_id=$1`, rawSourceID(m)).Scan(&head))
			assert.Equal(t, tomb.ManifestID, head)

			next, _ := f.accept(t, "device-a", "returned", removed.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, tt.returned))
			page, err = h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			messages, err = h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			assert.Len(t, messages, tt.wantMessages)
			stars, err := h.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{"codex:portable"}, stars)
			pins, err := h.ListPinnedMessages(t.Context(), "codex:portable", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, new("retained"), pins[0].Note)
		})
	}
}

// Another device's shorter copy must not replace the longer transcript just
// because the device that captured it no longer holds the file.
func TestRawProjectionTombstoneKeepsLongerCopyFromDepartedDevice(t *testing.T) {
	f := newProjectionFixture(t)
	h := f.hosted(t)
	short := projectionOutcome("same prompt")
	short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
	a, _ := f.accept(t, "device-a", "short", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
	b, accepted := f.accept(t, "device-b", "long", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("same prompt")))

	f.tombstone(t, b, "long-departed", accepted.Receipt)

	page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	messages, err := h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
	require.NoError(t, err)
	assert.Len(t, messages, 2)
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var sources int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM session_sources WHERE physical_session_id=$1`, resolved.SessionID).Scan(&sources))
	assert.Equal(t, 2, sources)
}

// Retention covers only a source leaving its device. Sessions the user, the
// parser, or the provider already removed stay removed.
func TestRawProjectionTombstoneDoesNotRestoreRemovedSessions(t *testing.T) {
	type fixture struct {
		projectionFixture
		h        *HostedStore
		manifest rawsync.CanonicalManifest
		receipt  string
	}
	deletePermanently := func(t *testing.T, f *fixture) {
		t.Helper()
		require.NoError(t, f.h.SoftDeleteSession(t.Context(), "codex:portable"))
		deleted, err := f.h.DeleteSessionIfTrashed(t.Context(), "codex:portable")
		require.NoError(t, err)
		require.Equal(t, int64(1), deleted)
	}
	project := func(t *testing.T, f *fixture, capture string, outcome rawderive.ParsedManifest) {
		t.Helper()
		var accepted rawsync.CommitResult
		f.manifest, accepted = f.accept(t, "device-a", capture, f.receipt)
		f.receipt = accepted.Receipt
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, f.manifest), f.manifest, outcome))
	}
	depart := func(t *testing.T, f *fixture) {
		t.Helper()
		var removed rawsync.CommitResult
		f.manifest, removed = f.tombstone(t, f.manifest, "departed", f.receipt)
		f.receipt = removed.Receipt
	}
	tests := []struct {
		name      string
		steps     func(*testing.T, *fixture)
		wantTrash int
	}{
		{"trashed before the source left", func(t *testing.T, f *fixture) {
			require.NoError(t, f.h.SoftDeleteSession(t.Context(), "codex:portable"))
			depart(t, f)
		}, 1},
		{"permanently deleted before the source left", func(t *testing.T, f *fixture) {
			deletePermanently(t, f)
			depart(t, f)
		}, 0},
		{"permanently deleted after the source left, then the source returns", func(t *testing.T, f *fixture) {
			depart(t, f)
			deletePermanently(t, f)
			project(t, f, "returned", projectionOutcome("retained"))
		}, 0},
		{"excluded by the parser before the source left", func(t *testing.T, f *fixture) {
			project(t, f, "excluded", rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ExcludedSessionIDs: []string{"codex:portable"}}})
			depart(t, f)
		}, 0},
		{"removed by an authoritative snapshot before the source left", func(t *testing.T, f *fixture) {
			project(t, f, "emptied", rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}})
			depart(t, f)
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fixture{projectionFixture: newProjectionFixture(t)}
			f.h = f.hosted(t)
			project(t, f, "included", projectionOutcome("retained"))
			tt.steps(t, f)
			page, err := f.h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, page.Sessions)
			trash, err := f.h.ListTrashedSessions(t.Context())
			require.NoError(t, err)
			assert.Len(t, trash, tt.wantTrash)
		})
	}
}
