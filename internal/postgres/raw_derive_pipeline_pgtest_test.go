//go:build pgtest

package postgres

import (
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
	"go.kenn.io/agentsview/internal/rawtest"
)

func TestRawCapturedSourcesReachHostedWorker(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentZCode, parser.AgentCodex} {
		t.Run(string(agent), func(t *testing.T) {
			f := newHostedFixture(t, "tenant-capture")
			pg := f.runtime
			metadata, err := NewHostedRawIngestStore(pg, f.tenant, "parser-data-17")
			require.NoError(t, err)
			identity := rawIngestIdentity(t, f.tenant)
			_, err = pg.Exec(`INSERT INTO raw_devices(device_id,display_name,credential_sha256,created_at) VALUES($1,'synthetic device',$2,clock_timestamp())`, identity.DeviceID, make([]byte, 32))
			require.NoError(t, err)
			root := t.TempDir()
			ids := []string{rawtest.ClaudeID}
			if agent == parser.AgentClaude {
				rawtest.Claude(t, root)
				rawtest.ClaudeChild(t, root)
				ids = append(ids, rawtest.ClaudeChildID)
			} else if agent == parser.AgentZCode {
				rawtest.ZCode(t, root)
				ids = []string{rawtest.ZCodeID, rawtest.ZCodeBillableID}
			} else {
				rawtest.CodexTools(t, root)
				ids = []string{"codex:" + rawtest.CodexToolsID}
			}
			oracle, engine := rawtest.Oracle(t, agent, root, "", "")
			require.Equal(t, len(ids), engine.SyncAll(t.Context(), nil).Synced)

			provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
			require.NoError(t, err)
			require.True(t, sources.Complete)
			require.NotEmpty(t, sources.Sources)
			checkpointDir := t.TempDir()
			checkpoint, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(checkpointDir, "checkpoint.db"), rawcheckpoint.Options{
				SpoolDir: filepath.Join(checkpointDir, "spool"), MaxOutboxBytes: 1 << 20,
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
			require.NoError(t, checkpoint.SetDevice(t.Context(), identity.DeviceID))

			repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, repository.Close()) })
			objects, err := rawsync.NewArtifactObjectStore(repository.Content())
			require.NoError(t, err)
			limits := rawsync.DefaultManifestLimits()
			service, err := rawsync.NewService(objects, metadata, limits, "parser-data-17")
			require.NoError(t, err)

			dispatch, err := rawderive.NewProviderParser(parser.ProviderFactories(), "hosted-worker")
			require.NoError(t, err)
			materializationDir := t.TempDir()
			sink, err := NewRawProjectionStore(pg, RawProjectionOptions{Tenant: f.tenant})
			require.NoError(t, err)
			worker, err := rawderive.NewWorker(rawderive.WorkerConfig{
				Queue: metadata, Manifests: rawderive.ManifestLoader{Store: objects, Limits: limits},
				Materializer: rawderive.Materializer{Store: objects, BaseDir: materializationDir, MaxTotalBytes: 1 << 20},
				Parser:       dispatch, Projection: sink, Owner: "worker-a", BatchSize: 1,
				LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, AttemptTimeout: time.Minute,
				RetryBase: time.Second, RetryMax: time.Minute, MaxAttempts: 3,
			})
			require.NoError(t, err)

			for _, source := range sources.Sources {
				capture, err := rawcapture.New(checkpoint).Capture(t.Context(), provider, source)
				require.NoError(t, err)
				require.Equal(t, rawcapture.StatusCaptured, capture.Status)
				manifest, found, err := checkpoint.FinalizeNextManifest(t.Context(), identity.DeviceID)
				require.NoError(t, err)
				require.True(t, found)

				for _, entry := range manifest.Entries {
					for _, object := range entry.Objects {
						missing, err := service.MissingObjects(t.Context(), identity, agent, []rawsync.ObjectRef{object})
						require.NoError(t, err)
						if len(missing) == 0 {
							continue
						}
						file, err := os.Open(checkpoint.ObjectPath(object))
						require.NoError(t, err)
						_, uploadErr := service.FinalizeObject(t.Context(), identity, agent, object, file)
						require.NoError(t, file.Close())
						require.NoError(t, uploadErr)
					}
				}
				commit, err := service.CommitManifest(t.Context(), identity, manifest)
				require.NoError(t, err)
				require.NoError(t, checkpoint.BindFinalizedCommit(t.Context(), identity.DeviceID, manifest.CaptureID, commit))
				_, err = checkpoint.AcknowledgeGeneration(t.Context(), identity.DeviceID, manifest.CaptureID, commit)
				require.NoError(t, err)

				result, err := worker.RunBatch(t.Context())

				require.NoError(t, err)
				assert.Equal(t, rawderive.BatchResult{Claimed: 1, Succeeded: 1}, result)
				var state string
				require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE manifest_id=$1`, commit.ManifestID).Scan(&state))
				assert.Equal(t, "complete", state)
			}

			store, err := newHostedAdapter(pg, f.tenant)
			require.NoError(t, err)
			rawtest.EqualStored(t, t.Context(), oracle, store, ids...)
			for _, id := range ids {
				resolved, err := sink.Resolve(t.Context(), id)
				require.NoError(t, err)
				rawtest.EqualUsageEvents(t, t.Context(), oracle, pg, id, resolved.SessionID)
			}

			remaining, err := os.ReadDir(materializationDir)
			require.NoError(t, err)
			assert.Empty(t, remaining, "the worker must remove the captured source tree")
		})
	}
}

// Source-app deletion must not hide sessions already captured in the hosted archive.
func TestRawProjectionCrushRetainsSessionsDeletedFromSource(t *testing.T) {
	f := newProjectionFixture(t)
	root := t.TempDir()
	sourceDB, err := sql.Open("sqlite3", filepath.Join(root, parser.CrushDBName))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sourceDB.Close()) })
	_, err = sourceDB.ExecContext(t.Context(), `
        CREATE TABLE sessions (
            id TEXT PRIMARY KEY, parent_session_id TEXT, title TEXT NOT NULL,
            message_count INTEGER NOT NULL DEFAULT 0,
            prompt_tokens INTEGER NOT NULL DEFAULT 0,
            completion_tokens INTEGER NOT NULL DEFAULT 0,
            cost REAL NOT NULL DEFAULT 0,
            updated_at INTEGER NOT NULL, created_at INTEGER NOT NULL
        );
        CREATE TABLE messages (
            id TEXT PRIMARY KEY, session_id TEXT NOT NULL, role TEXT NOT NULL,
            parts TEXT NOT NULL DEFAULT '[]', model TEXT,
            created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
        );
        INSERT INTO sessions (id,title,updated_at,created_at) VALUES
            ('session-1','First session',1789093626,1789093626),
            ('session-2','Second session',1789093626,1789093626);
        INSERT INTO messages (id,session_id,role,parts,created_at,updated_at) VALUES
            ('message-1','session-1','user','[{"type":"text","data":{"text":"hello"}}]',1789093626,1789093626),
            ('message-2','session-2','user','[{"type":"text","data":{"text":"second"}}]',1789093626,1789093626);
    `)
	require.NoError(t, err)
	provider, ok := parser.NewProvider(parser.AgentCrush, parser.ProviderConfig{Roots: []string{root}, Machine: "capture-device"})
	require.True(t, ok)
	discovery, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.Len(t, discovery.Sources, 1)
	cpRoot := t.TempDir()
	checkpoint, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(cpRoot, "checkpoint.db"), rawcheckpoint.Options{SpoolDir: filepath.Join(cpRoot, "spool"), MaxOutboxBytes: 1 << 20})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
	identity := rawIngestIdentity(t, f.tenant)
	require.NoError(t, checkpoint.SetDevice(t.Context(), identity.DeviceID))
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_devices(device_id,display_name,credential_sha256,created_at) VALUES($1,'synthetic device',$2,clock_timestamp())`, identity.DeviceID, make([]byte, 32))
	require.NoError(t, err)
	dispatch, err := rawderive.NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)

	store, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)

	// Replay deletion of one member and then the last member of the SQLite source.
	for _, remaining := range []int{2, 1, 0} {
		if remaining < 2 {
			id := "session-2"
			if remaining == 0 {
				id = "session-1"
			}
			_, err = sourceDB.ExecContext(t.Context(), `DELETE FROM messages WHERE session_id=?; DELETE FROM sessions WHERE id=?`, id, id)
			require.NoError(t, err)
		}
		capture, err := rawcapture.New(checkpoint).Capture(t.Context(), provider, discovery.Sources[0])
		require.NoError(t, err)
		require.Equal(t, rawcapture.StatusCaptured, capture.Status)
		manifest, found, err := checkpoint.FinalizeNextManifest(t.Context(), identity.DeviceID)
		require.NoError(t, err)
		require.True(t, found)
		for _, entry := range manifest.Entries {
			for _, object := range entry.Objects {
				file, err := os.Open(checkpoint.ObjectPath(object))
				require.NoError(t, err)
				_, uploadErr := f.custody.FinalizeObject(t.Context(), identity, parser.AgentCrush, object, file)
				require.NoError(t, file.Close())
				require.NoError(t, uploadErr)
			}
		}
		canonical, err := rawsync.ValidateAndCanonicalize(identity, manifest, rawsync.DefaultManifestLimits())
		require.NoError(t, err)
		commit, err := f.custody.CommitManifest(t.Context(), identity, manifest)
		require.NoError(t, err)
		require.NoError(t, checkpoint.BindFinalizedCommit(t.Context(), identity.DeviceID, manifest.CaptureID, commit))
		_, err = checkpoint.AcknowledgeGeneration(t.Context(), identity.DeviceID, manifest.CaptureID, commit)
		require.NoError(t, err)
		tree, err := (rawderive.Materializer{Store: f.objects, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), canonical)
		require.NoError(t, err)
		parsed, err := dispatch.Parse(t.Context(), canonical, tree)
		require.NoError(t, err)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, canonical), canonical, parsed))
		require.NoError(t, tree.Cleanup())
		var state string
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE manifest_id=$1`, commit.ManifestID).Scan(&state))
		assert.Equal(t, "complete", state, "snapshot with %d source sessions", remaining)
		for _, want := range []struct{ alias, content string }{
			{"crush:session-1", "hello"},
			{"crush:session-2", "second"},
		} {
			messages, err := store.GetAllMessages(t.Context(), want.alias)
			require.NoError(t, err)
			require.Len(t, messages, 1, "snapshot with %d source sessions, alias %s", remaining, want.alias)
			assert.Equal(t, want.content, messages[0].Content)
		}
	}
}
