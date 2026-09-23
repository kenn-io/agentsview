//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawProjectionRemovesUnresolvedPinAcrossCurrentCohort(t *testing.T) {
	f := newProjectionFixture(t)
	a, ar := f.accept(t, "device-a", "old-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("original")))
	b, br := f.accept(t, "device-b", "old-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("original")))
	aliasA, aliasB := f.alias(t, a), f.alias(t, b)
	require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 0, true, "group note"))
	refs, err := f.sink.PinReferences(t.Context(), aliasA)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	oldKey := refs[0].MessageKey
	a, ar = f.accept(t, "device-a", "changed-a", ar.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("replacement")))
	b, br = f.accept(t, "device-b", "changed-b", br.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("replacement")))
	refs, err = f.sink.PinReferences(t.Context(), aliasA)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.False(t, refs[0].Resolved)
	require.NoError(t, f.sink.RemovePin(t.Context(), aliasA, oldKey))
	for _, alias := range []string{aliasA, aliasB} {
		refs, err = f.sink.PinReferences(t.Context(), alias)
		require.NoError(t, err)
		assert.Empty(t, refs)
	}
	// Both members keep the explicit unpin after a split, even when the original
	// message becomes resolvable again on one branch.
	b, br = f.accept(t, "device-b", "original-b", br.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("original")))
	refs, err = f.sink.PinReferences(t.Context(), aliasB)
	require.NoError(t, err)
	assert.Empty(t, refs)
	require.NoError(t, f.sink.SetPin(t.Context(), aliasB, 0, true, "branch note"))
	b, br = f.accept(t, "device-b", "replacement-b-again", br.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("replacement")))
	refs, err = f.sink.PinReferences(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.False(t, refs[0].Resolved)
	require.NoError(t, f.sink.RemovePin(t.Context(), "codex:portable", oldKey))
	require.NoError(t, f.sink.RemovePin(t.Context(), "codex:portable", oldKey))
	b, _ = f.accept(t, "device-b", "original-b-after-base-clear", br.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("original")))
	refs, err = f.sink.PinReferences(t.Context(), aliasB)
	require.NoError(t, err)
	assert.Empty(t, refs, "base removal supersedes branch pin override")
}
