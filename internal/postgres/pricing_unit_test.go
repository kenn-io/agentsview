package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

type pricingProbeDriver struct{}

type pricingProbeConn struct {
	state *pricingProbeState
}

type pricingProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type pricingProbeState struct {
	mu      sync.Mutex
	queries int
	rows    [][]driver.Value
}

var (
	pricingProbeRegisterOnce sync.Once
	pricingProbeStatesMu     sync.Mutex
	pricingProbeStates       = map[string]*pricingProbeState{}
)

func newPricingProbeDB(
	t *testing.T, state *pricingProbeState,
) *sql.DB {
	t.Helper()
	pricingProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_pricing_probe", pricingProbeDriver{})
	})
	name := t.Name()
	pricingProbeStatesMu.Lock()
	pricingProbeStates[name] = state
	pricingProbeStatesMu.Unlock()
	t.Cleanup(func() {
		pricingProbeStatesMu.Lock()
		delete(pricingProbeStates, name)
		pricingProbeStatesMu.Unlock()
	})

	pg, err := sql.Open("agentsview_pricing_probe", name)
	require.NoError(t, err, "open pricing probe db")
	t.Cleanup(func() { pg.Close() })
	return pg
}

func (pricingProbeDriver) Open(name string) (driver.Conn, error) {
	pricingProbeStatesMu.Lock()
	state := pricingProbeStates[name]
	pricingProbeStatesMu.Unlock()
	return &pricingProbeConn{state: state}, nil
}

func (c *pricingProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *pricingProbeConn) Close() error { return nil }

func (c *pricingProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (c *pricingProbeConn) QueryContext(
	_ context.Context, _ string, _ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.queries++
	values := append([][]driver.Value(nil), c.state.rows...)
	c.state.mu.Unlock()
	return &pricingProbeRows{
		columns: []string{
			"model_pattern", "input_microdollars_per_mtok",
			"output_microdollars_per_mtok",
			"cache_creation_microdollars_per_mtok",
			"cache_read_microdollars_per_mtok", "updated_at",
			"above_input_tokens", "band_input_microdollars_per_mtok",
			"band_output_microdollars_per_mtok",
			"band_cache_creation_microdollars_per_mtok",
			"band_cache_read_microdollars_per_mtok", "band_updated_at",
		},
		values: values,
	}, nil
}

func (r *pricingProbeRows) Columns() []string { return r.columns }

func (r *pricingProbeRows) Close() error { return nil }

func (r *pricingProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func (s *pricingProbeState) queryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries
}

func TestPGPricingFilterMatchesUpsertSemantics(t *testing.T) {
	existing := []db.ModelPricing{
		{
			ModelPattern:         "same-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:00:00Z",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:00:00Z",
		},
	}
	desired := []db.ModelPricing{
		{
			ModelPattern:         "same-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:01:00Z",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("9"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:01:00Z",
		},
		{
			ModelPattern:         "missing-model",
			InputPerMTok:         money.MustParseDollars("5"),
			OutputPerMTok:        money.MustParseDollars("6"),
			CacheCreationPerMTok: money.MustParseDollars("7"),
			CacheReadPerMTok:     money.MustParseDollars("8"),
			UpdatedAt:            "2026-08-05T12:01:00Z",
		},
	}

	got, changedRows := db.FilterChangedModelPricing(existing, desired)

	assert.Equal(t, db.PricingChangeSummary{
		Total:     3,
		Missing:   1,
		Changed:   1,
		Unchanged: 1,
	}, got)
	require.Len(t, changedRows, 2)
	assert.Equal(t, "changed-model", changedRows[0].ModelPattern)
	assert.Equal(t, "missing-model", changedRows[1].ModelPattern)
}

func TestSyncModelPricingSkipsWriteWhenRemoteRowsUnchanged(t *testing.T) {
	ctx := context.Background()
	local, err := db.Open(t.TempDir() + "/local.db")
	require.NoError(t, err, "open local db")
	t.Cleanup(func() { local.Close() })
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "same-model",
		InputPerMTok:         money.MustParseDollars("1"),
		OutputPerMTok:        money.MustParseDollars("2"),
		CacheCreationPerMTok: money.MustParseDollars("3"),
		CacheReadPerMTok:     money.MustParseDollars("4"),
	}}), "seed local pricing")

	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"same-model", int64(1000000), int64(2000000), int64(3000000), int64(4000000), "old",
		}},
	}
	pg := newPricingProbeDB(t, state)
	sync := &Sync{pg: pg, local: local}

	require.NoError(t, sync.syncModelPricing(ctx))
	assert.Equal(t, 1, state.queryCount(), "pg pricing reads")
}
