package sessionmaterialize

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestMaterializeConfiguredSources(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	for _, mirror := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "mirror"}[mirror], func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Config{DataDir: root, InstallationID: "local", LocalMachineName: "local"}
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "test-project")
			cwd := t.TempDir()
			name := filepath.Join(root, "original", "project", id+".jsonl")
			stored := name
			s := &db.Session{ID: id, Agent: "claude", Machine: "local", FilePath: &stored}
			if mirror {
				cfg.RemoteHosts = []config.RemoteHost{{Host: "remote"}}
				s.ID, s.Machine = "remote~"+id, "remote"
				stored = `C:\Users\fixture\projects\original\` + id + `.jsonl`
				var err error
				name, err = remotesync.SafeRemappedRemotePath(remotesync.MirrorDir(root, "remote"), stored)
				require.NoError(t, err)
				stored = "remote:" + stored
			} else {
				cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {filepath.Join(root, "original")}}
				cfg.SessionSources = []config.SessionSource{{Agent: parser.AgentClaude, Dir: filepath.Join(root, "original"), Machine: "local"}}
				cfg.SourceMachines = map[parser.AgentType]map[string]string{parser.AgentClaude: {filepath.Join(root, "original"): "local"}}
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0700))
			original := []byte(`{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"fixture"}}` + "\n" + `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{"worktreePath":"/old"}}` + "\n")
			require.NoError(t, os.WriteFile(name, original, 0600))
			sidecar := filepath.Join(strings.TrimSuffix(name, ".jsonl"), "tool-results", "result.txt")
			require.NoError(t, os.MkdirAll(filepath.Dir(sidecar), 0700))
			require.NoError(t, os.WriteFile(sidecar, []byte("tool output"), 0600))
			result, err := Materialize(t.Context(), cfg, s, cwd)
			require.NoError(t, err)
			assert.Equal(t, "installed", result.Status)
			got, err := os.ReadFile(filepath.Join(home, "projects", "test-project", id+".jsonl"))
			require.NoError(t, err)
			assert.Equal(t, string(original)+`{"type":"worktree-state","worktreeSession":null,"sessionId":"`+id+`"}`+"\n", string(got))
			got, err = os.ReadFile(filepath.Join(home, "projects", "test-project", id, "tool-results", "result.txt"))
			require.NoError(t, err)
			assert.Equal(t, "tool output", string(got))
			result, err = Materialize(t.Context(), cfg, s, cwd)
			require.NoError(t, err)
			assert.Equal(t, "unchanged", result.Status)
			if mirror {
				remote := strings.TrimPrefix(stored, "remote:")
				stored = "other:" + remote
				_, err = Materialize(t.Context(), cfg, s, cwd)
				require.ErrorContains(t, err, "host does not match")
				stored = "remote:" + remote
				lock, err := remotesync.AcquireMirrorLock(t.Context(), remotesync.MirrorDir(root, "remote"))
				require.NoError(t, err)
				defer lock.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				_, err = Materialize(ctx, cfg, s, cwd)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(sidecar), "result2.txt"), []byte("more"), 0600))
				_, err = readLocalSource(t.Context(), filepath.Join(root, "original"), name, id, int64(len(original)+len("tool output")+3))
				require.ErrorContains(t, err, "exceeds size limit")
				require.ErrorContains(t, err, "result2.txt")
			}
		})
	}
}

func TestS3ConfiguredSource(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	const root = "s3://test/host/raw/claude"
	const contents = "{\"type\":\"user\",\"sessionId\":\"" + id + "\",\"message\":{\"content\":\"fixture\"}}\n"
	const base = "host/raw/claude/project/" + id
	_, err := readS3Source(t.Context(), config.Config{}, "remote", "s3://other/project/"+id+".jsonl", id, maxSourceBytes)
	require.ErrorContains(t, err, "configured roots")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			assert.True(t, strings.HasSuffix(r.URL.Query().Get("prefix"), "/"))
			w.Header().Set("Content-Type", "application/xml")
			if strings.Contains(r.URL.Query().Get("prefix"), "tool-results") {
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>`+base+`/tool-results/</Key><Size>0</Size></Contents><Contents><Key>`+base+`/tool-results/result.txt</Key><Size>4</Size></Contents><Contents><Key>`+base+`/tool-results/result2.txt</Key><Size>4</Size></Contents></ListBucketResult>`)
			} else {
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>`+base+`/subagents/</Key><Size>0</Size></Contents></ListBucketResult>`)
			}
			return
		}
		if r.URL.Path == "/test/"+base+"/tool-results/result.txt" || r.URL.Path == "/test/"+base+"/tool-results/result2.txt" {
			_, _ = io.WriteString(w, "tool")
			return
		}
		if r.URL.Path != "/test/host/raw/claude/project/"+id+".jsonl" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, contents)
	}))
	defer server.Close()
	t.Setenv("AWS_S3_ENDPOINT", server.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AWS_SESSION_TOKEN", "fixture")
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	files, err := readS3Source(t.Context(), cfg, "host", root+"/project/"+id+".jsonl", id, maxSourceBytes)
	require.NoError(t, err)
	require.Len(t, files, 3)
	assert.Equal(t, contents, string(files[0].data))
	assert.Equal(t, "tool-results/result.txt", files[1].rel)
	assert.Equal(t, "tool", string(files[1].data))
	assert.Equal(t, "tool-results/result2.txt", files[2].rel)
	assert.Equal(t, "tool", string(files[2].data))
	_, err = readS3Source(t.Context(), cfg, "host", root+"/project/"+id+".jsonl", id, int64(len(contents)+len("tool")+3))
	require.ErrorContains(t, err, "exceeds size limit")
}

func TestMaterializeParentSource(t *testing.T) {
	const selected = "12345678-1234-1234-1234-123456789abc"
	const parent = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	root := t.TempDir()
	home := t.TempDir()
	name := filepath.Join(root, "project", selected+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0700))
	original := []byte(`{"type":"user","sessionId":"` + parent + `","message":{"content":"parent"}}` + "\n" + `{"type":"worktree-state","sessionId":"` + parent + `","worktreeSession":{}}` + "\n")
	require.NoError(t, os.WriteFile(name, original, 0600))
	cfg := config.Config{InstallationID: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	s := &db.Session{ID: selected, Agent: "claude", Machine: "local", FilePath: &name, SourceSessionID: parent}
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project")
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	_, err := Materialize(t.Context(), cfg, s, t.TempDir())
	require.NoError(t, err)
	installed, err := os.ReadFile(filepath.Join(home, "projects", "project", selected+".jsonl"))
	require.NoError(t, err)
	assert.Equal(t, original, installed)
}
