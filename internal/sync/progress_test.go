package sync

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestAnomalyStats_RecordUnknownSchemaSessions(t *testing.T) {
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
				{"antigravity", 0},
				{"antigravity-cli", -2},
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
				{"antigravity", 1},
				{"antigravity-cli", 2},
				{"antigravity", 1},
			},
			wantByAgent: map[string]int{"antigravity": 2, "antigravity-cli": 2},
			wantTotal:   4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a AnomalyStats
			for _, r := range tt.records {
				a.RecordUnknownSchemaSessions(r.agent, r.n)
			}
			assert.Equal(t, tt.wantByAgent, a.UnknownSchemaSessionsByAgent)
			assert.Equal(t, tt.wantTotal, a.UnknownSchemaSessionsTotal)
		})
	}
}

// TestAnomalyAccumulator_RecordUnknownSchemaSession verifies whole-session
// accumulation per agent (no per-source dedup) and that a reset clears it.
func TestAnomalyAccumulator_RecordUnknownSchemaSession(t *testing.T) {
	var acc anomalyAccumulator
	acc.reset()

	acc.recordUnknownSchemaSession("antigravity")
	acc.recordUnknownSchemaSession("antigravity")
	acc.recordUnknownSchemaSession("antigravity-cli")

	var stats SyncStats
	acc.applyTo(&stats)
	assert.Equal(t, 3, stats.Anomalies.UnknownSchemaSessionsTotal)
	assert.Equal(t, map[string]int{"antigravity": 2, "antigravity-cli": 1},
		stats.Anomalies.UnknownSchemaSessionsByAgent)
	assert.False(t, stats.Anomalies.IsZero())

	// A fresh run starts from zero.
	acc.reset()
	var next SyncStats
	acc.applyTo(&next)
	assert.True(t, next.Anomalies.IsZero())
	assert.Zero(t, next.Anomalies.UnknownSchemaSessionsTotal)
	assert.Nil(t, next.Anomalies.UnknownSchemaSessionsByAgent)
}

func TestAnomalyAccumulator_Aggregate(t *testing.T) {
	var acc anomalyAccumulator
	acc.reset()

	// Two sessions of one agent, one of another, plus sanitize fixes
	// from messages and usage events across the run.
	acc.recordMalformedLines("claude", "a.jsonl", 4)
	acc.recordMalformedLines("codex", "b.jsonl", 1)
	acc.recordMalformedLines("claude", "c.jsonl", 2)
	acc.recordMalformedLines("gemini", "d.jsonl", 0) // ignored

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

// TestAnomalyAccumulator_DedupesMalformedLinesPerSourceFile verifies a source
// file that forks into several sessions counts its malformed lines once, while
// distinct files and empty-path (DB-backed) sessions still accumulate.
func TestAnomalyAccumulator_DedupesMalformedLinesPerSourceFile(t *testing.T) {
	var acc anomalyAccumulator
	acc.reset()

	// Three forked sessions of one Claude file all carry the same count.
	acc.recordMalformedLines("claude", "fork.jsonl", 2)
	acc.recordMalformedLines("claude", "fork.jsonl", 2)
	acc.recordMalformedLines("claude", "fork.jsonl", 2)
	// A different source file counts independently.
	acc.recordMalformedLines("claude", "other.jsonl", 5)
	// Empty path (DB-backed agent) is not deduped.
	acc.recordMalformedLines("warp", "", 1)
	acc.recordMalformedLines("warp", "", 1)

	var stats SyncStats
	acc.applyTo(&stats)
	assert.Equal(t, 2+5, stats.Anomalies.MalformedLinesByAgent["claude"],
		"forked file counted once; distinct file added")
	assert.Equal(t, 2, stats.Anomalies.MalformedLinesByAgent["warp"],
		"empty-path sessions are not deduped")
	assert.Equal(t, 9, stats.Anomalies.MalformedLinesTotal)
}

func TestAnomalyAccumulator_ResetClears(t *testing.T) {
	var acc anomalyAccumulator
	acc.recordMalformedLines("claude", "x.jsonl", 9)
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
			name: "unknown schema only",
			a:    AnomalyStats{UnknownSchemaSessionsTotal: 1},
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

// TestSyncStatsJSONOmitsZeroAnomalies verifies the anomaly JSON fields use
// omitzero semantics: a clean run emits no "anomalies" object at all, and a
// run with only malformed-line counts omits the empty nested "sanitize"
// object. Plain omitempty cannot do this for struct-valued fields.
func TestSyncStatsJSONOmitsZeroAnomalies(t *testing.T) {
	clean, err := json.Marshal(SyncStats{Synced: 3})
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "anomalies",
		"a clean run must not emit an empty anomalies object")

	malformedOnly, err := json.Marshal(SyncStats{
		Anomalies: AnomalyStats{
			MalformedLinesByAgent: map[string]int{"claude": 2},
			MalformedLinesTotal:   2,
		},
	})
	require.NoError(t, err)
	got := string(malformedOnly)
	assert.Contains(t, got, "malformed_lines_total",
		"malformed counts must still serialize")
	assert.NotContains(t, got, "sanitize",
		"malformed-only run must not emit an empty sanitize object")
}
