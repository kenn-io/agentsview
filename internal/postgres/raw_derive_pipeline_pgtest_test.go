//go:build pgtest

package postgres

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
