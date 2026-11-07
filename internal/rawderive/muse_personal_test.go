package rawderive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	musePersonalParityID     = "11111111-2222-4333-8444-555555555555"
	musePersonalParityRel    = "agent-66666666-7777-4888-8999-000000000000/sessions/" + musePersonalParityID + ".jsonl"
	musePersonalParityHeader = `{"type":"session_header","version":1,"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z"}` + "\n"
	musePersonalParityUser   = `{"type":"item","seq":0,"source":"scheduler.cron","created_at":"2026-10-05T15:04:06Z","item":{"type":"message","role":"user","text":"hello"}}` + "\n"
	musePersonalParityCall   = `{"type":"item","seq":1,"source":"runtime.background_exec","created_at":"2026-10-05T15:04:07Z","item":{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"}}` + "\n"
	musePersonalParityOutput = `{"type":"item","seq":2,"source":"runtime.background_exec","created_at":"2026-10-05T15:04:08Z","item":{"type":"function_call_output","call_id":"a","output":"result","success":true}}` + "\n"
	musePersonalParityIndex  = `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z"}]}`
)

func musePersonalParityWrite(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func musePersonalParitySetup(t *testing.T, body, index string) (parser.Provider, parser.SourceRef, *rawcheckpoint.Store, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(musePersonalParityRel))
	musePersonalParityWrite(t, path, musePersonalParityHeader+body)
	if index != "" {
		musePersonalParityWrite(t, filepath.Join(filepath.Dir(path), "sessions.json"), index)
	}
	p, ok := parser.NewProvider(parser.AgentMusePersonal,
		parser.ProviderConfig{Roots: []string{root}, Machine: "hosted-worker"})
	require.True(t, ok)
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	spool := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(spool, "checkpoint.db"),
		rawcheckpoint.Options{SpoolDir: filepath.Join(spool, "objects"), MaxOutboxBytes: 16 << 20})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	return p, sources[0], store, path
}

func musePersonalParityHosted(t *testing.T, store *rawcheckpoint.Store,
	objects map[rawsync.ObjectRef][]byte,
) (rawsync.CanonicalManifest, ParsedManifest) {
	t.Helper()
	m, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, ok)
	auth, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	canonical, err := rawsync.ValidateAndCanonicalize(auth, m, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	for _, ref := range canonical.Objects {
		if _, found := objects[ref]; found {
			continue
		}
		body, err := os.ReadFile(store.ObjectPath(ref))
		require.NoError(t, err)
		objects[ref] = body
	}
	tree, err := (Materializer{
		Store:   &materializerStore{objects: objects},
		BaseDir: t.TempDir(), MaxTotalBytes: 16 << 20,
	}).Materialize(t.Context(), canonical)
	require.NoError(t, err)
	defer func() { require.NoError(t, tree.Cleanup()) }()
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)
	result, err := dispatch.Parse(t.Context(), canonical, tree)
	require.NoError(t, err)
	return canonical, result
}

func musePersonalParityAck(t *testing.T, store *rawcheckpoint.Store, m rawsync.CanonicalManifest, generation int64) {
	t.Helper()
	commit := rawsync.CommitResult{
		ManifestID: m.ManifestID, Receipt: m.ManifestID,
		Generation: generation, Created: true,
	}
	require.NoError(t, store.BindFinalizedCommit(t.Context(), "device-a", m.Manifest.CaptureID, commit))
	_, err := store.AcknowledgeGeneration(t.Context(), "device-a", m.Manifest.CaptureID, commit)
	require.NoError(t, err)
}

func TestMusePersonalDurableCaptureHostedParity(t *testing.T) {
	p, source, store, _ := musePersonalParitySetup(t,
		musePersonalParityUser+musePersonalParityCall+musePersonalParityOutput, musePersonalParityIndex)
	local, err := p.Parse(t.Context(), parser.ParseRequest{Source: source, Machine: "hosted-worker"})
	require.NoError(t, err)
	require.Len(t, local.Results, 1)
	capture, err := rawcapture.New(store).Capture(t.Context(), p, source)
	require.NoError(t, err)
	assert.Equal(t, rawcapture.StatusCaptured, capture.Status)
	objects := map[rawsync.ObjectRef][]byte{}
	manifest, hosted := musePersonalParityHosted(t, store, objects)
	assert.Equal(t, "muse-personal:"+musePersonalParityID, manifest.Manifest.SourceKey)
	require.Len(t, manifest.Manifest.Entries, 2)
	assert.ElementsMatch(t, []string{
		musePersonalParityRel,
		filepath.ToSlash(filepath.Join(filepath.Dir(musePersonalParityRel), "sessions.json")),
	},
		[]string{manifest.Manifest.Entries[0].Path, manifest.Manifest.Entries[1].Path})
	require.Len(t, hosted.Outcome.Results, 1)
	l, h := local.Results[0].Result, hosted.Outcome.Results[0].Result
	assert.Equal(t, l.Messages, h.Messages)
	assert.Equal(t, "muse-personal:"+musePersonalParityID, h.Session.File.Path)
	l.Session.File.Path = h.Session.File.Path
	assert.Equal(t, l.Session, h.Session)
	require.Len(t, h.Messages, 2)
	assert.Equal(t, "hello", h.Messages[0].Content)
	require.Len(t, h.Messages[1].ToolCalls, 1)
	require.Len(t, h.Messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "result", h.Messages[1].ToolCalls[0].ResultEvents[0].Content)
	musePersonalParityAck(t, store, manifest, 1)
	again, err := rawcapture.New(store).Capture(t.Context(), p, source)
	require.NoError(t, err)
	assert.Equal(t, rawcapture.StatusUnchanged, again.Status)
	_, pending, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestMusePersonalHostedGenerations(t *testing.T) {
	p, source, store, path := musePersonalParitySetup(t,
		musePersonalParityUser+musePersonalParityCall, "")
	objects := map[rawsync.ObjectRef][]byte{}
	generation := int64(0)
	capture := func(wantRows int, wantEnd string, complete bool) rawsync.CanonicalManifest {
		t.Helper()
		result, err := rawcapture.New(store).Capture(t.Context(), p, source)
		require.NoError(t, err)
		assert.Equal(t, rawcapture.StatusCaptured, result.Status)
		manifest, hosted := musePersonalParityHosted(t, store, objects)
		assert.Equal(t, source.Key, manifest.Manifest.SourceKey)
		if !complete {
			assert.Empty(t, hosted.Outcome.Results)
			assert.False(t, hosted.Outcome.ResultSetComplete)
			assert.False(t, hosted.Outcome.ForceReplace)
		} else {
			require.Len(t, hosted.Outcome.Results, 1)
			r := hosted.Outcome.Results[0].Result
			assert.Equal(t, "muse-personal:"+musePersonalParityID, r.Session.File.Path)
			require.Len(t, r.Messages, wantRows)
			assert.Equal(t, wantEnd, r.Session.EndedAt.Format(time.RFC3339))
			assert.True(t, hosted.Outcome.ForceReplace)
		}
		generation++
		musePersonalParityAck(t, store, manifest, generation)
		return manifest
	}
	first := capture(2, "2026-10-05T15:04:07Z", true)
	musePersonalParityWrite(t, path, musePersonalParityHeader+musePersonalParityUser+
		musePersonalParityCall+musePersonalParityOutput)
	appended := capture(2, "2026-10-05T15:04:08Z", true)
	assert.Equal(t, first.Manifest.ConfiguredRootID, appended.Manifest.ConfiguredRootID)
	assert.Equal(t, first.ManifestID, appended.Manifest.ExpectedParentReceipt)
	transcriptBytes := func(m rawsync.CanonicalManifest) []byte {
		for _, e := range m.Manifest.Entries {
			if !strings.HasSuffix(e.Path, ".jsonl") {
				continue
			}
			var data bytes.Buffer
			for _, ref := range e.Objects {
				data.Write(objects[ref])
			}
			return data.Bytes()
		}
		require.FailNow(t, "no transcript entry")
		return nil
	}
	assert.Equal(t, musePersonalParityHeader+musePersonalParityUser+musePersonalParityCall+
		musePersonalParityOutput, string(transcriptBytes(appended)))
	index := filepath.Join(filepath.Dir(path), "sessions.json")
	musePersonalParityWrite(t, index, musePersonalParityIndex)
	indexed := capture(2, "2026-10-05T16:00:00Z", true)
	require.Len(t, indexed.Manifest.Entries, 2)
	info, err := os.Stat(index)
	require.NoError(t, err)
	musePersonalParityWrite(t, index, strings.ReplaceAll(musePersonalParityIndex, "16:00:00", "17:00:00"))
	require.NoError(t, os.Chtimes(index, info.ModTime(), info.ModTime()))
	capture(2, "2026-10-05T17:00:00Z", true)
	require.NoError(t, os.Remove(index))
	removed := capture(2, "2026-10-05T15:04:08Z", true)
	require.Len(t, removed.Manifest.Entries, 1)
	replacement := strings.ReplaceAll(musePersonalParityUser, "hello", "replacement")
	musePersonalParityWrite(t, path, musePersonalParityHeader+replacement+`{"type":"item"`)
	capture(0, "", false)
	musePersonalParityWrite(t, path, musePersonalParityHeader+replacement)
	completed := capture(1, "2026-10-05T15:04:06Z", true)
	assert.Equal(t, musePersonalParityHeader+replacement, string(transcriptBytes(completed)))
	moved := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))),
		"agent-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "sessions", filepath.Base(path))
	require.NoError(t, os.MkdirAll(filepath.Dir(moved), 0o700))
	require.NoError(t, os.Rename(path, moved))
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	source = sources[0]
	relocated := capture(1, "2026-10-05T15:04:06Z", true)
	assert.Equal(t, first.Manifest.SourceKey, relocated.Manifest.SourceKey)
	assert.Equal(t, first.Manifest.ConfiguredRootID, relocated.Manifest.ConfiguredRootID)
}
