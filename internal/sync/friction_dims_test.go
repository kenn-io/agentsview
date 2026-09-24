package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/nanoclaw"
	"go.kenn.io/agentsview/internal/nanoclaw/nanoclawtest"
)

func TestNewFrictionDims(t *testing.T) {
	data := t.TempDir()
	nanoclawtest.WriteCell(t, data)
	nanoclawtest.Exec(t, filepath.Join(data, "v2.db"),
		"UPDATE messaging_groups SET name = 'gen`eral\nroom' WHERE id = 'mg-1'")
	seats, err := friction.CompileSeatPatterns([]string{"*/v2-sessions/{seat}/*"})
	require.NoError(t, err)
	cell := func(agent, session string) string { return nanoclawtest.SessionPath(data, agent, session) }
	plain := filepath.Join(t.TempDir(), "projects", "p", "s.jsonl")
	resolver := func(f nanoclaw.Filter) *nanoclaw.Resolver { return nanoclaw.NewResolver(data, "", f) }

	assert.Nil(t, NewFrictionDims(nil, nil), "nothing configured: no hook, so no dims row")

	tests := []struct {
		name     string
		resolver *nanoclaw.Resolver
		seats    []friction.SeatPattern
		path     string
		want     db.FrictionSessionDims
		ok       bool
	}{
		{"plain_session_with_filtered_nanoclaw", resolver(nanoclaw.Filter{Exclude: []string{"reviewer"}}), nil, plain, db.FrictionSessionDims{}, false},
		{"no_file_path", resolver(nanoclaw.Filter{}), seats, "", db.FrictionSessionDims{}, false},
		{
			"nanoclaw_persona_sanitized", resolver(nanoclaw.Filter{}), nil, cell("ag-1", "s-1"),
			db.FrictionSessionDims{Persona: "helper", Channel: "gen'eral room", DimsSource: "nanoclaw"},
			true,
		},
		{
			"nanoclaw_excluded_has_no_names", resolver(nanoclaw.Filter{Exclude: []string{"reviewer"}}), nil, cell("ag-2", "s-2"),
			db.FrictionSessionDims{DimsSource: "nanoclaw", ReviewExcluded: true},
			true,
		},
		{
			"nanoclaw_excluded_with_seat_stores_only_exclusion", resolver(nanoclaw.Filter{Exclude: []string{"reviewer"}}), seats, cell("ag-2", "s-2"),
			db.FrictionSessionDims{DimsSource: "nanoclaw", ReviewExcluded: true},
			true,
		},
		{
			"seat_only", nil, seats, cell("ag-3", "s-3"),
			db.FrictionSessionDims{Seat: "ag-3", DimsSource: "seat_pattern"},
			true,
		},
		{
			"both", resolver(nanoclaw.Filter{}), seats, cell("ag-2", "s-2"),
			db.FrictionSessionDims{Seat: "ag-2", Persona: "reviewer", Channel: "events team", DimsSource: "nanoclaw+seat_pattern"},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := NewFrictionDims(tt.resolver, tt.seats)
			require.NotNil(t, hook)
			s := db.Session{ID: "s"}
			if tt.path != "" {
				p := tt.path
				s.FilePath = &p
			}
			got, ok := hook(t.Context(), s)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
