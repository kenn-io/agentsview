package server_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestRateLimitsAPI(t *testing.T) {
	te := setup(t)
	assert.JSONEq(t, `[]`, te.get(t, "/api/v1/rate-limits").Body.String())
	points := []parser.RateLimitSnapshot{{
		ObservedAt: time.Date(2023, 12, 24, 10, 0, 0, 0, time.UTC), LimitID: "codex",
	}}
	for i := 1; i <= 520; i++ {
		used := float64(i % 101)
		points = append(points, parser.RateLimitSnapshot{
			ObservedAt: time.Date(2024, 1, 1, 10, i, 0, 0, time.UTC),
			Ordinal:    int64(i), LimitID: "codex",
			Primary: &parser.RateLimitWindow{UsedPercent: &used},
		})
	}
	for _, machine := range []string{"machine-a", "machine-b"} {
		require.NoError(t, te.db.UpsertSession(db.Session{
			ID: machine, Agent: "codex", Machine: machine, RateLimits: points,
		}))
	}
	response := te.get(t, "/api/v1/rate-limits?machine=machine-a&since=1704103320&until=1704134340")
	assert.Equal(t, 200, response.Code)
	var series []db.RateLimitSeries
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &series))
	require.Len(t, series, 1)
	assert.Equal(t, "machine-a", series[0].Machine)
	require.Len(t, series[0].Points, 512)
	for i, point := range series[0].Points[1:] {
		assert.True(t, point.ObservedAt.After(series[0].Points[i].ObservedAt))
	}
	assert.Equal(t, new(2.0), series[0].Points[0].Primary.UsedPercent)
	assert.Equal(t, new(13.0), series[0].Points[511].Primary.UsedPercent)
	assert.Equal(t, "2024-01-01T18:40:00Z", series[0].Current.ObservedAt.Format(time.RFC3339))
	response = te.get(t, "/api/v1/rate-limits?machine=machine-a&since=1704153600&until=1704240000")
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &series))
	require.Len(t, series, 1)
	assert.Empty(t, series[0].Points)
	assert.Equal(t, "2024-01-01T18:40:00Z", series[0].Current.ObservedAt.Format(time.RFC3339))
	require.NoError(t, te.db.UpsertSession(db.Session{
		ID: "machine-a", Agent: "codex", Machine: "machine-a", RateLimits: []parser.RateLimitSnapshot{},
	}))
	assert.JSONEq(t, `[]`, te.get(t, "/api/v1/rate-limits?machine=machine-a").Body.String())
	response = te.get(t, "/api/v1/rate-limits")
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &series))
	require.Len(t, series, 1)
	assert.Equal(t, "machine-b", series[0].Machine)
}
