package db

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticAppendAndDaySelection(t *testing.T) {
	d := testDB(t)
	when := time.Date(2026, 1, 1, 23, 55, 0, 123, time.UTC)
	e := DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: when}
	first, err := d.AppendDiagnostic(t.Context(), e)
	require.NoError(t, err)
	retry, err := d.AppendDiagnostic(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, first, retry)
	e.Message = "another failure"
	_, err = d.AppendDiagnostic(t.Context(), e)
	require.ErrorIs(t, err, ErrDiagnosticConflict)
	signals, err := d.DiagnosticSignalsForDate(t.Context(), "2026-01-01", time.UTC)
	require.NoError(t, err)
	require.Len(t, signals, 1)
	assert.Equal(t, "worker", signals[0].Label)
	assert.Equal(t, "launch failed", signals[0].Text)
	dates, err := d.FrictionPendingDiagnosticDates(t.Context(), "2026-01-31", time.UTC)
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-01-01"}, dates)
	e.OccurredAt = time.Now().Add(10 * time.Minute)
	e.ID = uuid.NewV7().String()
	_, err = d.AppendDiagnostic(t.Context(), e)
	require.Error(t, err)
}

func TestDiagnosticResyncAndOlderArchive(t *testing.T) {
	source := testDB(t)
	target := testDB(t)
	e := DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: time.Date(2026, 1, 1, 23, 55, 0, 0, time.UTC)}
	_, err := source.AppendDiagnostic(t.Context(), e)
	require.NoError(t, err)
	require.NoError(t, target.CopyFrictionStateFrom(t.Context(), source.Path()))
	require.NoError(t, target.CopyFrictionStateFrom(t.Context(), source.Path()))
	events, err := target.DiagnosticEventsAfter(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, e.ID, events[0].ID)
	_, err = source.getWriter().Exec(t.Context(), `DROP TABLE diagnostic_events`)
	require.NoError(t, err)
	signals, err := source.DiagnosticSignalsForDate(t.Context(), "2026-01-01", time.UTC)
	require.NoError(t, err)
	assert.Empty(t, signals)
}
