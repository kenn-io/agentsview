package sync_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type pausedImportProvider struct {
	parser.Provider
	parses  atomic.Int32
	pauseAt int32
	paused  chan struct{}
	resume  chan struct{}
}

func (p *pausedImportProvider) DiscoverEach(ctx context.Context, yield func(parser.SourceRef) error) error {
	return p.Provider.(parser.StreamingDiscoverer).DiscoverEach(ctx, yield)
}

func (p *pausedImportProvider) Parse(ctx context.Context, req parser.ParseRequest) (parser.ParseOutcome, error) {
	if p.parses.Add(1) == p.pauseAt {
		close(p.paused)
		select {
		case <-p.resume:
		case <-ctx.Done():
			return parser.ParseOutcome{}, ctx.Err()
		}
	}
	return p.Provider.Parse(ctx, req)
}

type pausedImportFactory struct {
	parser.ProviderFactory
	provider *pausedImportProvider
}

func (f pausedImportFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func TestFirstImportWarpPublishesNewestFirst(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentWarp)
	env.engine.Close()
	warp := createWarpDB(t, env.warpDir)
	warp.addConversation(t, "older", "2026-04-07 10:00:00", "older message")
	warp.addConversation(t, "middle", "2026-04-07 11:00:00", "middle message")
	warp.addConversation(t, "newest", "2026-04-07 12:00:00", "newest message")
	factory, ok := parser.ProviderFactoryByType(parser.AgentWarp)
	require.True(t, ok)
	provider := &pausedImportProvider{
		Provider: factory.NewProvider(parser.ProviderConfig{Roots: []string{env.warpDir}, Machine: "local"}),
		pauseAt:  2, paused: make(chan struct{}), resume: make(chan struct{}),
	}
	engine := syncpkg.NewEngine(t.Context(), env.db, syncpkg.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentWarp: {env.warpDir}}, Machine: "local",
		ProviderFactories: []parser.ProviderFactory{pausedImportFactory{ProviderFactory: factory, provider: provider}},
	})
	t.Cleanup(engine.Close)
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.SyncNewest(t.Context(), 100, nil)
	}()
	select {
	case <-provider.paused:
	case <-time.After(30 * time.Second):
		close(provider.resume)
		<-done
		require.FailNow(t, "second source did not begin parsing")
	}
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	close(provider.resume)
	<-done
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	require.Equal(t, "warp:newest", page.Sessions[0].ID)
}

func TestFirstImportMixedSourcesPublishNewestFirst(t *testing.T) {
	for _, warpNewer := range []bool{true, false} {
		for _, limit := range []int{1, 100} {
			t.Run(fmt.Sprintf("warp-newer-%t-limit-%d", warpNewer, limit), func(t *testing.T) {
				env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentWarp)
				env.engine.Close()
				warp := createWarpDB(t, env.warpDir)
				warp.addConversation(t, "warp-session", "2026-04-07 12:00:00", "warp message")
				path := env.writeClaudeSession(t, "project", "claude-session.jsonl", testjsonl.NewSessionBuilder().AddClaudeUser(tsEarly, "claude message").String())
				mtime := time.Date(2026, 4, 7, 11, 0, 0, 0, time.UTC)
				pausedAgent, pausedRoot, want := parser.AgentClaude, env.claudeDir, "warp:warp-session"
				if !warpNewer {
					mtime = mtime.Add(2 * time.Hour)
					pausedAgent, pausedRoot, want = parser.AgentWarp, env.warpDir, "claude-session"
				}
				require.NoError(t, os.Chtimes(path, mtime, mtime))
				factory, ok := parser.ProviderFactoryByType(pausedAgent)
				require.True(t, ok)
				provider := &pausedImportProvider{
					Provider: factory.NewProvider(parser.ProviderConfig{Roots: []string{pausedRoot}, Machine: "local"}),
					pauseAt:  1, paused: make(chan struct{}), resume: make(chan struct{}),
				}
				otherAgent := parser.AgentWarp
				if pausedAgent == parser.AgentWarp {
					otherAgent = parser.AgentClaude
				}
				otherFactory, ok := parser.ProviderFactoryByType(otherAgent)
				require.True(t, ok)
				engine := syncpkg.NewEngine(t.Context(), env.db, syncpkg.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {env.claudeDir}, parser.AgentWarp: {env.warpDir}}, Machine: "local",
					ProviderFactories: []parser.ProviderFactory{pausedImportFactory{ProviderFactory: factory, provider: provider}, otherFactory},
				})
				t.Cleanup(engine.Close)
				done := make(chan struct{})
				go func() {
					defer close(done)
					engine.SyncNewest(t.Context(), limit, nil)
				}()
				if limit == 1 {
					select {
					case <-done:
					case <-time.After(30 * time.Second):
						close(provider.resume)
						<-done
						require.FailNow(t, "limited wave did not finish")
					}
				} else {
					select {
					case <-provider.paused:
					case <-time.After(30 * time.Second):
						close(provider.resume)
						<-done
						require.FailNow(t, "older source did not begin parsing")
					}
				}
				page, err := env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
				close(provider.resume)
				if limit != 1 {
					<-done
				}
				require.NoError(t, err)
				require.Len(t, page.Sessions, 1)
				require.Equal(t, want, page.Sessions[0].ID)
			})
		}
	}
}
