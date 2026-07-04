package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
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
	mu               sync.Mutex
	doneOnce         sync.Once
	queries          int
	err              error
	rows             [][]driver.Value
	block            <-chan struct{}
	afterCancelBlock <-chan struct{}
	done             chan struct{}
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
	ctx context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	defer func() {
		if c.state.done != nil {
			c.state.doneOnce.Do(func() { close(c.state.done) })
		}
	}()
	c.state.mu.Lock()
	c.state.queries++
	err := c.state.err
	values := append([][]driver.Value(nil), c.state.rows...)
	block := c.state.block
	afterCancelBlock := c.state.afterCancelBlock
	c.state.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			if afterCancelBlock != nil {
				<-afterCancelBlock
			}
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &pricingProbeRows{
		columns: []string{
			"model_pattern",
			"input_per_mtok",
			"output_per_mtok",
			"cache_creation_per_mtok",
			"cache_read_per_mtok",
			"updated_at",
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

func (s *pricingProbeState) setRows(rows [][]driver.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = rows
}

func (s *pricingProbeState) unblockNextQuery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.block = nil
	s.afterCancelBlock = nil
}

func TestCustomPricingOverridesPricingMap(t *testing.T) {
	fallback := fallbackPricingMap()
	tests := []struct {
		name       string
		dbPrices   []db.ModelPricing
		custom     map[string]config.CustomModelRate
		model      string
		wantInput  float64
		wantSource export.PricingRowSource
	}{
		{
			name:       "db pricing only",
			dbPrices:   []db.ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0}},
			model:      "acme-ultra-2.1",
			wantInput:  1.0,
			wantSource: export.PricingRowSourceFetched,
		},
		{
			name:       "custom overrides db",
			dbPrices:   []db.ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0}},
			custom:     map[string]config.CustomModelRate{"acme-ultra-2.1": {Input: 9.0}},
			model:      "acme-ultra-2.1",
			wantInput:  9.0,
			wantSource: export.PricingRowSourceCustom,
		},
		{
			name:       "custom adds new model",
			custom:     map[string]config.CustomModelRate{"new-model": {Input: 4.0}},
			model:      "new-model",
			wantInput:  4.0,
			wantSource: export.PricingRowSourceCustom,
		},
		{
			name: "custom keeps source when rates match fallback",
			custom: map[string]config.CustomModelRate{
				"gpt-5.5": {
					Input:         fallback["gpt-5.5"].InputPerMTok,
					Output:        fallback["gpt-5.5"].OutputPerMTok,
					CacheCreation: fallback["gpt-5.5"].CacheWritePerMTok,
					CacheRead:     fallback["gpt-5.5"].CacheReadPerMTok,
				},
			},
			model:      "gpt-5.5",
			wantInput:  fallback["gpt-5.5"].InputPerMTok,
			wantSource: export.PricingRowSourceCustom,
		},
		{
			name:       "custom does not affect other models",
			dbPrices:   []db.ModelPricing{{ModelPattern: "db-model", InputPerMTok: 2.0}},
			custom:     map[string]config.CustomModelRate{"other": {Input: 99.0}},
			model:      "db-model",
			wantInput:  2.0,
			wantSource: export.PricingRowSourceFetched,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Store{}
			s.SetCustomPricing(tt.custom)
			out := pricingRowsToMap(tt.dbPrices)
			s.applyCustomPricing(out)
			got, ok := out[tt.model]
			require.True(t, ok, "model %q not in map", tt.model)
			assert.InDelta(t, tt.wantInput, got.InputPerMTok, 0.001)
			assert.Equal(t, tt.wantSource, got.Source)
		})
	}
}

func TestLoadPricingMapSharesConcurrentDBRows(t *testing.T) {
	block := make(chan struct{})
	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"db-model", 1.0, 2.0, 3.0, 4.0, "2026-06-08",
		}},
		block: block,
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	type result struct {
		prices []export.EffectivePricingRow
		err    error
	}
	results := make(chan result, 2)
	go func() {
		prices, err := store.loadPricingMap(context.Background())
		results <- result{prices: prices, err: err}
	}()
	require.Eventually(t, func() bool {
		return state.queryCount() == 1
	}, time.Second, 10*time.Millisecond)

	go func() {
		prices, err := store.loadPricingMap(context.Background())
		results <- result{prices: prices, err: err}
	}()
	require.Never(t, func() bool {
		return state.queryCount() > 1
	}, 50*time.Millisecond, 10*time.Millisecond)
	close(block)

	first := <-results
	second := <-results
	require.NoError(t, first.err, "first loadPricingMap")
	require.NoError(t, second.err, "second loadPricingMap")
	require.Equal(t, 1, state.queryCount(), "pricing queries")
	first.prices[0].Rates.InputPerMTok = 99.0
	secondByPattern := pricingRowsByPattern(second.prices)
	assert.InDelta(t, 1.0, secondByPattern["db-model"].InputPerMTok, 0.001)
}

func TestLoadPricingMapUsesDBRowsAsEffectiveTable(t *testing.T) {
	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"db-model", 1.0, 2.0, 3.0, 4.0, "2026-06-08",
		}},
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	prices, err := store.loadPricingMap(context.Background())
	require.NoError(t, err, "loadPricingMap")

	byPattern := pricingRowsByPattern(prices)
	require.Len(t, byPattern, 1,
		"explicit DB rows should define the effective pricing table")
	assert.InDelta(t, 1.0, byPattern["db-model"].InputPerMTok, 0.001)
}

func TestLoadPricingMapKeepsSharedDBRowsForActiveCaller(t *testing.T) {
	block := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(block) }) }
	defer unblock()
	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"db-model", 1.0, 2.0, 3.0, 4.0, "2026-06-08",
		}},
		block: block,
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	type result struct {
		prices []export.EffectivePricingRow
		err    error
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan result, 1)
	go func() {
		prices, err := store.loadPricingMap(firstCtx)
		firstResult <- result{prices: prices, err: err}
	}()
	require.Eventually(t, func() bool {
		return state.queryCount() == 1
	}, time.Second, 10*time.Millisecond)

	secondResult := make(chan result, 1)
	go func() {
		prices, err := store.loadPricingMap(context.Background())
		secondResult <- result{prices: prices, err: err}
	}()
	require.Never(t, func() bool {
		return state.queryCount() > 1
	}, 50*time.Millisecond, 10*time.Millisecond)

	cancelFirst()

	first := <-firstResult
	require.ErrorIs(t, first.err, context.Canceled)

	unblock()
	second := <-secondResult
	require.NoError(t, second.err, "second loadPricingMap")
	secondByPattern := pricingRowsByPattern(second.prices)
	assert.InDelta(t, 1.0, secondByPattern["db-model"].InputPerMTok, 0.001)
	assert.Equal(t, 1, state.queryCount(), "pricing queries")
}

func TestLoadPricingMapCancelsDBRowsWithCaller(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"db-model", 1.0, 2.0, 3.0, 4.0, "2026-06-08",
		}},
		block: block,
		done:  make(chan struct{}),
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.loadPricingMap(ctx)
		result <- err
	}()
	require.Eventually(t, func() bool {
		return state.queryCount() == 1
	}, time.Second, 10*time.Millisecond)

	cancel()

	require.Eventually(t, func() bool {
		select {
		case <-state.done:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestLoadPricingMapStartsFreshLoadAfterAllWaitersCancel(t *testing.T) {
	block := make(chan struct{})
	releaseCanceledQuery := make(chan struct{})
	defer close(releaseCanceledQuery)
	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"db-model", 1.0, 2.0, 3.0, 4.0, "2026-06-08",
		}},
		block:            block,
		afterCancelBlock: releaseCanceledQuery,
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	ctx, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := store.loadPricingMap(ctx)
		firstResult <- err
	}()
	require.Eventually(t, func() bool {
		return state.queryCount() == 1
	}, time.Second, 10*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-firstResult, context.Canceled)
	state.unblockNextQuery()

	secondResult := make(chan error, 1)
	go func() {
		_, err := store.loadPricingMap(context.Background())
		secondResult <- err
	}()

	require.Eventually(t, func() bool {
		return state.queryCount() == 2
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, <-secondResult, "second loadPricingMap")
}

func TestSetCustomPricingForgetsInFlightPricingLoad(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"db-model", 1.0, 2.0, 3.0, 4.0, "2026-06-08",
		}},
		block: block,
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	type result struct {
		prices []export.EffectivePricingRow
		err    error
	}
	results := make(chan result, 2)
	go func() {
		prices, err := store.loadPricingMap(context.Background())
		results <- result{prices: prices, err: err}
	}()
	require.Eventually(t, func() bool {
		return state.queryCount() == 1
	}, time.Second, 10*time.Millisecond)

	store.SetCustomPricing(map[string]config.CustomModelRate{
		"custom-model": {Input: 9.0},
	})
	go func() {
		prices, err := store.loadPricingMap(context.Background())
		results <- result{prices: prices, err: err}
	}()

	require.Eventually(t, func() bool {
		return state.queryCount() == 2
	}, time.Second, 10*time.Millisecond)
}

func TestLoadPricingMapReloadsAfterCompletedDBRows(t *testing.T) {
	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"db-model", 1.0, 2.0, 3.0, 4.0, "2026-06-08",
		}},
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	first, err := store.loadPricingMap(context.Background())
	require.NoError(t, err, "first loadPricingMap")
	state.setRows([][]driver.Value{{
		"db-model", 7.0, 2.0, 3.0, 4.0, "2026-06-08",
	}})
	second, err := store.loadPricingMap(context.Background())
	require.NoError(t, err, "second loadPricingMap")

	require.Equal(t, 2, state.queryCount(), "pricing queries")
	firstByPattern := pricingRowsByPattern(first)
	secondByPattern := pricingRowsByPattern(second)
	assert.InDelta(t, 1.0, firstByPattern["db-model"].InputPerMTok, 0.001)
	assert.InDelta(t, 7.0, secondByPattern["db-model"].InputPerMTok, 0.001)
}

func pricingRowsByPattern(
	rows []export.EffectivePricingRow,
) map[string]export.ModelRates {
	out := make(map[string]export.ModelRates, len(rows))
	for _, row := range rows {
		out[row.ModelPattern] = row.Rates
	}
	return out
}

func TestLoadPricingMapDoesNotCacheMissingTableFallback(t *testing.T) {
	state := &pricingProbeState{
		err: errors.New(`relation "model_pricing" does not exist (SQLSTATE 42P01)`),
	}
	pg := newPricingProbeDB(t, state)
	store := &Store{pg: pg}

	_, err := store.loadPricingMap(context.Background())
	require.NoError(t, err, "first loadPricingMap")
	_, err = store.loadPricingMap(context.Background())
	require.NoError(t, err, "second loadPricingMap")

	assert.Equal(t, 2, state.queryCount(), "pricing queries")
}

func TestPGPricingUpsertStatementBatchesRows(t *testing.T) {
	query, args := pgPricingUpsertStatement([]db.ModelPricing{
		{
			ModelPattern:         "model-a",
			InputPerMTok:         1,
			OutputPerMTok:        2,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
		},
		{
			ModelPattern:         "model-b",
			InputPerMTok:         5,
			OutputPerMTok:        6,
			CacheCreationPerMTok: 7,
			CacheReadPerMTok:     8,
			UpdatedAt:            "source-time",
		},
	}, "call-time")

	assert.Contains(t, query,
		"VALUES ($1, $2, $3, $4, $5, $6), "+
			"($7, $8, $9, $10, $11, $12)")
	assert.Contains(t, query,
		"model_pricing.input_per_mtok IS DISTINCT FROM")
	assert.Contains(t, query, "EXCLUDED.input_per_mtok")
	assert.NotContains(t, query,
		"model_pricing.updated_at IS DISTINCT FROM")
	require.Len(t, args, 12)
	assert.Equal(t, "model-a", args[0])
	assert.Equal(t, "call-time", args[5])
	assert.Equal(t, "model-b", args[6])
	assert.Equal(t, "source-time", args[11])
}

func TestPGPricingFilterMatchesUpsertSemantics(t *testing.T) {
	existing := []db.ModelPricing{
		{
			ModelPattern:         "_fallback_version",
			InputPerMTok:         0,
			OutputPerMTok:        0,
			CacheCreationPerMTok: 0,
			CacheReadPerMTok:     0,
			UpdatedAt:            "v1",
		},
		{
			ModelPattern:         "same-model",
			InputPerMTok:         1,
			OutputPerMTok:        2,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "old",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         1,
			OutputPerMTok:        2,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "old",
		},
	}
	desired := []db.ModelPricing{
		{
			ModelPattern:         "_fallback_version",
			InputPerMTok:         0,
			OutputPerMTok:        0,
			CacheCreationPerMTok: 0,
			CacheReadPerMTok:     0,
			UpdatedAt:            "v2",
		},
		{
			ModelPattern:         "same-model",
			InputPerMTok:         1,
			OutputPerMTok:        2,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "new",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         1,
			OutputPerMTok:        9,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "new",
		},
		{
			ModelPattern:         "missing-model",
			InputPerMTok:         5,
			OutputPerMTok:        6,
			CacheCreationPerMTok: 7,
			CacheReadPerMTok:     8,
			UpdatedAt:            "new",
		},
	}

	got, changedRows := db.FilterChangedModelPricing(existing, desired)

	assert.Equal(t, db.PricingChangeSummary{
		Total:     4,
		Missing:   1,
		Changed:   1,
		Unchanged: 2,
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
		InputPerMTok:         1,
		OutputPerMTok:        2,
		CacheCreationPerMTok: 3,
		CacheReadPerMTok:     4,
	}}), "seed local pricing")

	state := &pricingProbeState{
		rows: [][]driver.Value{{
			"same-model", 1.0, 2.0, 3.0, 4.0, "old",
		}},
	}
	pg := newPricingProbeDB(t, state)
	sync := &Sync{pg: pg, local: local}

	require.NoError(t, sync.syncModelPricing(ctx))
	assert.Equal(t, 1, state.queryCount(), "pg pricing reads")
}
