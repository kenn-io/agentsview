//go:build pgtest

package postgres

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRawProjectionEmptyTrashExcludesOnlyFullyTrashedCohorts(t *testing.T) {
	f := newProjectionFixture(t)
	a, ar := f.accept(t, "device-a", "exclude-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("equal")))
	aa := f.alias(t, a)
	require.NoError(t, f.sink.SetCuration(t.Context(), aa, "display_name", "retained excluded name"))
	require.NoError(t, f.sink.SetCuration(t.Context(), aa, "trashed", true))
	b, _ := f.accept(t, "device-b", "exclude-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("equal")))
	removed, err := f.sink.EmptyTrash(t.Context())
	require.NoError(t, err)
	assert.Zero(t, removed)
	next, nr := f.accept(t, "device-a", "split-excluded", ar.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, projectionOutcome("split")))
	removed, err = f.sink.EmptyTrash(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	r, err := f.sink.Resolve(t.Context(), aa)
	require.NoError(t, err)
	assert.Equal(t, RawIdentityGone, r.State)
	newest, _ := f.accept(t, "device-a", "excluded-reparse", nr.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, newest), newest, projectionOutcome("equal")))

	require.NoError(t, f.sink.SetCuration(t.Context(), f.alias(t, b), "display_name", "visible name"))
	var name string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT c.value #>> '{}' FROM raw_curation c JOIN raw_session_public_aliases a ON a.anchor_branch=c.branch_id WHERE a.alias_id=$1 AND c.field='display_name'`, aa).Scan(&name))
	assert.Equal(t, "retained excluded name", name)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM session_sources`).Scan(&count))
	assert.Equal(t, 2, count, "user exclusion retains source proof")
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_session_branches WHERE active`).Scan(&count))
	assert.Equal(t, 2, count)
	trash, err := (&Store{pg: f.runtime}).ListTrashedSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, trash)
}
func TestRawProjectionBaseExclusionAlsoCoversFutureBranches(t *testing.T) {
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "base-exclude-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("content")))
	removed, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.False(t, removed)
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
	removed, err = f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.True(t, removed)
	b, _ := f.accept(t, "device-b", "base-exclude-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("new source")))
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM session_sources`).Scan(&count))
	assert.Equal(t, 2, count)
}
