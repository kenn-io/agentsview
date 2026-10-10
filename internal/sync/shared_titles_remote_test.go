package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestAntigravitySharedTitleRefreshOwnedRemoteSources(t *testing.T) {
	for _, mode := range []string{"transport", "filesystem mirror", "wrong machine", "outside root", "nested foreign owner", "nested configured root", "missing resolver", "wrong mapping"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			const bare = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			physical := filepath.Join(root, "brain", bare, ".system_generated", "logs", "transcript.jsonl")
			stored := physical
			fullID := "antigravity:" + bare
			machine := "remote"
			emitter := &sharedTitleScopeRecorder{}
			database := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "devbox", Emitter: emitter,
				SourceMachines: map[parser.AgentType]map[string]string{parser.AgentAntigravity: {root: "remote"}}}
			transported := mode != "filesystem mirror" && mode != "nested foreign owner"
			if transported {
				cfg.Machine = "remote"
				cfg.IDPrefix = "remote~"
				fullID = cfg.IDPrefix + fullID
				cfg.PathRewriter = func(path string) string {
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return ""
					}
					return "remote:/srv/antigravity/" + filepath.ToSlash(rel)
				}
				cfg.StoredPathResolver = func(path string) (string, bool) {
					rel, ok := strings.CutPrefix(path, "remote:/srv/antigravity/")
					if !ok {
						return "", false
					}
					return filepath.Join(root, filepath.FromSlash(rel)), true
				}
				stored = cfg.PathRewriter(physical)
			}
			if mode == "wrong machine" {
				machine = "otherhost"
			}
			if mode == "outside root" {
				stored = "remote:/srv/elsewhere/transcript.jsonl"
			}
			if mode == "nested foreign owner" {
				cfg.SourceMachines[parser.AgentAntigravity][filepath.Join(root, "brain")] = "otherhost"
			}
			if mode == "nested configured root" {
				cfg.AgentDirs[parser.AgentAntigravity] = append(cfg.AgentDirs[parser.AgentAntigravity], filepath.Join(root, "brain"))
			}
			if mode == "missing resolver" {
				cfg.StoredPathResolver = nil
			}
			if mode == "wrong mapping" {
				cfg.StoredPathResolver = func(string) (string, bool) { return filepath.Join(root, "wrong.jsonl"), true }
			}
			display := "user override"
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: fullID, Agent: string(parser.AgentAntigravity), Machine: machine, FilePath: &stored, SessionName: strPtr("old"), DisplayName: &display}))
			require.NoError(t, database.RenameSession(t.Context(), fullID, &display))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: fullID, Ordinal: 1, Role: "user", Content: "unchanged body"}}))
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			title := "remote rename"
			path := writeConversationSummariesDB(t, root, map[string]*string{bare: &title})
			plan, err := engine.PlanChangedPathsContext(t.Context(), []string{path + "-wal"})
			require.NoError(t, err)
			require.Len(t, plan.SharedTitleTasks, 1)
			assert.Empty(t, plan.Files)
			assert.Empty(t, plan.FallbackProviders)
			result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
			require.NoError(t, err)
			valid := mode == "transport" || mode == "filesystem mirror"
			if valid {
				requireStoredTitle(t, database, fullID, title)
				assert.Equal(t, 1, result.Stats.TitlesUpdated)
				assert.Equal(t, []string{"sessions"}, emitter.take())
				result, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.Zero(t, result.Stats.TitlesUpdated)
				assert.Empty(t, emitter.take())
			} else {
				requireStoredTitle(t, database, fullID, "old")
				assert.Zero(t, result.Stats.TitlesUpdated)
				assert.Empty(t, emitter.take())
			}
			assert.Zero(t, result.FilesProcessed)
			messages, err := database.GetAllMessages(t.Context(), fullID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "unchanged body", messages[0].Content)
			session, err := database.GetSessionFull(t.Context(), fullID)
			require.NoError(t, err)
			require.NotNil(t, session.DisplayName)
			assert.Equal(t, display, *session.DisplayName)
		})
	}
}

func TestQoderRemoteTitleRefreshNeverReadsLocalApplicationDatabase(t *testing.T) {
	for _, mode := range []string{"prefixed", "rewritten"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			root := filepath.Join(home, ".qoder", "projects")
			database := openTestDB(t)
			cfg := EngineConfig{Machine: "remote", AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}}}
			if mode == "prefixed" {
				cfg.IDPrefix = "remote~"
			} else {
				cfg.PathRewriter = func(path string) string { return path }
			}
			id := cfg.IDPrefix + "qoder:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			source := filepath.Join(root, "project", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.jsonl")
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: id, Agent: string(parser.AgentQoder), Machine: "remote", FilePath: &source, SessionName: strPtr("old")}))
			path := filepath.Join(home, "Library", "Application Support", "com.qoder.app.stable", "main.sqlite")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte("not a database"), 0o600))
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			plan, err := engine.PlanChangedPathsContext(t.Context(), []string{path})
			require.NoError(t, err)
			require.Len(t, plan.SharedTitleTasks, 1)
			result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
			require.NoError(t, err, "the corrupt local application database must not be read")
			assert.Zero(t, result.Stats.TitlesUpdated)
			requireStoredTitle(t, database, id, "old")
		})
	}
}
