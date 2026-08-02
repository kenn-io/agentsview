package bundialect

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/feature"
)

type dialectFixture struct {
	bun.BaseModel `bun:"table:dialect_fixtures"`
	ID            int64  `bun:",pk"`
	Name          string `bun:",notnull"`
	Enabled       bool   `bun:",notnull"`
	Payload       []byte
	Metadata      json.RawMessage `bun:",type:JSON"`
	OccurredAt    time.Time       `bun:",notnull"`
}

type dialectSource struct {
	bun.BaseModel `bun:"table:dialect_sources"`
	ID            int64  `bun:",pk"`
	Name          string `bun:",notnull"`
}

type dialectValuesFixture struct {
	ID   int64
	Name string
}

func openDialectTestDB(t *testing.T) *bun.DB {
	t.Helper()

	raw, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })

	return bun.NewDB(raw, New())
}

func requireFeature(t *testing.T, db *bun.DB, flag feature.Feature) {
	t.Helper()
	assert.True(t, db.Dialect().Features().Has(flag), "dialect must advertise exercised feature")
}

func TestDialectExecutesCanonicalBunQueries(t *testing.T) {
	store := openDialectTestDB(t)
	ctx := t.Context()

	_, err := store.NewCreateTable().Model((*dialectFixture)(nil)).IfNotExists().Exec(ctx)
	require.NoError(t, err)

	want := dialectFixture{
		ID:         7,
		Name:       "O'Reilly",
		Enabled:    true,
		Payload:    []byte{0x00, 0x7f, 0xff},
		Metadata:   json.RawMessage(`{"source":"duckdb"}`),
		OccurredAt: time.Date(2026, 8, 2, 12, 30, 0, 123_000_000, time.UTC),
	}
	_, err = store.NewInsert().Model(&want).Exec(ctx)
	require.NoError(t, err)

	requireFeature(t, store, feature.InsertOnConflict)
	_, err = store.NewInsert().Model(&dialectFixture{ID: 7, Name: "updated"}).
		On("CONFLICT (id) DO UPDATE").
		Set("name = EXCLUDED.name").
		Exec(ctx)
	require.NoError(t, err)

	var got dialectFixture
	err = store.NewSelect().Model(&got).Where("id = ?", 7).Scan(ctx)
	require.NoError(t, err)
	assert.Equal(t, "updated", got.Name)
	assert.True(t, got.Enabled)
	assert.Equal(t, []byte{0x00, 0x7f, 0xff}, got.Payload)
	assert.JSONEq(t, `{"source":"duckdb"}`, string(got.Metadata))
	assert.Equal(t, want.OccurredAt, got.OccurredAt)
}

func TestDialectAdvertisedFeaturesExecute(t *testing.T) {
	store := openDialectTestDB(t)
	ctx := t.Context()

	_, err := store.NewCreateTable().Model((*dialectFixture)(nil)).Exec(ctx)
	require.NoError(t, err)
	_, err = store.NewInsert().Model(&dialectFixture{
		ID: 7, Name: "updated", Enabled: true, OccurredAt: time.Date(2026, 8, 2, 12, 30, 0, 0, time.UTC),
	}).Exec(ctx)
	require.NoError(t, err)

	t.Run("cte_with_values", func(t *testing.T) {
		requireFeature(t, store, feature.CTE)
		requireFeature(t, store, feature.WithValues)
		values := []dialectValuesFixture{{ID: 8, Name: "cte"}}

		var got dialectValuesFixture
		err := store.NewSelect().
			With("input", store.NewValues(&values)).
			TableExpr("input").
			ColumnExpr("CAST(id AS BIGINT) AS id, name").
			Scan(ctx, &got)
		require.NoError(t, err)
		assert.Equal(t, dialectValuesFixture{ID: 8, Name: "cte"}, got)
	})

	t.Run("insert_returning", func(t *testing.T) {
		requireFeature(t, store, feature.Returning)
		requireFeature(t, store, feature.InsertReturning)

		var returnedID int64
		err := store.NewInsert().Model(&dialectFixture{
			ID: 9, Name: "returning", OccurredAt: time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC),
		}).Returning("id").Scan(ctx, &returnedID)
		require.NoError(t, err)
		assert.Equal(t, int64(9), returnedID)
	})

	t.Run("delete_returning", func(t *testing.T) {
		requireFeature(t, store, feature.Returning)
		requireFeature(t, store, feature.DeleteReturning)

		var returnedName string
		err := store.NewDelete().Model((*dialectFixture)(nil)).
			Where("id = ?", 9).
			Returning("name").
			Scan(ctx, &returnedName)
		require.NoError(t, err)
		assert.Equal(t, "returning", returnedName)
	})

	t.Run("update_from", func(t *testing.T) {
		requireFeature(t, store, feature.UpdateFromTable)

		_, err := store.NewCreateTable().Model((*dialectSource)(nil)).Exec(ctx)
		require.NoError(t, err)
		_, err = store.NewInsert().Model(&dialectSource{ID: 7, Name: "from-source"}).Exec(ctx)
		require.NoError(t, err)

		_, err = store.NewUpdate().Model((*dialectFixture)(nil)).
			TableExpr("dialect_sources AS source").
			Set("name = source.name").
			Where("dialect_fixtures.id = source.id").
			Exec(ctx)
		require.NoError(t, err)

		var name string
		err = store.NewSelect().Model((*dialectFixture)(nil)).
			Column("name").Where("id = ?", 7).Scan(ctx, &name)
		require.NoError(t, err)
		assert.Equal(t, "from-source", name)
	})

	t.Run("table_and_index_if_not_exists", func(t *testing.T) {
		requireFeature(t, store, feature.TableNotExists)
		requireFeature(t, store, feature.CreateIndexIfNotExists)

		for range 2 {
			_, err := store.NewCreateTable().Model((*dialectSource)(nil)).IfNotExists().Exec(ctx)
			require.NoError(t, err)
			_, err = store.NewCreateIndex().Model((*dialectFixture)(nil)).
				Index("dialect_fixture_name_idx").
				Column("name").
				IfNotExists().
				Exec(ctx)
			require.NoError(t, err)
		}
	})

	t.Run("select_exists", func(t *testing.T) {
		requireFeature(t, store, feature.SelectExists)

		exists, err := store.NewSelect().Model((*dialectFixture)(nil)).Where("id = ?", 7).Exists(ctx)
		require.NoError(t, err)
		assert.True(t, exists)

		exists, err = store.NewSelect().Model((*dialectFixture)(nil)).Where("id = ?", 404).Exists(ctx)
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("composite_in", func(t *testing.T) {
		requireFeature(t, store, feature.CompositeIn)

		var count int
		err := store.NewSelect().Model((*dialectFixture)(nil)).
			ColumnExpr("count(*)").
			Where("(id, name) IN ?", bun.Tuple([][]any{{int64(7), "from-source"}})).
			Scan(ctx, &count)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
	})

	t.Run("transaction_rollback", func(t *testing.T) {
		sentinel := errors.New("roll back dialect fixture")
		err := store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.NewInsert().Model(&dialectFixture{
				ID: 10, Name: "rollback", OccurredAt: time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC),
			}).Exec(ctx)
			if err != nil {
				return err
			}
			return sentinel
		})
		assert.ErrorIs(t, err, sentinel)

		exists, err := store.NewSelect().Model((*dialectFixture)(nil)).Where("id = ?", 10).Exists(ctx)
		require.NoError(t, err)
		assert.False(t, exists)
	})
}
