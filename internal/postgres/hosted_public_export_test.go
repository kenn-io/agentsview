//go:build pgtest

package postgres

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"testing"
)

// HostedPublicFixture is shared only with the external service/HTTP tests.
func HostedPublicFixture(t *testing.T) (db.Store, func(), func()) {
	t.Helper()
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("readable transcript")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	split := func() {
		b, _ := f.accept(t, "device-b", "capture-b", "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("other transcript")))
	}
	pin := func() { _, err := h.PinMessage("codex:portable", 0, new("remember this")); require.NoError(t, err) }
	return h, split, pin
}
