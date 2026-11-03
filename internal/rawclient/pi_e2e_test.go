package rawclient_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawupload"
	"go.kenn.io/agentsview/internal/rawwatch"
)

func TestPiRawSyncCaptureUploadDeriveLifecycle(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentPi, parser.AgentOMP, parser.AgentOMO, parser.AgentStepCode, parser.AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			ctx := t.Context()
			httpServer, auth, objects := rawclient.NewRawSyncServerForTest(t)
			enrollment, err := auth.EnrollDevice(ctx, "tenant-pi-test", "collector-test")
			require.NoError(t, err)
			checkpoint, err := rawcheckpoint.Open(ctx, filepath.Join(t.TempDir(), "checkpoint.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
			require.NoError(t, checkpoint.SetDevice(ctx, enrollment.Identity.DeviceID))
			client, err := rawclient.NewClient(rawclient.Config{BaseURL: httpServer.URL, DeviceID: enrollment.Identity.DeviceID, Credential: enrollment.Credential, HTTPClient: httpServer.Client(), ChunkBytes: 128})
			require.NoError(t, err)
			uploader := rawupload.New(checkpoint, client, enrollment.Identity.DeviceID)
			capturer := rawcapture.New(checkpoint)
			root := t.TempDir()
			childRel := "project/child.jsonl"
			parentRel := "project/parent-file.jsonl"
			switch agent {
			case parser.AgentPi, parser.AgentOMO:
				childRel = "project/parent-file/task/run-0/session.jsonl"
			case parser.AgentOMP:
				childRel = "project/parent-file/agent.jsonl"
			case parser.AgentPrimeAgent:
				childRel = "child.jsonl"
				parentRel = "parent-file.jsonl"
			default:
			}
			child := filepath.Join(root, filepath.FromSlash(childRel))
			parent := filepath.Join(root, filepath.FromSlash(parentRel))
			write := func(path, body string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			}
			write(parent, `{"type":"session","id":"parent-header"}`+"\n")
			title := ""
			if agent == parser.AgentOMP {
				title = `{"type":"title","title":"Title one"}` + "\n"
			}
			// Structural parents apply to nested Pi/OMO/OMP; flat siblings use parentSession.
			parentHint := ""
			if agent == parser.AgentStepCode || agent == parser.AgentPrimeAgent {
				parentHint = `,"parentSession":"parent-file.jsonl"`
			}
			content := title + `{"type":"session","id":"child-header","cwd":"/work/project"` + parentHint + "}\n" +
				`{"type":"message","id":"u1","message":{"role":"user","content":"capture this"}}` + "\n" +
				`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[{"type":"toolCall","id":"call1","name":"bash","arguments":{"command":"echo hello"}}]}}` + "\n" +
				`{"type":"message","id":"r1","parentId":"a1","message":{"role":"toolResult","toolCallId":"call1","content":"hello"}}` + "\n"
			write(child, content)
			provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}, Machine: "collector"})
			require.True(t, ok)
			sources, err := parser.RawCaptureSourcesForChangedPath(ctx, provider, parser.ChangedPathRequest{Path: child})
			require.NoError(t, err)
			require.Len(t, sources, 1)
			source := sources[0]
			capture, err := capturer.Capture(ctx, provider, source)
			require.NoError(t, err)
			require.Equal(t, rawcapture.StatusCaptured, capture.Status)
			upload := func(wantGeneration int64) rawsync.CanonicalManifest {
				result, found, err := uploader.UploadNext(ctx)
				require.NoError(t, err)
				require.True(t, found)
				assert.Equal(t, wantGeneration, result.Generation)
				_, reader, err := objects.OpenManifest(ctx, enrollment.Identity, result.ManifestID)
				require.NoError(t, err)
				payload, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.NoError(t, reader.Close())
				manifest, err := rawsync.ParseCanonicalManifest(enrollment.Identity, result.ManifestID, payload, rawsync.DefaultManifestLimits())
				require.NoError(t, err)
				assert.Equal(t, agent, manifest.Manifest.Provider)
				assert.Equal(t, source.Key, manifest.Manifest.SourceKey)
				return manifest
			}
			assertParity := func(manifest rawsync.CanonicalManifest, wantParent string) {
				local, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(ctx), parser.ParseRequest{Source: source})
				require.NoError(t, err)
				require.Len(t, local.Results, 1)
				// Derivation cannot consult the original collector tree.
				offline := root + "-offline"
				require.NoError(t, os.Rename(root, offline))
				defer func() { require.NoError(t, os.Rename(offline, root)) }()
				materialized, err := (rawderive.Materializer{Store: objects, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(ctx, manifest)
				require.NoError(t, err)
				defer func() { require.NoError(t, materialized.Cleanup()) }()
				dispatch, err := rawderive.NewProviderParser(parser.ProviderFactories(), "worker")
				require.NoError(t, err)
				hosted, err := dispatch.Parse(ctx, manifest, materialized)
				require.NoError(t, err)
				require.Len(t, hosted.Outcome.Results, 1)
				got := hosted.Outcome.Results[0].Result
				assert.Equal(t, local.Results[0].Result.Messages, got.Messages)
				expected := local.Results[0].Result.Session
				expected.File = got.Session.File
				expected.Machine = "worker"
				assert.Equal(t, expected, got.Session)
				assert.Equal(t, child, got.Session.File.Path)
				assert.Equal(t, wantParent, got.Session.ParentSessionID)
			}
			manifest := upload(1)
			require.Len(t, manifest.Manifest.Entries, 2)
			assertParity(manifest, provider.Definition().IDPrefix+"parent-header")
			base, found, err := checkpoint.CaptureBase(ctx, capture.Source)
			require.NoError(t, err)
			require.True(t, found)
			unchanged, err := capturer.Capture(ctx, provider, source)
			require.NoError(t, err)
			assert.Equal(t, rawcapture.StatusUnchanged, unchanged.Status)
			_, pending, err := uploader.UploadNext(ctx)
			require.NoError(t, err)
			assert.False(t, pending)
			appendLine := `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"appended"}}` + "\n"
			content += appendLine
			write(child, content)
			appended, err := capturer.Capture(ctx, provider, source)
			require.NoError(t, err)
			assert.Equal(t, rawcapture.StatusCaptured, appended.Status)
			manifest = upload(2)
			appendedBase, found, err := checkpoint.CaptureBase(ctx, capture.Source)
			require.NoError(t, err)
			require.True(t, found)
			firstObjects := piCapturedEntry(t, base.Entries, childRel).Objects
			laterObjects := piCapturedEntry(t, appendedBase.Entries, childRel).Objects
			require.Greater(t, len(laterObjects), len(firstObjects))
			assert.Equal(t, firstObjects, laterObjects[:len(firstObjects)])
			assertParity(manifest, provider.Definition().IDPrefix+"parent-header")
			// Same-size replacement, with mtime restored, must invalidate the prefix.
			info, err := os.Stat(child)
			require.NoError(t, err)
			content = strings.Replace(content, "capture this", "replace this", 1)
			write(child, content)
			require.NoError(t, os.Chtimes(child, info.ModTime(), info.ModTime()))
			replacement, err := capturer.Capture(ctx, provider, source)
			require.NoError(t, err)
			assert.Equal(t, rawcapture.StatusCaptured, replacement.Status)
			manifest = upload(3)
			assertParity(manifest, provider.Definition().IDPrefix+"parent-header")
			generation := int64(3)
			if agent == parser.AgentOMP {
				content = strings.Replace(content, "Title one", "Title two", 1)
				write(child, content)
				rewritten, err := capturer.Capture(ctx, provider, source)
				require.NoError(t, err)
				assert.Equal(t, rawcapture.StatusCaptured, rewritten.Status)
				generation++
				manifest = upload(generation)
				assertParity(manifest, provider.Definition().IDPrefix+"parent-header")
			}
			write(parent, `{"type":"session","id":"parent-header"}`+"\n"+`{"type":"message","message":{"role":"user","content":"parent grew"}}`+"\n")
			grown, err := capturer.Capture(ctx, provider, source)
			require.NoError(t, err)
			assert.Equal(t, rawcapture.StatusCaptured, grown.Status)
			generation++
			upload(generation)
			grownBase, found, err := checkpoint.CaptureBase(ctx, capture.Source)
			require.NoError(t, err)
			require.True(t, found)
			originalParent := piCapturedEntry(t, base.Entries, parentRel).Objects
			grownParent := piCapturedEntry(t, grownBase.Entries, parentRel).Objects
			require.Greater(t, len(grownParent), len(originalParent))
			assert.Equal(t, originalParent, grownParent[:len(originalParent)])
			require.NoError(t, os.Remove(parent))
			removed, err := capturer.Capture(ctx, provider, source)
			require.NoError(t, err)
			assert.Equal(t, rawcapture.StatusCaptured, removed.Status)
			generation++
			manifest = upload(generation)
			require.Len(t, manifest.Manifest.Entries, 1)
			assert.Equal(t, childRel, manifest.Manifest.Entries[0].Path)
			require.NoError(t, os.Remove(child))
			auditor := rawwatch.NewAuditor(checkpoint, capturer, 16)
			unavailable := root + "-unavailable"
			require.NoError(t, os.Rename(root, unavailable))
			incomplete, err := auditor.AuditProviderFull(ctx, provider)
			require.NoError(t, os.Rename(unavailable, root))
			require.NoError(t, err)
			assert.False(t, incomplete.Complete)
			assert.Zero(t, incomplete.Tombstoned)
			complete, err := auditor.AuditProviderFull(ctx, provider)
			require.NoError(t, err)
			assert.True(t, complete.Complete)
			assert.Equal(t, 1, complete.Tombstoned)
			generation++
			manifest = upload(generation)
			assert.Equal(t, rawsync.ManifestTombstone, manifest.Manifest.Kind)
			assert.Empty(t, manifest.Manifest.Entries)
		})
	}
}

func piCapturedEntry(t *testing.T, entries []rawcheckpoint.CapturedEntry, path string) rawcheckpoint.CapturedEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Path == path {
			return entry
		}
	}
	require.FailNow(t, "missing captured entry", path)
	return rawcheckpoint.CapturedEntry{}
}
