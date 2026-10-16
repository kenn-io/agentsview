package sync

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestTrySessionRefreshRefusesContentionAtEachStage(t *testing.T) {
	for _, stage := range []string{"parent", "local child", "s3 child"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				database := openTestDB(t)
				root := t.TempDir()
				parentPath := filepath.Join(root, "project", "parent.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(parentPath), 0o755))
				require.NoError(t, os.WriteFile(parentPath, []byte(testjsonl.NewSessionBuilder().
					AddClaudeUser("2026-01-01T00:00:00Z", "parent transcript").String()), 0o600))
				if stage == "s3 child" {
					parentPath = "s3://bucket/host-a/raw/claude/project/parent.jsonl"
				}
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{
					ID: "parent", Agent: "claude", Project: "project", FilePath: new(parentPath),
				}))
				engine := NewEngine(t.Context(), database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
					Ephemeral: true,
				})
				t.Cleanup(engine.Close)

				release := make(chan struct{}, 1)
				t.Cleanup(func() {
					select {
					case release <- struct{}{}:
					default:
					}
				})
				done := make(chan error, 1)
				entered := make(chan struct{})
				holdSync := func() {
					go func() {
						done <- engine.RunExclusive(func() error {
							close(entered)
							<-release
							return nil
						})
					}()
					synctest.Wait()
					select {
					case <-entered:
					default:
						require.FailNow(t, "exclusive sync did not acquire the lock")
					}
				}

				original := subagentTranscriptPaths
				t.Cleanup(func() { subagentTranscriptPaths = original })
				subagentTranscriptPaths = func(string) []string {
					assert.NotEqual(t, "parent", stage,
						"busy parent must stop before child discovery")
					holdSync()
					return []string{parentPath + "/subagents/agent-child.jsonl"}
				}
				if stage == "parent" {
					holdSync()
				}
				if stage == "s3 child" {
					oldStat := statClaudeS3Session
					statClaudeS3Session = func(string) (parser.S3Object, error) {
						return parser.S3Object{}, missingS3ObjectError()
					}
					t.Cleanup(func() { statClaudeS3Session = oldStat })
				}

				err := engine.TrySyncSessionWithSubagentsContext(t.Context(), "parent")
				require.ErrorIs(t, err, ErrSyncInProgress)
				release <- struct{}{}
				require.NoError(t, <-done)
			})
		})
	}
}
