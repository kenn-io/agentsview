package sessionmaterialize

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestMaterializeCodexConfiguredAndMirror(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const page = "22222222-2222-4222-8222-222222222222"
	for _, mirror := range []bool{false, true} {
		t.Run(fmt.Sprintf("mirror=%t", mirror), func(t *testing.T) {
			home, root, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("CODEX_HOME", home)
			cfg := config.Config{DataDir: root, InstallationID: "local", LocalMachineName: "local"}
			location := filepath.Join(root, "sessions")
			oldName := "rollout-2026-01-01T01-00-00-" + id + ".jsonl"
			newName := "rollout-2026-01-02T01-00-00-" + id + "_" + page + ".jsonl"
			stored := filepath.Join(location, "2026", "01", "01", oldName)
			session := &db.Session{ID: "codex:" + id, Agent: "codex", Machine: "local", FilePath: &stored}
			if mirror {
				cfg.RemoteHosts = []config.RemoteHost{{Host: "remote"}}
				session.ID, session.Machine = "remote~codex:"+id, "remote"
				stored = "remote:/source/sessions/2026/01/01/" + oldName
				location = filepath.Join(remotesync.MirrorDir(root, "remote"), "source", "sessions")
			} else {
				cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {location}}
			}
			oldPath := filepath.Join(location, "2026", "01", "01", oldName)
			newPath := filepath.Join(location, "2026", "01", "02", newName)
			require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o700))
			require.NoError(t, os.MkdirAll(filepath.Dir(newPath), 0o700))
			old := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", id))
			newPage := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":1}}}`+"\n", id, id))
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
			result, err = Materialize(t.Context(), cfg, nil, session, cwd)
			require.NoError(t, err)
			assert.Equal(t, "unchanged", result.Status)
			require.NoError(t, os.WriteFile(result.Paths[1], []byte("changed"), 0o600))
			_, err = Materialize(t.Context(), cfg, nil, session, cwd)
			require.ErrorContains(t, err, "differs")
			got, err := os.ReadFile(newPath)
			require.NoError(t, err)
			assert.Equal(t, newPage, got)
			if mirror {
				encoder, err := zstd.NewWriter(nil)
				require.NoError(t, err)
				compressed := encoder.EncodeAll(old, nil)
				encoder.Close()
				require.NoError(t, os.Remove(oldPath))
				require.NoError(t, os.WriteFile(oldPath+".zst", compressed, 0o600))
				t.Setenv("CODEX_HOME", t.TempDir())
				result, err = Materialize(t.Context(), cfg, nil, session, cwd)
				require.NoError(t, err)
				got, err = os.ReadFile(result.Paths[0])
				require.NoError(t, err)
				assert.Equal(t, compressed, got)
			}
		})
	}
}

func TestMaterializeCodexS3(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const root = "s3://test/host/raw/codex"
	const key = "host/raw/codex/sessions/2026/01/01/rollout-2026-01-01T01-00-00-" + id + ".jsonl"
	const archivedKey = "host/raw/codex/archived_sessions/rollout-2026-01-01T01-00-00-" + id + ".jsonl"
	content := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`+"\n", id)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size></Contents><Contents><Key>%s</Key><Size>%d</Size></Contents></ListBucketResult>`, key, len(content), archivedKey, len(content))
			return
		}
		if r.URL.Path != "/test/"+key {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()
	t.Setenv("AWS_S3_ENDPOINT", server.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AWS_SESSION_TOKEN", "fixture")
	t.Setenv("CODEX_HOME", t.TempDir())
	uri := "s3://test/" + key
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}}
	session := &db.Session{ID: "codex:" + id, Agent: "codex", Machine: "host", FilePath: &uri}
	bad := *session
	bad.Machine = "other"
	_, err := Materialize(t.Context(), cfg, nil, &bad, t.TempDir())
	require.ErrorContains(t, err, "source identity differs")
	bad = *session
	bad.ID = "remote~codex:" + id
	_, err = Materialize(t.Context(), cfg, nil, &bad, t.TempDir())
	require.ErrorContains(t, err, "source host differs")
	result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.NoError(t, err)
	require.Len(t, result.Paths, 1)
	got, err := os.ReadFile(result.Paths[0])
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
}

func TestMaterializeCodexMissingAndCanceled(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	root := t.TempDir()
	missing := filepath.Join(root, "2026", "01", "01", "rollout-2026-01-01T01-00-00-"+id+".jsonl")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}}
	session := &db.Session{ID: "codex:" + id, Agent: "codex", Machine: "local", FilePath: &missing}
	_, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Materialize(ctx, cfg, nil, session, t.TempDir())
	require.ErrorIs(t, err, context.Canceled)

	// The requested page can refer to a parent in a configured sibling archive.
	const parent = "22222222-2222-4222-8222-222222222222"
	base, home := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	live, archived := filepath.Join(base, "sessions"), filepath.Join(base, "archived_sessions")
	page := filepath.Join(live, "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	ancestor := filepath.Join(archived, "rollout-20240115-"+parent+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(page), 0o700))
	require.NoError(t, os.MkdirAll(archived, 0o700))
	require.NoError(t, os.WriteFile(page, []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":1}}}`+"\n", id, parent)), 0o600))
	require.NoError(t, os.WriteFile(ancestor, []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", parent)), 0o600))
	cfg.AgentDirs[parser.AgentCodex] = []string{live, archived}
	session.FilePath = &page
	result, err := Materialize(t.Context(), cfg, nil, session, t.TempDir())
	require.NoError(t, err)
	require.Len(t, result.Paths, 2)
	assert.Equal(t, filepath.Base(ancestor), filepath.Base(result.Paths[0]))
}
