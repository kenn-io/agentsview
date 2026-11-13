package sessionmaterialize

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	lineageDevice = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lineageG      = "33333333-3333-4333-8333-333333333333"
	lineageP      = "11111111-1111-4111-8111-111111111111"
	lineageC      = "22222222-2222-4222-8222-222222222222"
	lineageR      = "44444444-4444-4444-8444-444444444444"
)

type snapshotLineageFixture struct {
	t                *testing.T
	root, data, seed string
	settings         rawarchive.RecoverySettings
	saved            *db.DB
	archive          *rawarchive.Archive
}

func newSnapshotLineageFixture(t *testing.T) *snapshotLineageFixture {
	t.Helper()
	root := t.TempDir()
	data := t.TempDir()
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(lineageDevice))
	return &snapshotLineageFixture{t: t, root: root, data: data, seed: filepath.Join(root, "seed"), settings: rawarchive.RecoverySettings{LocalMachineName: "fixture"}}
}

func (f *snapshotLineageFixture) capture(name, previous string, homes ...string) string {
	f.t.Helper()
	var roots []rawarchive.RootSpec
	for _, home := range homes {
		roots = append(roots, rawarchive.RootSpec{Provider: "codex", Path: home})
	}
	destination := filepath.Join(f.root, name)
	_, err := rawarchive.Capture(f.t.Context(), rawarchive.CaptureOptions{DataDir: f.data, Destination: destination, IdentityFrom: previous, Settings: f.settings, Roots: roots})
	require.NoError(f.t, err)
	return filepath.Join(destination, "capture.json")
}

func (f *snapshotLineageFixture) open(capture string) {
	f.t.Helper()
	_, err := rawarchive.Seed(f.t.Context(), capture, f.seed, nil)
	require.NoError(f.t, err)
	f.saved, err = db.OpenIsolatedContext(f.t.Context(), filepath.Join(f.seed, "sessions.db"))
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = f.saved.Close() })
	f.archive, err = rawarchive.Open(f.t.Context(), f.saved, f.seed, nil)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = f.archive.Close() })
}

func lineagePage(thread, timestamp, message string, ordinal int, fields map[string]any) string {
	fields["history_mode"] = "paginated"
	withOrdinal := func(line string, n int) string {
		return strings.TrimSuffix(line, "}") + fmt.Sprintf(`,"ordinal":%d}`, n)
	}
	return testjsonl.JoinJSONL(withOrdinal(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", timestamp, fields), ordinal), withOrdinal(testjsonl.CodexMsgJSON("user", message, timestamp), ordinal+1))
}

func lineageBase(thread string, ordinal, bytes int) map[string]any {
	return map[string]any{"thread_id": thread, "end_ordinal_exclusive": ordinal, "end_byte_offset": bytes}
}

func (f *snapshotLineageFixture) accepted(home, key, original string) (db.RawArchiveRoot, *db.RawArchiveSource, rawsync.CanonicalManifest) {
	f.t.Helper()
	roots, err := f.saved.ListRawArchiveRoots(f.t.Context())
	require.NoError(f.t, err)
	var root db.RawArchiveRoot
	for _, item := range roots {
		if item.OriginalPath == home && item.Provider == "codex" {
			root = item
		}
	}
	require.NotEmpty(f.t, root.ID)
	require.Equal(f.t, lineageDevice, root.DeviceID)
	source, err := f.saved.RawArchiveSourceByKey(f.t.Context(), root.ID, "codex:"+key)
	require.NoError(f.t, err)
	require.NotNil(f.t, source)
	require.Equal(f.t, "codex:"+key, source.SourceKey)
	require.Equal(f.t, original, source.OriginalPath)
	archiveID, err := f.saved.GetArchiveID(f.t.Context())
	require.NoError(f.t, err)
	identity, err := rawsync.NewAuthIdentity(archiveID, lineageDevice)
	require.NoError(f.t, err)
	manifest, err := rawsync.ParseCanonicalManifest(identity, source.ManifestID, source.CanonicalJSON, rawsync.DefaultManifestLimits())
	require.NoError(f.t, err)
	return root, source, manifest
}

func requireLineageEntry(t *testing.T, manifest rawsync.CanonicalManifest, name, content string) {
	t.Helper()
	var matches []rawsync.Entry
	for _, entry := range manifest.Manifest.Entries {
		if filepath.Base(entry.Path) == name {
			matches = append(matches, entry)
		}
	}
	require.Len(t, matches, 1)
	require.Equal(t, int64(len(content)), matches[0].Length)
	require.Equal(t, []rawsync.ObjectRef{{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content))), Length: int64(len(content))}}, matches[0].Objects)
}

func (f *snapshotLineageFixture) inventory(root db.RawArchiveRoot, name, content string) {
	f.t.Helper()
	files, err := f.saved.ListRawArchiveFiles(f.t.Context(), 0, 1000)
	require.NoError(f.t, err)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	count := 0
	for _, file := range files {
		if file.SHA256 == hash {
			count++
			require.Equal(f.t, root.ID, file.RootID)
			require.Equal(f.t, name, filepath.Base(file.Path))
			require.Equal(f.t, int64(len(content)), file.Size)
		}
	}
	require.Equal(f.t, 1, count)
}

func requireOldCodexDeferred(t *testing.T, err error, dataDir, oldName string) {
	t.Helper()
	require.Error(t, err)
	_, ok := err.(archivesource.DrainError)
	require.True(t, ok, "expected an attachment DrainError")
	for {
		require.NotNil(t, err)
		if retry, ok := err.(interface {
			ReconciliationRetryDeferOnly() bool
			ReconciliationRetryPaths() []string
			ReconciliationRetryRoots() []string
			ReconciliationRetryOverflow() bool
		}); ok {
			require.True(t, retry.ReconciliationRetryDeferOnly())
			require.Empty(t, retry.ReconciliationRetryRoots())
			require.False(t, retry.ReconciliationRetryOverflow())
			paths := retry.ReconciliationRetryPaths()
			require.Len(t, paths, 1)
			rel, err := filepath.Rel(filepath.Join(dataDir, "archive-sources", "scratch"), paths[0])
			require.NoError(t, err)
			require.True(t, filepath.IsLocal(rel))
			parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
			require.Len(t, parts, 6)
			require.True(t, strings.HasPrefix(parts[0], "agentsview-raw-"))
			require.NotEqual(t, "agentsview-raw-", parts[0])
			require.Equal(t, []string{"sessions", "2025", "01", "01", oldName}, parts[1:])
			return
		}
		switch wrapped := err.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			require.Len(t, children, 1)
			require.NotNil(t, children[0])
			err = children[0]
		case interface{ Unwrap() error }:
			err = wrapped.Unwrap()
		default:
			require.FailNow(t, "unexpected deferred-ingest error leaf")
		}
	}
}

func (f *snapshotLineageFixture) materialize(key string, homes []string, names, contents []string, oldSource *db.RawArchiveSource) {
	f.t.Helper()
	ctx := f.t.Context()
	archiveID, err := f.saved.GetArchiveID(ctx)
	require.NoError(f.t, err)
	repository := filepath.Join(f.root, "repository")
	report, err := f.archive.Backup(ctx, repository, f.settings, "fixture")
	require.NoError(f.t, err)
	require.NoError(f.t, f.archive.Close())
	require.NoError(f.t, f.saved.Close())
	for _, home := range homes {
		require.NoError(f.t, os.RemoveAll(home))
	}
	receiver := dbtest.OpenTestDB(f.t)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(f.t, err)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}}}
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	f.t.Cleanup(engine.Close)
	manager, err := archivesource.NewManager(ctx, cfg, receiver, engine, nil)
	require.NoError(f.t, err)
	f.t.Cleanup(manager.Close)
	stats, err := manager.Drain(ctx, nil)
	if oldSource == nil {
		require.NoError(f.t, err)
	} else {
		require.Equal(f.t, "codex:"+lineageC, oldSource.SourceKey)
		require.Equal(f.t, 1, stats.Deferred)
		require.Zero(f.t, stats.Failed)
		require.False(f.t, stats.Aborted)
		requireOldCodexDeferred(f.t, err, cfg.DataDir, "rollout-2025-01-01T00-00-00-"+lineageC+".jsonl")
	}
	id := lineageDevice + "~codex:" + key
	session, err := receiver.GetSessionFull(ctx, id)
	require.NoError(f.t, err)
	require.NotNil(f.t, session)
	if oldSource != nil {
		require.Equal(f.t, lineageDevice+"~codex:"+lineageC, session.ID)
		require.Equal(f.t, lineageDevice, session.Machine)
		owners, err := receiver.ArchiveSessionOwners(ctx, id)
		require.NoError(f.t, err)
		require.Len(f.t, owners, 1)
		require.Equal(f.t, db.ArchiveSessionProvenance{
			ArchiveSourceProvenance: db.ArchiveSourceProvenance{
				Attachment: db.ArchiveAttachmentKey{RepositoryID: report.RepositoryID, SnapshotID: report.SnapshotID},
				Source:     db.ArchiveSourceIdentity{ArchiveID: archiveID, DeviceID: lineageDevice, RootID: oldSource.RootID, SourceKey: oldSource.SourceKey},
				ManifestID: oldSource.ManifestID,
			},
			NativeSessionID: "codex:" + lineageC,
		}, owners[0])
	}
	home := f.t.TempDir()
	f.t.Setenv("CODEX_HOME", home)
	f.t.Setenv("CODEX_SESSIONS_DIR", "")
	result, err := Materialize(ctx, cfg, receiver, session, f.t.TempDir())
	require.NoError(f.t, err)
	require.Equal(f.t, id, result.RequestedID)
	require.Equal(f.t, "snapshot:"+report.SnapshotID, result.Source)
	require.Equal(f.t, names[len(names)-1], result.InstalledPage)
	require.Len(f.t, result.Paths, len(names))
	for i, path := range result.Paths {
		require.Equal(f.t, names[i], filepath.Base(path))
		got, err := os.ReadFile(path)
		require.NoError(f.t, err)
		require.Equal(f.t, contents[i], string(got))
	}
	var installed []string
	require.NoError(f.t, filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			installed = append(installed, entry.Name())
		}
		return nil
	}))
	require.ElementsMatch(f.t, names, installed, "intermediate pages are provenance only")
}

func TestMaterializeCodexSnapshotLaterNewestPage(t *testing.T) {
	f := newSnapshotLineageFixture(t)
	child, parent := filepath.Join(f.root, "child"), filepath.Join(f.root, "parent")
	pName := "rollout-2024-01-15T00-00-00-" + lineageP + ".jsonl"
	oldName := "rollout-2025-01-01T00-00-00-" + lineageC + ".jsonl"
	newName := "rollout-2026-01-01T00-00-00-" + lineageC + "_" + lineageR + ".jsonl"
	pPath := filepath.Join(parent, "sessions", "2024", "01", "15", pName)
	oldPath := filepath.Join(child, "sessions", "2025", "01", "01", oldName)
	newPath := filepath.Join(child, "sessions", "2026", "01", "01", newName)
	p := lineagePage(lineageP, "2024-01-15T00:00:00Z", "parent", 0, map[string]any{})
	old := lineagePage(lineageC, "2025-01-01T00:00:00Z", "old child", 2, map[string]any{"forked_from_id": lineageP, "forked_from_ordinal_exclusive": 2, "history_base": lineageBase(lineageP, 2, len(p))})
	newPage := lineagePage(lineageC, "2026-01-01T00:00:00Z", "new child", 2, map[string]any{"forked_from_id": lineageP, "forked_from_ordinal_exclusive": 2, "history_base": lineageBase(lineageP, 2, len(p))})
	dbtest.WriteTestFile(t, oldPath, []byte(old))
	first := f.capture("first", "", child)
	f.open(first)
	firstRoot, firstSource, firstManifest := f.accepted(child, lineageC, oldPath)
	requireLineageEntry(t, firstManifest, oldName, old)
	require.Len(t, firstManifest.Manifest.Entries, 1)
	firstManifestID := firstSource.ManifestID
	dbtest.WriteTestFile(t, pPath, []byte(p))
	dbtest.WriteTestFile(t, newPath, []byte(newPage))
	second := f.capture("second", first, child, parent)
	spec, err := rawarchive.LoadImportSpec(t.Context(), second)
	require.NoError(t, err)
	_, err = f.archive.Import(t.Context(), spec)
	require.NoError(t, err)
	childRoot, oldSource, oldManifest := f.accepted(child, lineageC, oldPath)
	require.Equal(t, firstRoot.ID, childRoot.ID)
	require.Equal(t, firstManifestID, oldSource.ManifestID)
	requireLineageEntry(t, oldManifest, oldName, old)
	require.Len(t, oldManifest.Manifest.Entries, 1)
	_, _, newManifest := f.accepted(child, lineageC+"_"+lineageR, newPath)
	requireLineageEntry(t, newManifest, newName, newPage)
	requireLineageEntry(t, newManifest, pName, p)
	parentRoot, _, _ := f.accepted(parent, lineageP, pPath)
	require.NotEqual(t, childRoot.ID, parentRoot.ID)
	f.inventory(childRoot, newName, newPage)
	f.inventory(parentRoot, pName, p)
	f.materialize(lineageC, []string{child, parent}, []string{pName, newName}, []string{p, newPage}, oldSource)
}
