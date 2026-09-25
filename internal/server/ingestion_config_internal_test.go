package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
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

	patch := map[string]any{"disabled_agents": nil}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			assert.NoError(t, s.applyIngestionSettings(t.Context(), patch))
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
