package sessionmaterialize

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestMaterializeCodexConfigured(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const page = "22222222-2222-4222-8222-222222222222"
	home, root, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cfg := config.Config{DataDir: root, InstallationID: "local", LocalMachineName: "local"}
	location := filepath.Join(root, "archived_sessions", "source", "sessions")
	oldName := "rollout-2026-01-01T01-00-00-" + id + ".jsonl"
	newName := "rollout-2026-01-02T01-00-00-" + id + "_" + page + ".jsonl"
	stored := filepath.Join(location, "2026", "01", "01", oldName)
	session := &db.Session{ID: "codex:" + id, Agent: "codex", Machine: "local", FilePath: &stored}
	cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {location}}
	oldPath := filepath.Join(location, "2026", "01", "01", oldName)
	newPath := filepath.Join(location, "2026", "01", "02", newName)
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Dir(newPath), 0o700))
	old := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", id))
	newPage := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", id, id, len(old)))
	require.NoError(t, os.WriteFile(oldPath, old, 0o600))
	require.NoError(t, os.WriteFile(newPath, newPage, 0o600))
	result, err := Materialize(t.Context(), cfg, nil, session, cwd)
	require.NoError(t, err)
	assert.Equal(t, session.ID, result.RequestedID)
	assert.Equal(t, newName, result.InstalledPage)
	assert.Equal(t, "installed", result.Status)
	for index, want := range [][]byte{old, newPage} {
		got, err := os.ReadFile(result.Paths[index])
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	installedPaths := result.Paths
	t.Run("relabeled recorded source", func(t *testing.T) {
		relabeled := *session
		relabeled.Machine = "old-label"
		result, err := Materialize(t.Context(), cfg, nil, &relabeled, cwd)
		require.NoError(t, err)
		assert.Equal(t, installedPaths, result.Paths)
		missing := filepath.Join(location, "missing", oldName)
		relabeled.FilePath = &missing
		_, err = Materialize(t.Context(), cfg, nil, &relabeled, cwd)
		require.ErrorContains(t, err, "outside configured roots")
	})
	t.Run("recorded directory alias", func(t *testing.T) {
		alias := filepath.Join(t.TempDir(), "source-link")
		if err := os.Symlink(location, alias); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		linked := *session
		linked.FilePath = new(filepath.Join(alias, "2026", "01", "01", oldName))
		result, err := Materialize(t.Context(), cfg, nil, &linked, cwd)
		require.NoError(t, err)
		assert.Equal(t, "unchanged", result.Status)
		assert.Equal(t, installedPaths, result.Paths)
		outside := filepath.Join(t.TempDir(), oldName)
		require.NoError(t, os.WriteFile(outside, old, 0o600))
		linked.FilePath = &outside
		_, err = Materialize(t.Context(), cfg, nil, &linked, cwd)
		require.ErrorContains(t, err, "outside configured roots")
	})
	// A publisher failure unrelated to rollout matching reaches the caller.
	blockedHome := t.TempDir()
	t.Setenv("CODEX_HOME", blockedHome)
	require.NoError(t, os.Mkdir(filepath.Join(blockedHome, "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(blockedHome, "sessions", "2026"), []byte("block"), 0o600))
	_, err = Materialize(t.Context(), cfg, nil, session, cwd)
	require.ErrorContains(t, err, "2026")
}

func TestMaterializeCodexMirror(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const page = "22222222-2222-4222-8222-222222222222"
	home, root, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cfg := config.Config{DataDir: root, InstallationID: "local", LocalMachineName: "local"}
	oldName := "rollout-2026-01-01T01-00-00-" + id + ".jsonl"
	newName := "rollout-2026-01-02T01-00-00-" + id + "_" + page + ".jsonl"
	cfg.RemoteHosts = []config.RemoteHost{{Host: "remote"}}
	stored := "remote:/archived_sessions/source/sessions/2026/01/01/" + oldName
	session := &db.Session{ID: "remote~codex:" + id, Agent: "codex", Machine: "remote", FilePath: &stored}
	location := filepath.Join(remotesync.MirrorDir(root, "remote"), "archived_sessions", "source", "sessions")
	sidecar := remotesync.MirrorDir(root, "remote") + ".codex-roots.json"
	require.NoError(t, os.MkdirAll(filepath.Dir(sidecar), 0o700))
	require.NoError(t, os.WriteFile(sidecar, []byte(`{"version":1,"dirs":["/archived_sessions/source/sessions","/archived_sessions/source/archived_sessions"]}`), 0o600))
	oldPath := filepath.Join(location, "2026", "01", "01", oldName)
	newPath := filepath.Join(location, "2026", "01", "02", newName)
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Dir(newPath), 0o700))
	old := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", id))
	newPage := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", id, id, len(old)))
	require.NoError(t, os.WriteFile(oldPath, old, 0o600))
	require.NoError(t, os.WriteFile(newPath, newPage, 0o600))
	result, err := Materialize(t.Context(), cfg, nil, session, cwd)
	require.NoError(t, err)
	assert.Equal(t, session.ID, result.RequestedID)
	assert.Equal(t, newName, result.InstalledPage)
	assert.Equal(t, "installed", result.Status)
	for index, want := range [][]byte{old, newPage} {
		got, err := os.ReadFile(result.Paths[index])
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	originalSidecar, err := os.ReadFile(sidecar)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, metadata, page, failure string
	}{
		{"separate day roots", `{"version":1,"dirs":["/archived_sessions/source/sessions/2026/01/01","/archived_sessions/source/sessions/2026/01/02"]}`, oldName, ""},
		{"containing root wins", `{"version":1,"dirs":["/archived_sessions/source/sessions/2026/01/01","/archived_sessions/source/sessions"]}`, newName, ""},
		{"unadvertised", `{"version":1,"dirs":["/elsewhere"]}`, "", "outside Codex roots"},
		{"newer version", `{"version":2,"dirs":[]}`, "", "compatible or newer AgentsView"},
		{"invalid metadata", `{`, "", "metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			require.NoError(t, os.WriteFile(sidecar, []byte(tc.metadata), 0o600))
			got, err := Materialize(t.Context(), cfg, nil, session, cwd)
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.page, got.InstalledPage)
			}
		})
	}
	t.Run("missing sidecar", func(t *testing.T) {
		require.NoError(t, os.Remove(sidecar))
		_, err := Materialize(t.Context(), cfg, nil, session, cwd)
		require.ErrorContains(t, err, "sync the host if metadata is missing or outdated")
	})
	archive := filepath.Join(filepath.Dir(location), "archived_sessions")
	archivedPath := filepath.Join(archive, oldName)
	for _, tc := range []struct {
		name                             string
		advertiseSibling, compressedLive bool
	}{
		{"sibling not advertised", false, false},
		{"archived ancestor", true, false},
		{"live compressed before archived plain", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			require.NoError(t, os.MkdirAll(archive, 0o700))
			require.NoError(t, os.RemoveAll(archivedPath))
			require.NoError(t, os.RemoveAll(oldPath))
			require.NoError(t, os.RemoveAll(oldPath+".zst"))
			require.NoError(t, os.WriteFile(archivedPath, old, 0o600))
			metadata := originalSidecar
			if !tc.advertiseSibling {
				metadata = []byte(`{"version":1,"dirs":["/archived_sessions/source/sessions"]}`)
			}
			require.NoError(t, os.WriteFile(sidecar, metadata, 0o600))
			want := old
			if tc.compressedLive {
				encoder, err := zstd.NewWriter(nil)
				require.NoError(t, err)
				want = encoder.EncodeAll(old, nil)
				encoder.Close()
				require.NoError(t, os.WriteFile(oldPath+".zst", want, 0o600))
			}
			result, err := Materialize(t.Context(), cfg, nil, session, cwd)
			if !tc.advertiseSibling {
				require.ErrorContains(t, err, "missing Codex ancestor")
				return
			}
			require.NoError(t, err)
			got, err := os.ReadFile(result.Paths[0])
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestMaterializeCodexS3(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const root = "s3://test/host/raw/codex/sessions"
	const archivedRoot = "s3://test/host/raw/codex/archived_sessions"
	const key = "host/raw/codex/sessions/2026/01/01/rollout-2026-01-01T01-00-00-" + id + ".jsonl"
	const parent = "22222222-2222-4222-8222-222222222222"
	const parentKey = "host/raw/codex/archived_sessions/rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	parentContent := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", parent)
	content := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", id, parent, len(parentContent))
	var corrupt atomic.Bool
	var unsafeSelected atomic.Bool
	var unsafeFetched atomic.Bool
	unsafeKey := "host/raw/codex/sessions/2026/01/01/rollout-x\\..\\..\\..\\..\\escape-" + id + ".jsonl"
	unrelatedKey := "host/raw/codex/sessions/2026/01/01/rollout-x\\..\\escape-33333333-3333-4333-8333-333333333333.jsonl"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			if strings.Contains(r.URL.Query().Get("prefix"), "archived_sessions") {
				_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size></Contents></ListBucketResult>`, parentKey, len(parentContent))
			} else {
				listed := key
				if unsafeSelected.Load() {
					listed += ".zst"
				}
				_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size></Contents><Contents><Key>%s</Key><Size>10</Size></Contents>`, listed, len(content), unrelatedKey)
				if unsafeSelected.Load() {
					_, _ = fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>10</Size></Contents>`, unsafeKey)
				}
				_, _ = io.WriteString(w, `</ListBucketResult>`)
			}
			return
		}
		switch r.URL.Path {
		case "/test/" + key:
			if corrupt.Load() {
				_, _ = io.WriteString(w, "corrupt")
				return
			}
			_, _ = io.WriteString(w, content)
		case "/test/" + parentKey:
			_, _ = io.WriteString(w, parentContent)
		default:
			unsafeFetched.Store(true)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("AWS_S3_ENDPOINT", server.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AWS_SESSION_TOKEN", "fixture")
	t.Setenv("CODEX_HOME", t.TempDir())
	uri := "s3://test/" + key
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root, archivedRoot}}}
	session := &db.Session{ID: "host~codex:" + id, Agent: "codex", Machine: "host", FilePath: &uri}
	bad := *session
	bad.Machine = "other"
	bad.ID = "other~codex:" + id
	_, err := Materialize(t.Context(), cfg, nil, &bad, t.TempDir())
	require.ErrorContains(t, err, "source identity differs")
	bad = *session
	bad.ID = "remote~codex:" + id
	_, err = Materialize(t.Context(), cfg, nil, &bad, t.TempDir())
	require.ErrorContains(t, err, "source host differs")
	result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.NoError(t, err)
	require.Len(t, result.Paths, 2)
	got, err := os.ReadFile(result.Paths[0])
	require.NoError(t, err)
	assert.Equal(t, parentContent, string(got))
	got, err = os.ReadFile(result.Paths[1])
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
	unsafeSelected.Store(true)
	unsafeHome := t.TempDir()
	t.Setenv("CODEX_HOME", unsafeHome)
	_, err = Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.ErrorContains(t, err, "invalid selected Codex S3 path")
	assert.False(t, unsafeFetched.Load())
	unsafeSelected.Store(false)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	corrupt.Store(true)
	_, err = Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.ErrorContains(t, err, "metadata")
}

func TestMaterializeCodexCustomMirrorScope(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const pageID = "22222222-2222-4222-8222-222222222222"
	data := t.TempDir()
	mirror := remotesync.MirrorDir(data, "remote")
	oldName := "rollout-2026-01-01T00-00-00-" + id + ".jsonl"
	newName := "rollout-2026-01-02T00-00-00-" + id + "_" + pageID + ".jsonl"
	old := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", id))
	newPage := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", id, id, len(old)))
	for name, body := range map[string][]byte{
		filepath.Join(mirror, "x", "custom", "2026", "01", "01", oldName): old,
		filepath.Join(mirror, "x", "custom", "2026", "01", "02", newName): newPage,
		filepath.Join(mirror, "x", "other", "2026", "01", "02", newName):  []byte("unrelated conflict"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
		require.NoError(t, os.WriteFile(name, body, 0o600))
	}
	require.NoError(t, os.WriteFile(mirror+".codex-roots.json", []byte(`{"version":1,"dirs":["/x/custom","/x/other"]}`), 0o600))
	t.Setenv("CODEX_HOME", t.TempDir())
	stored := "remote:/x/custom/2026/01/01/" + oldName
	session := &db.Session{ID: "remote~codex:" + id, Agent: "codex", Machine: "remote", FilePath: &stored}
	cfg := config.Config{DataDir: data, RemoteHosts: []config.RemoteHost{{Host: "remote"}}}
	result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, newName, result.InstalledPage)
	require.Len(t, result.Paths, 2)
	got, err := os.ReadFile(result.Paths[1])
	require.NoError(t, err)
	assert.Equal(t, newPage, got)
}

func TestMaterializeCodexLoadedLinkedHome(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const parent = "22222222-2222-4222-8222-222222222222"
	base := t.TempDir()
	profile, live := filepath.Join(base, "profile"), filepath.Join(base, "storage", "live")
	archive := filepath.Join(profile, "archived_sessions")
	require.NoError(t, os.MkdirAll(profile, 0o700))
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.MkdirAll(archive, 0o700))
	if err := os.Symlink(live, filepath.Join(profile, "sessions")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	t.Setenv("AGENTSVIEW_DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("CODEX_HOME", profile)
	t.Setenv("CODEX_SESSIONS_DIR", "")
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	config.RegisterServeFlags(fs)
	require.NoError(t, fs.Parse(nil))
	cfg, err := config.Load(fs)
	require.NoError(t, err)
	parentBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", parent))
	parentPath := filepath.Join(archive, "rollout-2024-01-15T00-00-00-"+parent+".jsonl")
	require.NoError(t, os.WriteFile(parentPath, parentBytes, 0o600))
	page := filepath.Join(live, "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	pageBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", id, parent, len(parentBytes)))
	require.NoError(t, os.WriteFile(page, pageBytes, 0o600))
	session := &db.Session{ID: "codex:" + id, Agent: "codex", Machine: cfg.InstallationID, FilePath: &page}
	t.Setenv("CODEX_HOME", filepath.Join(base, "destination"))
	result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.NoError(t, err)
	require.Len(t, result.Paths, 2)
	got, err := os.ReadFile(result.Paths[0])
	require.NoError(t, err)
	assert.Equal(t, parentBytes, got)
}

func TestMaterializeCodexMissingAndCrossRoot(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	root := t.TempDir()
	missing := filepath.Join(root, "2026", "01", "01", "rollout-2026-01-01T01-00-00-"+id+".jsonl")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}}
	session := &db.Session{ID: "codex:" + id, Agent: "codex", Machine: "local", FilePath: &missing}
	_, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.ErrorContains(t, err, "unavailable")

	// The requested page can refer to a parent in a configured sibling archive.
	const parent = "22222222-2222-4222-8222-222222222222"
	base, home := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	live, archived := filepath.Join(base, "sessions"), filepath.Join(base, "archived_sessions")
	page := filepath.Join(live, "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	ancestor := filepath.Join(archived, "rollout-2024-01-15T00-00-00-"+parent+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(page), 0o700))
	require.NoError(t, os.MkdirAll(archived, 0o700))
	ancestorContent := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", parent)
	require.NoError(t, os.WriteFile(page, []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", id, parent, len(ancestorContent))), 0o600))
	require.NoError(t, os.WriteFile(ancestor, []byte(ancestorContent), 0o600))
	cfg.AgentDirs[parser.AgentCodex] = []string{live, archived}
	session.FilePath = &page
	// An identical same-rollout copy in the other native tree is safe.
	identical := filepath.Join(home, "archived_sessions", "other-date", filepath.Base(ancestor))
	require.NoError(t, os.MkdirAll(filepath.Dir(identical), 0o700))
	ancestorBytes, err := os.ReadFile(ancestor)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(identical, ancestorBytes, 0o600))
	_, err = Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.NoError(t, err)
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	// A different native representation of the ancestor must fail before any
	// requested page is installed, even under the sibling archive tree.
	conflict := filepath.Join(home, "archived_sessions", "other-date", filepath.Base(ancestor)+".zst")
	require.NoError(t, os.MkdirAll(filepath.Dir(conflict), 0o700))
	require.NoError(t, os.WriteFile(conflict, []byte("different"), 0o600))
	_, err = Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.ErrorContains(t, err, "differs")
	_, err = os.Stat(filepath.Join(home, "sessions"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.Remove(conflict))
	result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.NoError(t, err)
	require.Len(t, result.Paths, 2)
	assert.Equal(t, filepath.Base(ancestor), filepath.Base(result.Paths[0]))

	t.Run("linked native roots", func(t *testing.T) {
		home, targetLive, targetArchive := t.TempDir(), t.TempDir(), t.TempDir()
		t.Setenv("CODEX_HOME", home)
		if err := os.Symlink(targetLive, filepath.Join(home, "sessions")); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		require.NoError(t, os.Symlink(targetArchive, filepath.Join(home, "archived_sessions")))
		conflict := filepath.Join(targetArchive, filepath.Base(ancestor))
		require.NoError(t, os.WriteFile(conflict, []byte("different"), 0o600))
		_, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
		require.ErrorContains(t, err, "differs")
		require.NoError(t, os.Remove(conflict))
		result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
		require.NoError(t, err)
		require.Len(t, result.Paths, 2)
		got, err := os.ReadFile(filepath.Join(targetLive, "2024", "01", "15", filepath.Base(ancestor)))
		require.NoError(t, err)
		assert.Equal(t, ancestorBytes, got)
	})
}
