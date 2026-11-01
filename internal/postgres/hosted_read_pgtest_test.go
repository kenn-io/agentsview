//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type hostedPendingSelectionTracer struct {
	argument string
	after    func()
}

func (tracer *hostedPendingSelectionTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	for _, arg := range data.Args {
		if value, ok := arg.(string); ok && value == tracer.argument {
			return context.WithValue(ctx, tracer, true)
		}
	}
	return ctx
}

func (tracer *hostedPendingSelectionTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(tracer) == true && tracer.after != nil {
		tracer.after()
	}
}

func TestHostedPublicReadsDuringPendingSelection(t *testing.T) {
	for _, tc := range []struct {
		name, argument string
		deny, probe    bool
	}{
		{"search", "needle", false, false},
		{"search error", "needle", true, false},
		{"source probe", activityReportProjectIdentityGenerationKey, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			m, accepted := f.accept(t, "device-a", "published", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("published needle")))
			// Copy the verified OpenHosted startup boundary, changing only tracing.
			conn, err := f.runtime.Conn(t.Context())
			require.NoError(t, err)
			var config *pgx.ConnConfig
			err = conn.Raw(func(driver any) error {
				config = driver.(*stdlib.Conn).Conn().Config()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			tracer := &hostedPendingSelectionTracer{argument: tc.argument}
			config.Tracer = tracer
			traced := stdlib.OpenDB(*config)
			traced.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, traced.Close()) })
			h, err := newHostedAdapter(traced, f.tenant)
			require.NoError(t, err)
			if tc.deny {
				role, err := quoteIdentifier(f.role)
				require.NoError(t, err)
				_, err = f.admin.ExecContext(t.Context(), `REVOKE SELECT ON messages FROM `+role)
				require.NoError(t, err)
			}
			before, err := h.revision(t.Context())
			require.NoError(t, err)
			reads := 0
			tracer.after = func() {
				reads++
				pending, next := f.accept(t, "device-a", fmt.Sprintf("pending-%d", reads), accepted.Receipt)
				accepted = next
				_, err := f.sink.SelectSourceGeneration(t.Context(), pending, "parser-1")
				require.NoError(t, err)
			}
			var page db.SearchPage
			if tc.probe {
				_, err = h.ActivityReportSourceProbe(t.Context())
			} else {
				page, err = h.Search(t.Context(), db.SearchFilter{Query: "needle", Limit: 20})
			}
			after, revisionErr := h.revision(t.Context())
			require.NoError(t, revisionErr)
			assert.Equal(t, before.Identity, after.Identity)
			assert.Equal(t, before.Corpus, after.Corpus)
			assert.Equal(t, before.Selection+int64(reads), after.Selection)
			t.Logf("reads=%d; before=%+v; after=%+v", reads, before, after)
			if tc.probe {
				assert.ErrorIs(t, err, ErrHostedIdentityChanged)
				assert.Equal(t, 3, reads)
			} else {
				if tc.deny {
					var denied *pgconn.PgError
					require.ErrorAs(t, err, &denied)
					assert.Equal(t, "42501", denied.Code)
				} else {
					require.NoError(t, err)
					require.Len(t, page.Results, 1)
					assert.Equal(t, "codex:portable", page.Results[0].SessionID)
					assert.Contains(t, page.Results[0].Snippet, "published needle")
				}
				assert.Equal(t, 1, reads)
			}
		})
	}
}
