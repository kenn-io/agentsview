package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func writeFirstImportClaudeSession(t *testing.T, project string, i int, mtime time.Time) {
	t.Helper()
	path := filepath.Join(project, fmt.Sprintf("session-%03d.jsonl", i))
	content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	if !mtime.IsZero() {
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}
}

func TestFirstImportSelectsZCodeBeforeOlderFile(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	writeFirstImportClaudeSession(t, project, 0, time.Unix(1, 0))
	zcode := filepath.Join(t.TempDir(), ".zcode", "cli")
	writeProcessProviderZCodeDB(t, zcode)
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}, parser.AgentZCode: {zcode}}, Machine: "local", DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	engine.syncNewest(t.Context(), nil, nil)
	require.True(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
	newest, err := archive.GetSession(t.Context(), "zcode:session-001")
	require.NoError(t, err)
	require.NotNil(t, newest)
	older, err := archive.GetSession(t.Context(), "session-000")
	require.NoError(t, err)
	require.Nil(t, older)
}

func TestFirstImportStagedFailureLeavesNoSession(t *testing.T) {
	root := t.TempDir()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122c80"
	path := filepath.Join(root, "2026", "08", "01", "rollout-2026-08-01T10-00-00-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	source := fmt.Sprintf("{\"type\":\"session_meta\",\"timestamp\":\"2026-08-01T10:00:00Z\",\"payload\":{\"id\":%q,\"cwd\":%q,\"originator\":\"codex_cli_rs\"}}\n", id, root) +
		`{"type":"response_item","timestamp":"2026-08-01T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Check this code"}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	archive := openTestDB(t)
	raw, err := sql.Open("sqlite3", archive.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER fail_message BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'blocked message'); END`)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DeferStartupMaintenance: true, StagedCodexParseMinBytes: 1, CodexStagingDir: t.TempDir(),
	})
	t.Cleanup(engine.Close)
	stats := engine.syncNewest(t.Context(), nil, nil)
	require.Positive(t, stats.Failed)
	session, err := archive.GetSession(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.Nil(t, session)
}

func TestFirstImportRefreshesWhileOlderSessionsAreWritten(t *testing.T) {
	for _, tc := range []struct {
		name           string
		count, pauseAt int
		cancel         bool
	}{
		{"resume", 250, 200, false},
		{"cancel", 450, 400, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			require.NoError(t, os.MkdirAll(project, 0o755))
			for i := range tc.count {
				writeFirstImportClaudeSession(t, project, i, time.Unix(1700000000+int64(i), 0))
			}
			archive := openTestDB(t)
			refresh := make(chan struct{}, 1)
			releaseRefresh := make(chan struct{})
			paused := make(chan struct{}, 1)
			resume := make(chan struct{})
			var fallback atomic.Bool
			var events atomic.Int32
			engine := NewEngine(t.Context(), archive, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", DeferStartupMaintenance: true,
				Emitter: emitterFunc(func(scope string) {
					if scope != "sessions" || !fallback.Load() {
						return
					}
					if tc.cancel {
						if events.Add(1) == 1 {
							refresh <- struct{}{}
							<-releaseRefresh
						}
					} else {
						page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
						if err == nil && len(page.Sessions) > 100 && events.CompareAndSwap(0, 1) {
							refresh <- struct{}{}
						}
					}
				}),
			})
			t.Cleanup(engine.Close)
			engine.workerCountOverride = 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := engine.RunFirstImport(ctx, func(progress Progress) {
					if progress.Phase == PhaseSyncing && progress.SessionsTotal == tc.count {
						fallback.Store(true)
					}
					if progress.Phase == PhaseSyncing && progress.SessionsDone == tc.pauseAt {
						paused <- struct{}{}
						<-resume
					}
				})
				done <- err
			}()
			finished := false
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRefresh); close(resume) }) }
			t.Cleanup(func() {
				release()
				if !finished {
					<-done
				}
			})
			select {
			case <-refresh:
			case <-time.After(30 * time.Second):
				require.FailNow(t, "no refresh while importing older sessions")
			}
			select {
			case <-paused:
			case <-time.After(30 * time.Second):
				require.FailNow(t, "import did not reach the pause")
			}
			if tc.cancel {
				cancel()
			} else {
				page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
				require.NoError(t, err)
				assert.Greater(t, len(page.Sessions), 100)
				assert.Less(t, len(page.Sessions), tc.count)
				newer, err := archive.GetSession(t.Context(), "session-149")
				require.NoError(t, err)
				older, err := archive.GetSession(t.Context(), "session-000")
				require.NoError(t, err)
				require.NotNil(t, newer)
				require.Nil(t, older)
			}
			release()
			if tc.cancel {
				select {
				case <-done:
					finished = true
				case <-time.After(500 * time.Millisecond): //nolint:kennlint // deadlinetest: cancellation must finish within the deadline after the paused import and emitter are released
					<-done
					finished = true
					require.FailNow(t, "pending refresh delayed cancellation")
				}
			} else {
				select {
				case err := <-done:
					finished = true
					require.NoError(t, err)
				case <-time.After(30 * time.Second):
					require.FailNow(t, "import did not finish")
				}
			}
		})
	}
}

type firstImportFailingProvider struct {
	parser.Provider
}

func (p *firstImportFailingProvider) Capabilities() parser.Capabilities {
	caps := p.Provider.Capabilities()
	caps.Source.StreamingDiscovery = parser.CapabilitySupported
	return caps
}

func (p *firstImportFailingProvider) DiscoverEach(context.Context, func(parser.SourceRef) error) error {
	return errors.New("listing unavailable")
}

type firstImportFailingFactory struct{ provider *firstImportFailingProvider }

func (f firstImportFailingFactory) Definition() parser.AgentDef { return f.provider.Definition() }

func (f firstImportFailingFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f firstImportFailingFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func TestFirstImportNewestWaveSurvivesListingFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "healthy.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()), 0o600))
	writeFirstImportClaudeSession(t, filepath.Dir(path), 0, time.Unix(1, 0))
	require.NoError(t, os.Chtimes(path, time.Unix(2, 0), time.Unix(2, 0)))
	archive := openTestDB(t)
	warp, ok := parser.ProviderFactoryByType(parser.AgentWarp)
	require.True(t, ok)
	provider := &firstImportFailingProvider{warp.NewProvider(parser.ProviderConfig{})}
	claude, ok := parser.ProviderFactoryByType(parser.AgentClaude)
	require.True(t, ok)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}, parser.AgentWarp: {t.TempDir()}}, Machine: "local", DeferStartupMaintenance: true,
		ProviderFactories: []parser.ProviderFactory{claude, firstImportFailingFactory{provider}},
	})
	t.Cleanup(engine.Close)

	engine.syncNewest(t.Context(), nil, nil)
	healthy, err := archive.GetSession(t.Context(), "healthy")
	require.NoError(t, err)
	require.NotNil(t, healthy)
	older, err := archive.GetSession(t.Context(), "session-000")
	require.NoError(t, err)
	require.Nil(t, older)
}
