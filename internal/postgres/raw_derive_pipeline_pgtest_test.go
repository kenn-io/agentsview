//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawCapturedSourcesReachHostedWorker(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentZCode} {
		t.Run(string(agent), func(t *testing.T) {
			pg, metadata := newRawIngestTestStore(t)
			identity := rawIngestIdentity(t, "tenant-a")
			root := t.TempDir()
			if agent == parser.AgentClaude {
				contents, err := os.ReadFile("../parser/testdata/claude/valid_session.jsonl")
				require.NoError(t, err)
				project := filepath.Join(root, "project")
				require.NoError(t, os.Mkdir(project, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(project, "session.jsonl"), contents, 0o600))
			} else {
				dbDir := filepath.Join(root, "db")
				require.NoError(t, os.Mkdir(dbDir, 0o700))
				sourceDB, err := sql.Open("sqlite3", filepath.Join(dbDir, parser.ZCodeDBName))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sourceDB.Close()) })
				_, err = sourceDB.Exec(`
					PRAGMA journal_mode = WAL;
					CREATE TABLE session (
						id TEXT PRIMARY KEY, project_id TEXT, workspace_id TEXT,
						directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER
					);
					CREATE TABLE message (
						id TEXT PRIMARY KEY, session_id TEXT, time_created TEXT, data TEXT
					);
					CREATE TABLE part (
						id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created TEXT, data TEXT
					);
					INSERT INTO session VALUES (
						'session-a', NULL, NULL, '/workspace/project', 'Captured session',
						1783339200000, 1783339260000
					);
					INSERT INTO message VALUES (
						'message-a', 'session-a', '2026-07-06T12:00:01Z', '{"role":"user"}'
					);
					INSERT INTO part VALUES (
						'part-a', 'message-a', 'session-a', '2026-07-06T12:00:01Z',
						'{"type":"text","text":"Fix the login bug"}'
					);
				`)
				require.NoError(t, err)
			}
			provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
			require.NoError(t, err)
			require.True(t, sources.Complete)
			require.Len(t, sources.Sources, 1)
			checkpointDir := t.TempDir()
			checkpoint, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(checkpointDir, "checkpoint.db"), rawcheckpoint.Options{
				SpoolDir: filepath.Join(checkpointDir, "spool"), MaxOutboxBytes: 1 << 20,
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
			require.NoError(t, checkpoint.SetDevice(t.Context(), identity.DeviceID))
			capture, err := rawcapture.New(checkpoint).Capture(t.Context(), provider, sources.Sources[0])
			require.NoError(t, err)
			require.Equal(t, rawcapture.StatusCaptured, capture.Status)
			manifest, found, err := checkpoint.FinalizeNextManifest(t.Context(), identity.DeviceID)
			require.NoError(t, err)
			require.True(t, found)

			repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, repository.Close()) })
			objects, err := rawsync.NewArtifactObjectStore(repository.Content())
			require.NoError(t, err)
			limits := rawsync.DefaultManifestLimits()
			service, err := rawsync.NewService(objects, metadata, limits, "parser-data-17")
			require.NoError(t, err)
			for _, entry := range manifest.Entries {
				for _, object := range entry.Objects {
					file, err := os.Open(checkpoint.ObjectPath(object))
					require.NoError(t, err)
					_, uploadErr := service.FinalizeObject(t.Context(), identity, agent, object, file)
					require.NoError(t, file.Close())
					require.NoError(t, uploadErr)
				}
			}
			commit, err := service.CommitManifest(t.Context(), identity, manifest)
			require.NoError(t, err)
			dispatch, err := rawderive.NewProviderParser(parser.ProviderFactories(), "hosted-worker")
			require.NoError(t, err)
			materializationDir := t.TempDir()
			sink := &rawParseRecordingSink{store: metadata}
			worker, err := rawderive.NewWorker(rawderive.WorkerConfig{
				Queue: metadata, Manifests: rawderive.ManifestLoader{Store: objects, Limits: limits},
				Materializer: rawderive.Materializer{Store: objects, BaseDir: materializationDir, MaxTotalBytes: 1 << 20},
				Parser:       dispatch, Projection: sink, Owner: "worker-a", BatchSize: 1,
				LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, AttemptTimeout: time.Minute,
				RetryBase: time.Second, RetryMax: time.Minute, MaxAttempts: 3,
			})
			require.NoError(t, err)

			result, err := worker.RunBatch(t.Context())

			require.NoError(t, err)
			assert.Equal(t, rawderive.BatchResult{Claimed: 1, Succeeded: 1}, result)
			require.Len(t, sink.parsed.Outcome.Results, 1)
			assert.Empty(t, sink.parsed.Outcome.SourceErrors)
			parsed := sink.parsed.Outcome.Results[0].Result
			require.NotNil(t, parsed.Session)
			assert.Equal(t, agent, parsed.Session.Agent)
			require.NotEmpty(t, parsed.Messages)
			assert.Equal(t, "Fix the login bug", parsed.Messages[0].Content)
			var state string
			require.NoError(t, pg.QueryRowContext(t.Context(),
				`SELECT state FROM raw_ingest_jobs WHERE manifest_id = $1`, commit.ManifestID,
			).Scan(&state))
			assert.Equal(t, "complete", state)
			remaining, err := os.ReadDir(materializationDir)
			require.NoError(t, err)
			assert.Empty(t, remaining, "the worker must remove the captured source tree")
		})
	}
}

// The production projection is a later layer. Record its input while using
// the real lease-completion fence; this test stops at that boundary.
type rawParseRecordingSink struct {
	store  *RawIngestStore
	parsed rawderive.ParsedManifest
}

func (s *rawParseRecordingSink) Project(
	ctx context.Context, lease rawderive.JobLease, _ rawsync.CanonicalManifest, parsed rawderive.ParsedManifest,
) error {
	if err := s.store.CompleteRawParseJob(ctx, lease); err != nil {
		return err
	}
	s.parsed = parsed
	return nil
}
