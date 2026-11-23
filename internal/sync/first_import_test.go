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

func TestFirstImportResumesAfterFirstWave(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	for i := range 250 {
		writeFirstImportClaudeSession(t, project, i, time.Unix(1700000000+int64(i), 0))
	}
	archive := openTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", DeferStartupMaintenance: true,
		Emitter: emitterFunc(func(scope string) {
			if scope == "sessions" {
				cancel()
			}
		}),
	})
	t.Cleanup(engine.Close)
	_, ran, err := engine.RunFirstImport(ctx, nil)
	require.True(t, ran)
	require.ErrorIs(t, err, context.Canceled)
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	require.Equal(t, "session-249", page.Sessions[0].ID)
	require.True(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
	engine.emitter = nil
	marker, err := archive.GetSyncState(t.Context(), db.FirstImportCompletedKey)
	require.NoError(t, err)
	require.Empty(t, marker)
	engine.workerCountOverride = 1
	paused, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := engine.RunFirstImport(t.Context(), func(p Progress) {
			if p.Phase == PhaseSyncing && p.SessionsTotal == 250 && p.SessionsDone == 201 {
				close(paused)
				<-resume
			}
		})
		done <- err
	}()
	finished := false
	t.Cleanup(func() {
		if !finished {
			close(resume)
			<-done
		}
	})
	select {
	case <-paused:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "reconciliation did not reach the pause")
	}
	newer, err := archive.GetSession(t.Context(), "session-149")
	require.NoError(t, err)
	older, err := archive.GetSession(t.Context(), "session-000")
	require.NoError(t, err)
	require.NotNil(t, newer)
	require.Nil(t, older)
	close(resume)
	err = <-done
	finished = true
	require.NoError(t, err)
	page, err = archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 250)
	require.False(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
}

func TestFirstImportBoundsCopilotSessionsWithSharedStore(t *testing.T) {
	for _, tied := range []bool{false, true} {
		t.Run(fmt.Sprintf("tied-%t", tied), func(t *testing.T) {
			root := t.TempDir()
			for i := range 30 {
				id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
				path := filepath.Join(root, "session-state", id, "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				content := fmt.Sprintf("{\"type\":\"session.start\",\"data\":{\"sessionId\":%q},\"timestamp\":\"2026-01-01T00:00:00Z\"}\n{\"type\":\"user.message\",\"data\":{\"content\":\"hello\"},\"timestamp\":\"2026-01-01T00:00:01Z\"}\n", id)
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
				mtime := time.Unix(1700000000, 0)
				if !tied {
					mtime = mtime.Add(time.Duration(i) * time.Second)
				}
				require.NoError(t, os.Chtimes(path, mtime, mtime))
			}
			store, err := sql.Open("sqlite3", filepath.Join(root, "session-store.db"))
			require.NoError(t, err)
			_, err = store.ExecContext(t.Context(), `CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY, session_id TEXT, model TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER, created_at TEXT)`)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			archive := openTestDB(t)
			engine := NewEngine(t.Context(), archive, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local", DeferStartupMaintenance: true,
			})
			t.Cleanup(engine.Close)
			_, complete := engine.syncNewest(t.Context(), 25, nil, nil)
			require.False(t, complete)
			page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 25)
			for i := range 30 {
				id := fmt.Sprintf("copilot:00000000-0000-4000-8000-%012d", i)
				session, err := archive.GetSession(t.Context(), id)
				require.NoError(t, err)
				if (tied && i < 25) || (!tied && i >= 5) {
					require.NotNil(t, session, id)
				} else {
					require.Nil(t, session, id)
				}
			}
		})
	}
}

type firstImportFingerprintProvider struct {
	parser.Provider
	mu    sync.Mutex
	paths []string
}

func (p *firstImportFingerprintProvider) Fingerprint(ctx context.Context, source parser.SourceRef) (parser.SourceFingerprint, error) {
	p.mu.Lock()
	p.paths = append(p.paths, source.DisplayPath)
	p.mu.Unlock()
	return p.Provider.Fingerprint(ctx, source)
}

type firstImportFingerprintFactory struct {
	parser.ProviderFactory
	provider *firstImportFingerprintProvider
}

func (f firstImportFingerprintFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func TestFirstImportNewestWave(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		known bool
		want  []string
	}{
		{"newest", 30, true, []string{"session-027", "session-028", "session-029"}},
		{"known-and-unknown", 5, false, []string{"session-000", "session-001", "session-004"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			require.NoError(t, os.MkdirAll(project, 0o755))
			for i := range tc.count {
				mtime := time.Unix(0, 0)
				if tc.known {
					mtime = time.Unix(1700000000+int64(i), 0)
				} else if tc.name == "known-and-unknown" && i == 4 {
					mtime = time.Unix(10, 0)
				}
				writeFirstImportClaudeSession(t, project, i, mtime)
			}
			factory, ok := parser.ProviderFactoryByType(parser.AgentClaude)
			require.True(t, ok)
			counting := &firstImportFingerprintProvider{Provider: factory.NewProvider(parser.ProviderConfig{Roots: []string{root}, Machine: "local"})}
			archive := openTestDB(t)
			engine := NewEngine(t.Context(), archive, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", DeferStartupMaintenance: true,
				ProviderFactories: []parser.ProviderFactory{firstImportFingerprintFactory{ProviderFactory: factory, provider: counting}},
			})
			t.Cleanup(engine.Close)
			_, complete := engine.syncNewest(t.Context(), 3, nil, nil)
			require.False(t, complete)
			page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
			require.NoError(t, err)
			var ids []string
			for _, session := range page.Sessions {
				ids = append(ids, session.ID)
			}
			require.ElementsMatch(t, tc.want, ids)
			if tc.name == "newest" {
				require.NotEmpty(t, counting.paths)
				for _, path := range counting.paths {
					require.Contains(t, []string{filepath.Join(project, "session-027.jsonl"), filepath.Join(project, "session-028.jsonl"), filepath.Join(project, "session-029.jsonl")}, path)
				}
			}
		})
	}
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
				writeFirstImportClaudeSession(t, project, i, time.Time{})
			}
			archive := openTestDB(t)
			refresh := make(chan struct{}, 1)
			releaseRefresh := make(chan struct{})
			paused := make(chan struct{}, 1)
			resume := make(chan struct{})
			var fallback atomic.Bool
			var events atomic.Int32
			var engine *Engine
			engine = NewEngine(t.Context(), archive, EngineConfig{
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
							engine.SyncAll(t.Context(), nil)
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
				if err == nil {
					for _, session := range page.Sessions {
						messages, readErr := archive.GetMessages(t.Context(), session.ID, 0, 10, true)
						if readErr != nil || len(messages) == 0 {
							require.FailNowf(t, "unreadable session", "listed session %s has no readable messages: %v", session.ID, readErr)
						}
					}
				}
				require.NoError(t, err)
				assert.Greater(t, len(page.Sessions), 100)
				assert.Less(t, len(page.Sessions), tc.count)
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
					require.FailNow(t, "reentrant emitter blocked the import")
				}
			}
		})
	}
}

type firstImportFailingProvider struct {
	parser.ProviderBase
	listingFailure bool
}

func (p *firstImportFailingProvider) Capabilities() parser.Capabilities {
	caps := p.ProviderBase.Capabilities()
	caps.Source.StreamingDiscovery = parser.CapabilitySupported
	return caps
}

func (p *firstImportFailingProvider) DiscoverEach(ctx context.Context, yield func(parser.SourceRef) error) error {
	if p.listingFailure {
		return errors.New("listing unavailable")
	}
	return yield(parser.SourceRef{DisplayPath: "broken"})
}

func (p *firstImportFailingProvider) Fingerprint(context.Context, parser.SourceRef) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{}, errors.New("source unavailable")
}

func (p *firstImportFailingProvider) Parse(context.Context, parser.ParseRequest) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

type firstImportFailingFactory struct{ provider *firstImportFailingProvider }

func (f firstImportFailingFactory) Definition() parser.AgentDef { return f.provider.Definition() }

func (f firstImportFailingFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f firstImportFailingFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func TestFirstImportProviderFailures(t *testing.T) {
	for _, listingFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("listing-failure-%t", listingFailure), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "project", "healthy.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()), 0o600))
			archive := openTestDB(t)
			provider := &firstImportFailingProvider{parser.ProviderBase{Def: parser.AgentDef{Type: parser.AgentWarp, FileBased: false}}, listingFailure}
			claude, ok := parser.ProviderFactoryByType(parser.AgentClaude)
			require.True(t, ok)
			engine := NewEngine(t.Context(), archive, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}, parser.AgentWarp: {t.TempDir()}}, Machine: "local", DeferStartupMaintenance: true,
				ProviderFactories: []parser.ProviderFactory{claude, firstImportFailingFactory{provider}},
			})
			t.Cleanup(engine.Close)

			wave, _ := engine.syncNewest(t.Context(), 100, nil, nil)
			assert.Equal(t, 1, wave.Synced)
			stats, ran, err := engine.RunFirstImport(t.Context(), nil)
			require.NoError(t, err)
			require.True(t, ran)
			assert.Positive(t, stats.Failed)
			assert.Equal(t, !listingFailure, stats.AuthoritativeDiscoveryComplete())
			marker, err := archive.GetSyncState(t.Context(), db.FirstImportCompletedKey)
			require.NoError(t, err)
			if listingFailure {
				assert.Empty(t, marker)
			} else {
				assert.Equal(t, "1", marker)
			}
		})
	}
}

func TestFirstImportSelectKeepsHealthyProviderAfterListingFailure(t *testing.T) {
	archive := openTestDB(t)
	failed := &firstImportFailingProvider{parser.ProviderBase{Def: parser.AgentDef{Type: parser.AgentWarp, FileBased: false}}, true}
	healthy := &firstImportFailingProvider{parser.ProviderBase{Def: parser.AgentDef{Type: parser.AgentForge, FileBased: false}}, false}
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentWarp: {t.TempDir()}, parser.AgentForge: {t.TempDir()}}, Machine: "local",
		ProviderFactories: []parser.ProviderFactory{firstImportFailingFactory{failed}, firstImportFailingFactory{healthy}},
	})
	t.Cleanup(engine.Close)
	wave := &firstImportWave{limit: 1}
	require.NoError(t, engine.firstImportSelect(t.Context(), nil, nil, wave))
	assert.Contains(t, wave.selected, firstImportSource{parser.AgentForge, "broken"})
}
