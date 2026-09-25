package server

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestIngestionConfigReadsOneReloadedSnapshot(t *testing.T) {
	// Each reload returns one of two configurations whose disabled providers
	// and roots belong together. A reader must never mix them.
	snapshots := []config.Config{
		{
			AgentDirs:      map[parser.AgentType][]string{parser.AgentClaude: {"/a"}},
			DisabledAgents: []parser.AgentType{parser.AgentGemini},
		},
		{
			AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {"/b"}},
		},
	}
	var reloads atomic.Int64
	s := &Server{
		ingestionReloader: func(context.Context) (config.Config, error) {
			return snapshots[reloads.Add(1)%2], nil
		},
	}
	s.cfg.AdoptSessionSources(snapshots[0])
	s.activeDisabledAgents = snapshots[0].DisabledAgents

	rollback := map[string]any{"disabled_agents": []parser.AgentType{}}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			assert.NoError(t, s.applyIngestionSettings(t.Context(), rollback))
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 200 {
				cfg := s.ingestionConfig()
				dirs := cfg.AgentDirs[parser.AgentClaude]
				if len(cfg.DisabledAgents) == 0 {
					assert.Equal(t, []string{"/b"}, dirs)
				} else {
					assert.Equal(t, []string{"/a"}, dirs)
				}
			}
		})
	}
	wg.Wait()
}

func TestProviderSettingsReconfigureOnDemandEngine(t *testing.T) {
	root := t.TempDir()
	engine := syncpkg.NewEngine(t.Context(), dbtest.OpenTestDB(t), syncpkg.EngineConfig{
		AgentDirs:      map[parser.AgentType][]string{parser.AgentGemini: {root}},
		DisabledAgents: []parser.AgentType{parser.AgentGemini},
	})
	t.Cleanup(engine.Close)
	require.Empty(t, engine.ReconciliationRootsForAgent(string(parser.AgentGemini)))

	s := &Server{
		onDemandEngine: engine,
		ingestionReloader: func(context.Context) (config.Config, error) {
			return config.Config{
				AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}},
			}, nil
		},
	}
	require.NoError(t, s.applyIngestionSettings(t.Context(),
		map[string]any{"disabled_agents": []parser.AgentType{}}))

	assert.Eventually(t, func() bool {
		return slices.Equal(
			engine.ReconciliationRootsForAgent(string(parser.AgentGemini)),
			[]string{root},
		)
	}, 5*time.Second, 10*time.Millisecond,
		"manual syncs use the saved provider selection")
}
