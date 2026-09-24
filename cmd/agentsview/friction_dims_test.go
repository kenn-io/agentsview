package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/nanoclaw/nanoclawtest"
)

func TestNewFrictionDimsFunc(t *testing.T) {
	data := t.TempDir()
	nanoclawtest.WriteCell(t, data)
	session := func(path string) db.Session { return db.Session{ID: "s", FilePath: &path} }

	t.Run("nothing_configured", func(t *testing.T) {
		assert.Nil(t, newFrictionDimsFunc(config.Config{}))
		assert.Nil(t, newNanoClawResolver(config.Config{}))
	})

	t.Run("seat_patterns_only", func(t *testing.T) {
		var cfg config.Config
		cfg.Friction.SeatPatterns = []string{"*/v2-sessions/{seat}/*"}
		hook := newFrictionDimsFunc(cfg)
		require.NotNil(t, hook)
		d, ok := hook(t.Context(), session(nanoclawtest.SessionPath(data, "ag-1", "s-1")))
		assert.True(t, ok)
		assert.Equal(t, db.FrictionSessionDims{Seat: "ag-1", DimsSource: "seat_pattern"}, d)
	})

	t.Run("nanoclaw_with_filter", func(t *testing.T) {
		var cfg config.Config
		cfg.Friction.NanoClaw = config.FrictionNanoClawConfig{DataDir: data, Exclude: []string{"reviewer"}}
		hook := newFrictionDimsFunc(cfg)
		require.NotNil(t, hook)
		d, ok := hook(t.Context(), session(nanoclawtest.SessionPath(data, "ag-2", "s-2")))
		assert.True(t, ok)
		assert.True(t, d.ReviewExcluded)
		d, _ = hook(t.Context(), session(nanoclawtest.SessionPath(data, "ag-1", "s-1")))
		assert.Equal(t, "helper", d.Persona)
	})

	t.Run("nanoclaw_custom_db", func(t *testing.T) {
		var cfg config.Config
		cfg.Friction.NanoClaw = config.FrictionNanoClawConfig{DataDir: data, DB: filepath.Join(data, "v2.db")}
		assert.NotNil(t, newNanoClawResolver(cfg))
	})
}
