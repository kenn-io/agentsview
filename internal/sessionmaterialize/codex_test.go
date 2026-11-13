package sessionmaterialize

import (
	"encoding/json/v2"
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
	t.Run("nested machine lineage", func(t *testing.T) {
		t.Setenv("CODEX_HOME", t.TempDir())
		const foreignID = "33333333-3333-4333-8333-333333333333"
		foreignRoot := filepath.Join(location, "2026", "01", "03")
		require.NoError(t, os.MkdirAll(foreignRoot, 0o700))
		t.Cleanup(func() { assert.NoError(t, os.RemoveAll(foreignRoot)) })
		foreignPage := filepath.Join(foreignRoot, "rollout-2026-01-03T01-00-00-"+id+".jsonl")
		foreignBytes := []byte(`{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","history_mode":"paginated","label":"machine-b"}}` + "\n")
		require.NoError(t, os.WriteFile(foreignPage, foreignBytes, 0o600))
		scoped := cfg
		scoped.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {location, foreignRoot}}
		scoped.SourceMachines = map[parser.AgentType]map[string]string{parser.AgentCodex: {location: "machine-a", foreignRoot: "machine-b"}}
		selected := *session
		selected.Machine = "machine-a"
		got, err := Materialize(t.Context(), scoped, nil, &selected, cwd)
		require.NoError(t, err)
		assert.Equal(t, newName, got.InstalledPage)
		require.Len(t, got.Paths, 2)
		first, err := os.ReadFile(got.Paths[0])
		require.NoError(t, err)
		assert.Equal(t, old, first)
		last, err := os.ReadFile(got.Paths[1])
		require.NoError(t, err)
		assert.Equal(t, newPage, last)
		selected.Machine = "old-label"
		got, err = Materialize(t.Context(), scoped, nil, &selected, cwd)
		require.NoError(t, err)
		assert.Equal(t, newName, got.InstalledPage)
		missing := filepath.Join(location, "missing", oldName)
		selected.FilePath = &missing
		_, err = Materialize(t.Context(), scoped, nil, &selected, cwd)
		require.ErrorContains(t, err, "outside configured roots")

		foreignAncestor := filepath.Join(foreignRoot, "rollout-2026-01-03T02-00-00-"+foreignID+".jsonl")
		ancestorBytes := []byte(`{"type":"session_meta","payload":{"id":"33333333-3333-4333-8333-333333333333","history_mode":"paginated","label":"machine-b ancestor"}}` + "\n")
		require.NoError(t, os.WriteFile(foreignAncestor, ancestorBytes, 0o600))
		localNeedsForeign := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", id, foreignID, len(ancestorBytes)))
		require.NoError(t, os.WriteFile(newPath, localNeedsForeign, 0o600))
		t.Cleanup(func() { assert.NoError(t, os.WriteFile(newPath, newPage, 0o600)) })
		selected.Machine = "machine-a"
		selected.FilePath = &stored
		_, err = Materialize(t.Context(), scoped, nil, &selected, cwd)
		require.ErrorContains(t, err, "missing Codex ancestor")
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
		{"invalid metadata", `{`, "", "source locations"},
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
		require.ErrorContains(t, err, "sync the host if locations are missing or outdated")
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
	var foreignListed atomic.Bool
	unsafeKey := "host/raw/codex/sessions/2026/01/01/rollout-x\\..\\..\\..\\..\\escape-" + id + ".jsonl"
	unrelatedKey := "host/raw/codex/sessions/2026/01/01/rollout-x\\..\\escape-33333333-3333-4333-8333-333333333333.jsonl"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			switch r.URL.Query().Get("prefix") {
			case "host/raw/codex/archived_sessions/":
				_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size></Contents></ListBucketResult>`, parentKey, len(parentContent))
			case "other/raw/codex/archived_sessions/":
				foreignListed.Store(true)
				_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size></Contents></ListBucketResult>`, strings.Replace(parentKey, "host/", "other/", 1), len(parentContent))
			case "host/raw/codex/sessions/":
				listed := key
				if unsafeSelected.Load() {
					listed += ".zst"
				}
				_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size></Contents><Contents><Key>%s</Key><Size>10</Size></Contents>`, listed, len(content), unrelatedKey)
				if unsafeSelected.Load() {
					_, _ = fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>10</Size></Contents>`, unsafeKey)
				}
				_, _ = io.WriteString(w, `</ListBucketResult>`)
			default:
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
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
	for _, tc := range []struct {
		name, live, archived string
	}{
		{"no slash", root, archivedRoot},
		{"live slash", root + "/", archivedRoot},
		{"archive slash", root, archivedRoot + "/"},
		{"both slash", root + "/", archivedRoot + "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {tc.live, tc.archived}}}
			result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
			require.NoError(t, err)
			require.Len(t, result.Paths, 2)
			got, err := os.ReadFile(result.Paths[0])
			require.NoError(t, err)
			assert.Equal(t, parentContent, string(got))
			got, err = os.ReadFile(result.Paths[1])
			require.NoError(t, err)
			assert.Equal(t, content, string(got))
		})
	}
	t.Run("foreign ancestor", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CODEX_HOME", home)
		cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root, "s3://test/other/raw/codex/archived_sessions/"}}}
		_, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
		require.ErrorContains(t, err, "missing Codex ancestor")
		assert.False(t, foreignListed.Load())
		assert.False(t, unsafeFetched.Load())
		entries, err := os.ReadDir(home)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
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
	t.Run("missing root beneath escaping symlink", func(t *testing.T) {
		escape := filepath.Join(mirror, "x", "escape")
		if err := os.Symlink(t.TempDir(), escape); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		require.NoError(t, os.WriteFile(mirror+".codex-roots.json", []byte(`{"version":1,"dirs":["/x/custom","/x/escape/missing"]}`), 0o600))
		home := t.TempDir()
		t.Setenv("CODEX_HOME", home)
		_, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
		require.ErrorContains(t, err, "outside the mirror")
		entries, err := os.ReadDir(home)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
}

func TestMaterializeCodexLoadedLinkedHome(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const parent = "22222222-2222-4222-8222-222222222222"
	base := t.TempDir()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "AGENTSVIEW_") || strings.HasPrefix(name, "AGENT_VIEWER_") {
			t.Setenv(name, "")
		}
	}
	for _, def := range parser.Registry {
		for _, name := range []string{def.EnvVar, def.NativeEnvVar, def.DefaultRootEnvVar} {
			if name != "" {
				t.Setenv(name, "")
			}
		}
	}
	for _, name := range []string{"XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "LOCALAPPDATA", "CRUSH_GLOBAL_DATA"} {
		t.Setenv(name, base)
	}
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

	for _, tc := range []struct {
		name                                                    string
		linked, index, missing, missingRoot, shared, standalone bool
	}{
		{name: "linked without index", linked: true},
		{name: "linked missing mirrored active root", linked: true, missingRoot: true},
		{name: "linked with rename index", linked: true, index: true},
		{name: "unlinked control"},
		{name: "parent in unrelated home", linked: true, missing: true},
		{name: "shared live without ancestor", linked: true, shared: true, standalone: true},
		{name: "shared live with archived ancestor", linked: true, shared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			home, other := filepath.Join(root, "home"), filepath.Join(root, "other")
			live, archived := filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")
			if tc.linked {
				live = filepath.Join(root, "storage", "live")
			}
			for _, dir := range []string{home, live, archived, filepath.Join(other, "archived_sessions")} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
			}
			if tc.linked {
				if err := os.Symlink(live, filepath.Join(home, "sessions")); err != nil {
					t.Skipf("directory symlinks unavailable: %v", err)
				}
			}
			if tc.shared {
				if err := os.Symlink(live, filepath.Join(other, "sessions")); err != nil {
					t.Skipf("directory symlinks unavailable: %v", err)
				}
			}
			parentData := []byte(lineagePage(parent, "2024-01-15T00:00:00Z", "parent", 0, map[string]any{}))
			childData := []byte(lineagePage(id, "2026-01-01T00:00:00Z", "child", 2, map[string]any{
				"forked_from_id": parent, "forked_from_ordinal_exclusive": 2,
				"history_base": lineageBase(parent, 2, len(parentData)),
			}))
			if tc.standalone {
				childData = []byte(lineagePage(id, "2026-01-01T00:00:00Z", "child", 0, map[string]any{}))
			}
			parentDir := archived
			if tc.shared {
				parentDir = filepath.Join(other, "archived_sessions")
			}
			if tc.missing {
				parentDir = filepath.Join(other, "archived_sessions")
				const replacement = "33333333-3333-4333-8333-333333333333"
				p := filepath.Join(archived, "rollout-2024-01-16T00-00-00-"+replacement+".jsonl")
				require.NoError(t, os.WriteFile(p, []byte(lineagePage(replacement, "2024-01-16T00:00:00Z", "other", 0, map[string]any{})), 0o600))
			}
			parentFile := filepath.Join(parentDir, "rollout-2024-01-15T00-00-00-"+parent+".jsonl")
			childFile := filepath.Join(live, "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(childFile), 0o700))
			require.NoError(t, os.WriteFile(parentFile, parentData, 0o600))
			require.NoError(t, os.WriteFile(childFile, childData, 0o600))
			const newRollout = "33333333-3333-4333-8333-333333333333"
			newName := "rollout-2026-01-02T00-00-00-" + id + "_" + newRollout + ".jsonl"
			newFile := filepath.Join(archived, newName)
			newData := []byte(lineagePage(id, "2026-01-02T00:00:00Z", "newer child", 2, map[string]any{
				"forked_from_id": parent, "forked_from_ordinal_exclusive": 2,
				"history_base": lineageBase(parent, 2, len(parentData)),
			}))
			if tc.missingRoot {
				require.NoError(t, os.WriteFile(newFile, newData, 0o600))
			}
			if tc.index {
				index := fmt.Sprintf("{\"id\":%q,\"thread_name\":\"renamed child\"}\n", id)
				require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexSessionIndexFilename), []byte(index), 0o600))
			}
			data := filepath.Join(root, "remote-config")
			require.NoError(t, os.MkdirAll(data, 0o700))
			configText := fmt.Sprintf("[agents.codex]\nhomes = [%q, %q]\n", home, other)
			var unrelatedParent string
			var unrelatedData []byte
			if tc.shared {
				unrelated := filepath.Join(root, "unrelated")
				unrelatedArchive := filepath.Join(unrelated, "archived_sessions")
				require.NoError(t, os.MkdirAll(filepath.Join(unrelated, "sessions"), 0o700))
				require.NoError(t, os.MkdirAll(unrelatedArchive, 0o700))
				unrelatedParent = filepath.Join(unrelatedArchive, "rollout-2025-01-15T00-00-00-"+parent+".jsonl")
				unrelatedData = []byte(lineagePage(parent, "2025-01-15T00:00:00Z", "unrelated", 0, map[string]any{}))
				require.NoError(t, os.WriteFile(unrelatedParent, unrelatedData, 0o600))
				configText = fmt.Sprintf("[agents.codex]\nhomes = [%q, %q, %q]\n", home, other, unrelated)
			}
			require.NoError(t, os.WriteFile(filepath.Join(data, "config.toml"), []byte(configText), 0o600))
			t.Setenv("AGENTSVIEW_DATA_DIR", data)
			t.Setenv("CODEX_HOME", home)
			fs := flag.NewFlagSet("remote fixture", flag.ContinueOnError)
			config.RegisterServeFlags(fs)
			require.NoError(t, fs.Parse(nil))
			remoteCfg, err := config.Load(fs)
			require.NoError(t, err)
			for agent := range remoteCfg.AgentDirs {
				if agent != parser.AgentCodex {
					delete(remoteCfg.AgentDirs, agent)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := remotesync.ValidateProtocolHeader(r.Header); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				remotesync.SetProtocolHeader(w.Header())
				var response any
				var err error
				switch r.URL.Path {
				case "/api/v1/remote-sync/targets":
					response, err = remotesync.ResolveTargets(r.Context(), remoteCfg)
				case "/api/v1/remote-sync/manifest":
					var request remotesync.TargetSet
					if err = json.UnmarshalRead(r.Body, &request); err == nil {
						response, err = remotesync.ServeManifest(r.Context(), remoteCfg, request)
					}
				case "/api/v1/remote-sync/archive":
					var request remotesync.ArchiveRequest
					if err = json.UnmarshalRead(r.Body, &request); err == nil {
						var plan remotesync.ArchivePlan
						plan, err = remotesync.PlanArchive(r.Context(), remoteCfg, request)
						if err == nil {
							w.Header().Set("Content-Type", "application/x-tar")
							err = plan.Write(r.Context(), w)
							if err == nil {
								return
							}
						}
					}
				default:
					http.NotFound(w, r)
					return
				}
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, response)
			}))
			t.Cleanup(server.Close)
			localData := filepath.Join(root, "collector")
			database, err := db.Open(t.Context(), filepath.Join(root, "archive.db"))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, database.Close()) })
			sync := remotesync.RemoteSync{Host: "remote", Source: remotesync.HTTPSource{URL: server.URL, Client: server.Client()}, DataDir: localData, DB: database}
			prepared, err := sync.Prepare(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, prepared.Close()) })
			stats, err := prepared.ImportActive(t.Context())
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			require.Zero(t, stats.Deferred)
			require.NoError(t, prepared.Close())
			imported, err := database.GetSessionFull(t.Context(), "remote~codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, imported)
			require.NotNil(t, imported.FilePath)
			require.Equal(t, "remote:"+childFile, *imported.FilePath)
			mirror := remotesync.MirrorDir(localData, "remote")
			if tc.shared {
				metadata, err := remotesync.LoadMirrorCodexMetadata(t.Context(), mirror)
				require.NoError(t, err)
				require.Equal(t, remotesync.CodexHomeRoots{Sessions: live, ArchivedSessions: archived}, metadata.Homes[home])
				require.Equal(t, remotesync.CodexHomeRoots{Sessions: live, ArchivedSessions: filepath.Join(other, "archived_sessions")}, metadata.Homes[other])
			}
			checkSources := func() {
				sources := map[string][]byte{parentFile: parentData, childFile: childData}
				if tc.shared {
					sources[unrelatedParent] = unrelatedData
				}
				for path, want := range sources {
					mirrored, err := remotesync.SafeRemappedRemotePath(mirror, path)
					require.NoError(t, err)
					for _, file := range []string{path, mirrored} {
						got, err := os.ReadFile(file)
						require.NoError(t, err)
						require.Equal(t, want, got)
					}
				}
			}
			checkSources()
			if tc.missingRoot {
				mirroredChild, err := remotesync.SafeRemappedRemotePath(mirror, childFile)
				require.NoError(t, err)
				mirroredLive, err := remotesync.SafeRemappedRemotePath(mirror, live)
				require.NoError(t, err)
				checkArchived := func() {
					t.Helper()
					for path, want := range map[string][]byte{parentFile: parentData, newFile: newData} {
						mirrored, err := remotesync.SafeRemappedRemotePath(mirror, path)
						require.NoError(t, err)
						for _, file := range []string{path, mirrored} {
							got, err := os.ReadFile(file)
							require.NoError(t, err)
							require.Equal(t, want, got)
						}
					}
				}
				checkArchived()
				require.NoError(t, os.Remove(childFile))
				for _, dir := range []string{filepath.Join(live, "2026", "01", "01"), filepath.Join(live, "2026", "01"), filepath.Join(live, "2026")} {
					require.NoError(t, os.Remove(dir))
				}
				next, err := sync.Prepare(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, next.Close()) })
				stats, err := next.ImportActive(t.Context())
				require.NoError(t, err)
				require.Zero(t, stats.Failed)
				require.Zero(t, stats.Deferred)
				require.NoError(t, next.Close())
				imported, err = database.GetSessionFull(t.Context(), "remote~codex:"+id)
				require.NoError(t, err)
				require.NotNil(t, imported)
				require.NotNil(t, imported.FilePath)
				require.Equal(t, "remote:"+childFile, *imported.FilePath)
				for _, path := range []string{mirroredChild, mirroredLive} {
					_, err := os.Stat(path)
					require.ErrorIs(t, err, os.ErrNotExist)
				}
				metadata, err := remotesync.LoadMirrorCodexMetadata(t.Context(), mirror)
				require.NoError(t, err)
				require.Equal(t, remotesync.CodexHomeRoots{Sessions: live, ArchivedSessions: archived}, metadata.Homes[home])
				checkArchived()
				require.NoError(t, os.Remove(live))
				_, err = os.Stat(live)
				require.ErrorIs(t, err, os.ErrNotExist)
				imported, err = database.GetSessionFull(t.Context(), "remote~codex:"+id)
				require.NoError(t, err)
				require.NotNil(t, imported)
				require.NotNil(t, imported.FilePath)
				require.Equal(t, "remote:"+childFile, *imported.FilePath)
				destination := filepath.Join(root, "destination")
				t.Setenv("CODEX_HOME", destination)
				localCfg := config.Config{DataDir: localData, RemoteHosts: []config.RemoteHost{{Host: "remote"}}}
				result, err := Materialize(t.Context(), localCfg, database, imported, t.TempDir())
				checkArchived()
				require.NoError(t, err)
				require.Equal(t, imported.ID, result.RequestedID)
				require.Equal(t, newName, result.InstalledPage)
				parentDest := filepath.Join(destination, "sessions", "2024", "01", "15", filepath.Base(parentFile))
				newDest := filepath.Join(destination, "sessions", "2026", "01", "02", newName)
				require.ElementsMatch(t, []string{parentDest, newDest}, result.Paths)
				for path, want := range map[string][]byte{parentDest: parentData, newDest: newData} {
					got, err := os.ReadFile(path)
					require.NoError(t, err)
					require.Equal(t, want, got)
				}
				return
			}
			destination := filepath.Join(root, "destination")
			t.Setenv("CODEX_HOME", destination)
			localCfg := config.Config{DataDir: localData, RemoteHosts: []config.RemoteHost{{Host: "remote"}}}
			result, err := Materialize(t.Context(), localCfg, database, imported, t.TempDir())
			checkSources()
			if tc.missing {
				require.ErrorContains(t, err, "missing Codex ancestor")
				_, err = os.Stat(destination)
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			require.Equal(t, imported.ID, result.RequestedID)
			require.Equal(t, filepath.Base(childFile), result.InstalledPage)
			if tc.standalone {
				require.Len(t, result.Paths, 1)
				got, err := os.ReadFile(result.Paths[0])
				require.NoError(t, err)
				require.Equal(t, childData, got)
				return
			}
			require.Len(t, result.Paths, 2)
			for i, want := range [][]byte{parentData, childData} {
				got, err := os.ReadFile(result.Paths[i])
				require.NoError(t, err)
				require.Equal(t, want, got)
			}
		})
	}
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
