package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSyncStats_RecordSkip(t *testing.T) {
	tests := []struct {
		name  string
		skips int
		want  int
	}{
		{"zero skips", 0, 0},
		{"multiple skips", 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s SyncStats
			for i := 0; i < tt.skips; i++ {
				s.RecordSkip()
			}
			assert.Equal(t, tt.want, s.Skipped)
			assert.Equal(t, 0, s.Synced)
		})
	}
}

func TestSyncStats_RecordSynced(t *testing.T) {
	tests := []struct {
		name   string
		synced []int
		want   int
	}{
		{"zero synced", []int{}, 0},
		{"multiple synced", []int{5, 3}, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s SyncStats
			for _, v := range tt.synced {
				s.RecordSynced(v)
			}
			assert.Equal(t, 0, s.Skipped)
			assert.Equal(t, tt.want, s.Synced)
		})
	}
}

func TestAnomalyStats_RecordMalformedLines(t *testing.T) {
	tests := []struct {
		name    string
		records []struct {
			agent string
			n     int
		}
		wantByAgent map[string]int
		wantTotal   int
	}{
		{
			name:        "clean run records nothing",
			wantByAgent: nil,
			wantTotal:   0,
		},
		{
			name: "zero and negative counts are ignored",
			records: []struct {
				agent string
				n     int
			}{
				{"claude", 0},
				{"codex", -3},
			},
			wantByAgent: nil,
			wantTotal:   0,
		},
		{
			name: "per-agent breakdown plus grand total",
			records: []struct {
				agent string
				n     int
			}{
				{"claude", 2},
				{"codex", 5},
				{"claude", 3},
			},
			wantByAgent: map[string]int{"claude": 5, "codex": 5},
			wantTotal:   10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a AnomalyStats
			for _, r := range tt.records {
				a.RecordMalformedLines(r.agent, r.n)
			}
			assert.Equal(t, tt.wantByAgent, a.MalformedLinesByAgent)
			assert.Equal(t, tt.wantTotal, a.MalformedLinesTotal)
		})
	}
}

func TestAnomalyAccumulator_Aggregate(t *testing.T) {
	var acc anomalyAccumulator
	acc.reset()

	// Two sessions of one agent, one of another, plus sanitize fixes
	// from messages and usage events across the run.
	acc.recordMalformedLines("claude", 4)
	acc.recordMalformedLines("codex", 1)
	acc.recordMalformedLines("claude", 2)
	acc.recordMalformedLines("gemini", 0) // ignored

	acc.recordSanitize(validationStats{ControlCharsStripped: 3, RoleCoerced: 1})
	acc.recordSanitize(validationStats{ModelClamped: 2, TokensClamped: 5})
	acc.recordSanitize(validationStats{}) // no-op

	var stats SyncStats
	acc.applyTo(&stats)

	assert.Equal(t, 7, stats.Anomalies.MalformedLinesTotal)
	assert.Equal(t, map[string]int{"claude": 6, "codex": 1},
		stats.Anomalies.MalformedLinesByAgent)
	assert.Equal(t, SanitizeStats{
		ControlCharsStripped: 3,
		ModelClamped:         2,
		TokensClamped:        5,
		RoleCoerced:          1,
	}, stats.Anomalies.Sanitize)
	assert.Equal(t, 11, stats.Anomalies.Sanitize.Total())
	assert.False(t, stats.Anomalies.IsZero())
}

func TestAnomalyAccumulator_ResetClears(t *testing.T) {
	var acc anomalyAccumulator
	acc.recordMalformedLines("claude", 9)
	acc.recordSanitize(validationStats{TimestampsBlanked: 1})
	acc.reset()

	var stats SyncStats
	acc.applyTo(&stats)
	assert.True(t, stats.Anomalies.IsZero())
	assert.Zero(t, stats.Anomalies.MalformedLinesTotal)
	assert.Nil(t, stats.Anomalies.MalformedLinesByAgent)
}

func TestAnomalyStats_IsZero(t *testing.T) {
	tests := []struct {
		name string
		a    AnomalyStats
		want bool
	}{
		{"empty", AnomalyStats{}, true},
		{
			name: "malformed only",
			a:    AnomalyStats{MalformedLinesTotal: 1},
			want: false,
		},
		{
			name: "sanitize only",
			a:    AnomalyStats{Sanitize: SanitizeStats{ModelClamped: 1}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.a.IsZero())
		})
	}
}

func TestProgress_Percent(t *testing.T) {
	tests := []struct {
		name string
		p    Progress
		want float64
	}{
		{
			name: "zero total",
			p:    Progress{SessionsTotal: 0, SessionsDone: 0},
			want: 0,
		},
		{
			name: "half done",
			p:    Progress{SessionsTotal: 10, SessionsDone: 5},
			want: 50,
		},
		{
			name: "all done",
			p:    Progress{SessionsTotal: 4, SessionsDone: 4},
			want: 100,
		},
		{
			name: "one third",
			p:    Progress{SessionsTotal: 3, SessionsDone: 1},
			want: 33.333333,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.Percent()
			assert.InDelta(t, tt.want, got, 1e-4)
		})
	}
}
