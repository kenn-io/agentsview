package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	root := t.TempDir()
	for i := range 30 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		path := filepath.Join(root, "session-state", id, "events.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		content := fmt.Sprintf("{\"type\":\"session.start\",\"data\":{\"sessionId\":%q},\"timestamp\":\"2026-01-01T00:00:00Z\"}\n{\"type\":\"user.message\",\"data\":{\"content\":\"hello\"},\"timestamp\":\"2026-01-01T00:00:01Z\"}\n", id)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		mtime := time.Unix(1700000000, 0)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}
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
	_, _, err = engine.RunFirstImport(t.Context(), nil)
	require.NoError(t, err)
	page, err = archive.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 30)
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
	resume := make(chan struct{})
	var events int
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", DeferStartupMaintenance: true,
		Emitter: emitterFunc(func(scope string) {
			if scope == "sessions" {
				events++
				if events == 2 {
					refresh <- struct{}{}
					<-resume
				}
			}
		}),
	})
	t.Cleanup(engine.Close)
	done := make(chan error, 1)
	go func() {
		_, _, err := engine.RunFirstImport(t.Context(), nil)
		done <- err
	}()
	select {
	case <-refresh:
	case <-time.After(30 * time.Second):
		close(resume)
		t.Fatal("no refresh while importing older sessions")
	}
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
	close(resume)
	require.NoError(t, err)
	require.Greater(t, len(page.Sessions), 100)
	require.Less(t, len(page.Sessions), 250)
	require.NoError(t, <-done)
}
