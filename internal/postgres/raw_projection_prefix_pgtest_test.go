//go:build pgtest

package postgres

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawProjectionPrefixCopiesRetainSourceHistory(t *testing.T) {
	for _, longerFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(longerFirst), func(t *testing.T) {
			f := newProjectionFixture(t)
			short := projectionOutcome("same prompt")
			short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
			long := projectionOutcome("same prompt")
			var b rawsync.CanonicalManifest
			var accepted rawsync.CommitResult
			if longerFirst {
				b, accepted = f.accept(t, "device-b", "long", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
			}
			a, shortAccepted := f.accept(t, "device-a", "short", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
			if !longerFirst {
				b, accepted = f.accept(t, "device-b", "long", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
			}
			h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, "codex:portable", page.Sessions[0].ID)
			messages, err := h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			_, err = h.StarSession(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NoError(t, f.sink.SetPin(t.Context(), f.alias(t, a), 0, true, "retained prefix"))
			a, _ = f.accept(t, "device-a", "same-short", shortAccepted.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
			var sources, captures int
			resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*),count(DISTINCT session_id) FROM session_sources WHERE physical_session_id=$1`, resolved.SessionID).Scan(&sources, &captures))
			assert.Equal(t, 2, sources)
			assert.Equal(t, 2, captures)
			// Removing the longer source must reveal the original shorter capture,
			// not leave the other device claiming content it never supplied.
			b, _ = f.accept(t, "device-b", "removed", accepted.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
			messages, err = h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "same prompt", messages[0].Content)
			stars, err := h.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, stars, "codex:portable")
			pins, err := h.ListPinnedMessages(t.Context(), "codex:portable", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, new("retained prefix"), pins[0].Note)
		})
	}
}

func TestRawProjectionPrefixDoesNotChooseDivergentContinuation(t *testing.T) {
	f := newProjectionFixture(t)
	for i := range 3 {
		outcome := projectionOutcome("shared prompt")
		if i == 0 {
			outcome.Outcome.Results[0].Result.Messages = outcome.Outcome.Results[0].Result.Messages[:1]
		} else if i == 2 {
			outcome.Outcome.Results[0].Result.Messages[1].Content = "different continuation"
		}
		m, _ := f.accept(t, fmt.Sprintf("device-%d", i), fmt.Sprintf("capture-%d", i), "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	}
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityAmbiguous, resolved.State)
	require.Len(t, resolved.Variants, 3)
	for _, alias := range resolved.Variants {
		variant, err := f.sink.Resolve(t.Context(), alias)
		require.NoError(t, err)
		assert.Equal(t, RawIdentityUnique, variant.State)
		var exists bool
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1)`, variant.SessionID).Scan(&exists))
		assert.True(t, exists)
	}
}
