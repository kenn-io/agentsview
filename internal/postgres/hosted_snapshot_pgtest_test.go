//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

func tracedHostedReader(t *testing.T, f projectionFixture, argument string) (*HostedStore, *hostedPendingSelectionTracer) {
	t.Helper()
	conn, err := f.runtime.Conn(t.Context())
	require.NoError(t, err)
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(driver any) error {
		cfg = driver.(*stdlib.Conn).Conn().Config()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	tracer := &hostedPendingSelectionTracer{argument: argument}
	cfg.Tracer = tracer
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	h, err := newHostedAdapter(pool, f.tenant)
	require.NoError(t, err)
	return h, tracer
}

// A real publication between SQL statements must not replace or invalidate
// the published transcript currently being read. The writer uses its own pool.
func TestHostedPublicReadsDuringPublication(t *testing.T) {
	for _, operation := range []string{"search", "list", "sidebar", "session", "messages", "pins"} {
		t.Run(operation, func(t *testing.T) {
			f := newProjectionFixture(t)
			m, accepted := f.accept(t, "device-a", "initial", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("initial needle")))
			argument := "codex:portable"
			switch operation {
			case "search":
				argument = "needle"
			case "list", "sidebar":
				argument = "synthetic"
			}
			h, tracer := tracedHostedReader(t, f, argument)
			initial, err := h.GetSession(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NotNil(t, initial)
			if operation == "pins" {
				_, err = h.PinMessage(t.Context(), "codex:portable", 0, new("retained note"))
				require.NoError(t, err)
			}
			before, err := h.revision(t.Context())
			require.NoError(t, err)
			publications := 0
			tracer.after = func() {
				// Bound a broken reader without allowing its three retries to
				// succeed merely because this writer stopped after one change.
				if publications == 16 {
					return
				}
				publications++
				next, receipt := f.accept(t, "device-a", fmt.Sprintf("updated-%d", publications), accepted.Receipt)
				accepted = receipt
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, projectionOutcome(fmt.Sprintf("updated needle %d", publications))))
			}
			switch operation {
			case "search":
				page, err := h.Search(t.Context(), db.SearchFilter{Query: "needle", Limit: 20})
				require.NoError(t, err)
				require.Len(t, page.Results, 1)
				assert.Equal(t, "codex:portable", page.Results[0].SessionID)
				assert.Contains(t, page.Results[0].Snippet, "initial needle")
			case "list":
				page, err := h.ListSessions(t.Context(), db.SessionFilter{Project: "synthetic", Limit: 20})
				require.NoError(t, err)
				require.Len(t, page.Sessions, 1)
				assert.Equal(t, "codex:portable", page.Sessions[0].ID)
				assert.Equal(t, initial.TranscriptRevision, page.Sessions[0].TranscriptRevision)
			case "sidebar":
				page, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Project: "synthetic", Limit: 20})
				require.NoError(t, err)
				require.Len(t, page.Sessions, 1)
				assert.Equal(t, "codex:portable", page.Sessions[0].ID)
				assert.Equal(t, initial.TranscriptRevision, page.Sessions[0].TranscriptRevision)
			case "session":
				session, err := h.GetSession(t.Context(), "codex:portable")
				require.NoError(t, err)
				require.NotNil(t, session)
				assert.Equal(t, "codex:portable", session.ID)
				assert.Equal(t, initial.TranscriptRevision, session.TranscriptRevision)
			case "messages":
				messages, err := h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Equal(t, "initial needle", messages[0].Content)
			case "pins":
				pins, err := h.ListPinnedMessages(t.Context(), "codex:portable", "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, "codex:portable", pins[0].SessionID)
				assert.False(t, pins[0].Unresolved)
				require.NotNil(t, pins[0].Content)
				assert.Equal(t, "initial needle", *pins[0].Content)
			}
			tracer.after = nil
			require.Positive(t, publications, "publication hook must run during the read")
			after, err := h.revision(t.Context())
			require.NoError(t, err)
			assert.Greater(t, after.Corpus, before.Corpus)
			current, err := h.GetMessages(t.Context(), "codex:portable", 0, 1, true)
			require.NoError(t, err)
			require.Len(t, current, 1)
			assert.Equal(t, fmt.Sprintf("updated needle %d", publications), current[0].Content)
		})
	}
}

func TestHostedPricedReadWithSingleConnection(t *testing.T) {
	for _, missing := range []string{"none", "model pricing", "GenAI pricing"} {
		t.Run(missing, func(t *testing.T) {
			f := newProjectionFixture(t)
			m, _ := f.accept(t, "device-a", "priced", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("priced transcript")))
			h, _ := tracedHostedReader(t, f, "")
			switch missing {
			case "model pricing":
				_, err := f.admin.ExecContext(t.Context(), `DROP TABLE model_pricing_bands, model_pricing`)
				require.NoError(t, err)
			case "GenAI pricing":
				_, err := f.admin.ExecContext(t.Context(), `DROP TABLE genai_pricing`)
				require.NoError(t, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			for _, rates := range []struct {
				input, output string
				cost          int64
			}{{"2", "3", 43}, {"5", "7", 104}} {
				h.SetCustomPricing(map[string]config.CustomModelRate{"synthetic-model": {
					InputMicrodollarsPerMTok:  money.MustParseDollars(rates.input).Microdollars,
					OutputMicrodollarsPerMTok: money.MustParseDollars(rates.output).Microdollars,
				}})
				usage, err := h.GetSessionUsage(ctx, "codex:portable", true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, "codex:portable", usage.SessionID)
				assert.True(t, usage.HasCost)
				assert.Equal(t, rates.cost, usage.Cost.Microdollars)
			}
		})
	}
}

func TestHostedCanceledReadReleasesConnection(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "initial", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("initial needle")))
	h, tracer := tracedHostedReader(t, f, "needle")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tracer.after = cancel
	_, err := h.Search(ctx, db.SearchFilter{Query: "needle", Limit: 20})
	require.ErrorIs(t, err, context.Canceled)
	tracer.after = nil
	recovery, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	page, err := h.Search(recovery, db.SearchFilter{Query: "needle", Limit: 20})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "codex:portable", page.Results[0].SessionID)
}
