package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type failingFirstImportProvider struct{ parser.Provider }

func (p failingFirstImportProvider) Parse(context.Context, parser.ParseRequest) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, errors.New("source unavailable")
}

type failingFirstImportFactory struct{ parser.ProviderFactory }

func (f failingFirstImportFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return failingFirstImportProvider{f.ProviderFactory.NewProvider(cfg)}
}

func TestFirstImportCompletionWithFailedSources(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	factory, ok := parser.ProviderFactoryByType(parser.AgentClaude)
	require.True(t, ok)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		AgentDirs: cfg.AgentDirs, Machine: "local", DeferStartupMaintenance: true,
		ProviderFactories: []parser.ProviderFactory{failingFirstImportFactory{factory}},
	})
	t.Cleanup(engine.Close)
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	ran, err := runDeferredStartupSyncFallback(t.Context(), cfg, engine, database, nil, nil, timeout, true)
	require.NoError(t, err)
	require.True(t, ran)
	require.Positive(t, engine.LastSyncStats().Failed)
	require.True(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
	first, err := isFirstImport(t.Context(), cfg)
	require.NoError(t, err)
	require.False(t, first, "a full pass with source failures should not repeat first-import startup")
}

func TestFirstImportInterruptedBeforeFullPass(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		AgentDirs: cfg.AgentDirs, Machine: "local", DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, ran, err := engine.RunFirstImport(ctx, func(progress syncpkg.Progress) {
		if progress.Phase == syncpkg.PhaseSyncing && progress.SessionsDone > 0 {
			cancel()
		}
	})
	require.True(t, ran)
	require.ErrorIs(t, err, context.Canceled)
	first, err := isFirstImport(t.Context(), cfg)
	require.NoError(t, err)
	require.True(t, first)
}

type failingDiscoveryProvider struct{ parser.Provider }

func (p failingDiscoveryProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	return nil, errors.New("listing unavailable")
}

type failingDiscoveryFactory struct{ parser.ProviderFactory }

func (f failingDiscoveryFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return failingDiscoveryProvider{f.ProviderFactory.NewProvider(cfg)}
}

func TestFirstImportRetriesFailedDiscovery(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	factory, ok := parser.ProviderFactoryByType(parser.AgentClaude)
	require.True(t, ok)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		AgentDirs: cfg.AgentDirs, Machine: "local", DeferStartupMaintenance: true,
		ProviderFactories: []parser.ProviderFactory{failingDiscoveryFactory{factory}},
	})
	t.Cleanup(engine.Close)
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	ran, err := runDeferredStartupSyncFallback(t.Context(), cfg, engine, database, nil, nil, timeout, true)
	require.NoError(t, err)
	require.True(t, ran)
	first, err := isFirstImport(t.Context(), cfg)
	require.NoError(t, err)
	require.True(t, first, "a failed listing must retry the first import")
}
