package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestFirstImportResumesAfterFirstWave(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	for i := range 250 {
		path := filepath.Join(project, fmt.Sprintf("session-%03d.jsonl", i))
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		mtime := time.Unix(1700000000+int64(i), 0)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
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
	require.Len(t, page.Sessions, 100)
	require.True(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
	engine.emitter = nil
	_, ran, err = engine.RunFirstImport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ran)
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
			_, complete := engine.SyncNewest(t.Context(), 25, nil)
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
			_, _, err = engine.RunFirstImport(t.Context(), nil)
			require.NoError(t, err)
			page, err = archive.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 30)
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

func TestFirstImportFingerprintsOnlySelectedClaudeFiles(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	for i := range 30 {
		path := filepath.Join(project, fmt.Sprintf("session-%03d.jsonl", i))
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		mtime := time.Unix(1700000000+int64(i), 0)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
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
	_, complete := engine.SyncNewest(t.Context(), 3, nil)
	require.False(t, complete)
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 3)
	require.NotEmpty(t, counting.paths)
	for _, path := range counting.paths {
		require.Contains(t, []string{
			filepath.Join(project, "session-027.jsonl"),
			filepath.Join(project, "session-028.jsonl"),
			filepath.Join(project, "session-029.jsonl"),
		}, path)
	}
}

func TestFirstImportRefreshesWhileOlderSessionsAreWritten(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	for i := range 250 {
		path := filepath.Join(project, fmt.Sprintf("session-%03d.jsonl", i))
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	archive := openTestDB(t)
	refresh := make(chan struct{}, 1)
	paused := make(chan struct{}, 1)
	resume := make(chan struct{})
	var events atomic.Int32
	var engine *Engine
	engine = NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", DeferStartupMaintenance: true,
		Emitter: emitterFunc(func(scope string) {
			if scope == "sessions" {
				if events.Add(1) == 2 {
					refresh <- struct{}{}
					engine.SyncAll(t.Context(), nil)
				}
			}
		}),
	})
	t.Cleanup(engine.Close)
	done := make(chan error, 1)
	go func() {
		_, _, err := engine.RunFirstImport(t.Context(), func(progress Progress) {
			if progress.Phase == PhaseSyncing && progress.SessionsDone == 200 {
				paused <- struct{}{}
				<-resume
			}
		})
		done <- err
	}()
	select {
	case <-refresh:
	case <-time.After(30 * time.Second):
		close(resume)
		t.Fatal("no refresh while importing older sessions")
	}
	select {
	case <-paused:
	case <-time.After(30 * time.Second):
		close(resume)
		t.Fatal("import did not reach the pause")
	}
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
	if err == nil {
		for _, session := range page.Sessions {
			messages, readErr := archive.GetMessages(t.Context(), session.ID, 0, 10, true)
			if readErr != nil || len(messages) == 0 {
				close(resume)
				t.Fatalf("listed session %s has no readable messages: %v", session.ID, readErr)
			}
		}
	}
	var starred bool
	var starErr error
	if len(page.Sessions) > 0 {
		starred, starErr = archive.StarSession(t.Context(), page.Sessions[0].ID)
	}
	close(resume)
	require.NoError(t, err)
	require.NoError(t, starErr)
	require.True(t, starred)
	require.Greater(t, len(page.Sessions), 100)
	require.Less(t, len(page.Sessions), 250)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("reentrant emitter blocked the import")
	}
}

func TestFirstImportListsStagedCodexWithMessages(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	root := writeCodexParityRoot(t, uuid)
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", StagedCodexParseMinBytes: 1,
	})
	t.Cleanup(engine.Close)
	stats, _ := engine.SyncNewest(t.Context(), 1, nil)
	require.Zero(t, stats.Failed)
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	messages, err := archive.GetMessages(t.Context(), page.Sessions[0].ID, 0, 100, true)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	require.Equal(t, "run the suite", messages[0].Content)
}

func TestFirstImportListedSessionsHaveMessages(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	for i := range 220 {
		path := filepath.Join(project, fmt.Sprintf("session-%03d.jsonl", i))
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.RunFirstImport(t.Context(), nil)
	}()
	observed := false
	var failure string
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("first import did not finish")
		case <-done:
			require.True(t, observed, "read at least one session while importing")
			require.Empty(t, failure)
			return
		default:
		}
		page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
		if err != nil {
			failure = err.Error()
			continue
		}
		for _, session := range page.Sessions {
			observed = true
			messages, err := archive.GetMessages(t.Context(), session.ID, 0, 10, true)
			if err != nil || len(messages) == 0 {
				failure = fmt.Sprintf("listed session %s had no messages: %v", session.ID, err)
				break
			}
		}
	}
}
