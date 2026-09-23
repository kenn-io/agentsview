//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrictionSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema))
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema), "idempotent")

	tests := []struct {
		table   string
		columns []string
	}{
		{"friction_findings", []string{
			"id", "session_id", "kind", "detector", "message_ordinal",
			"call_index", "tool_name", "label", "text", "evidence", "title",
			"fingerprint", "occurred_at", "seq", "rules_version", "created_at",
		}},
		{"friction_session_dims", []string{
			"session_id", "seat", "persona", "channel", "dims_source",
			"review_excluded",
		}},
		{"sessions", []string{
			"friction_count", "friction_rules_version", "friction_hash",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			for _, c := range tt.columns {
				var ok bool
				require.NoError(t, pg.QueryRowContext(ctx, `
					SELECT EXISTS (SELECT 1 FROM information_schema.columns
					WHERE table_schema = $1 AND table_name = $2
					  AND column_name = $3)`,
					schemaTestSchema, tt.table, c).Scan(&ok))
				assert.True(t, ok, "%s.%s", tt.table, c)
			}
		})
	}
}

func TestCheckSchemaCompatMissingFrictionHash(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	pg, err := Open(pgURL, "agentsview", true)
	require.NoError(t, err)
	defer pg.Close()
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, "agentsview"))
	require.NoError(t, CheckSchemaCompat(ctx, pg))
	_, err = pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN friction_hash`)
	require.NoError(t, err)
	require.Error(t, CheckSchemaCompat(ctx, pg))
}

func TestPushSchemaCurrentRequiresFrictionTables(t *testing.T) {
	for _, table := range []string{"friction_findings", "friction_session_dims"} {
		t.Run(table, func(t *testing.T) {
			pgURL := testPGURL(t)
			cleanPGSchema(t, pgURL)
			t.Cleanup(func() { cleanPGSchema(t, pgURL) })
			pg, err := Open(pgURL, "agentsview", true)
			require.NoError(t, err)
			defer pg.Close()
			ctx := context.Background()
			require.NoError(t, EnsureSchema(ctx, pg, "agentsview"))
			require.True(t, pushSchemaCurrent(ctx, pg))
			_, err = pg.ExecContext(ctx, "DROP TABLE "+table)
			require.NoError(t, err)
			assert.False(t, pushSchemaCurrent(ctx, pg),
				"an upgraded hub must run EnsureSchema before pushing friction")
			require.NoError(t, EnsureSchema(ctx, pg, "agentsview"))
			assert.True(t, pushSchemaCurrent(ctx, pg))
		})
	}
}
