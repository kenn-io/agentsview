package sessionmaterialize

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
				var err error
				name, err = remotesync.SafeRemappedRemotePath(remotesync.MirrorDir(root, "remote"), stored)
				require.NoError(t, err)
			} else {
				cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {filepath.Join(root, "original")}}
				cfg.SessionSources = []config.SessionSource{{Agent: parser.AgentClaude, Dir: filepath.Join(root, "original"), Machine: "local"}}
				cfg.SourceMachines = map[parser.AgentType]map[string]string{parser.AgentClaude: {filepath.Join(root, "original"): "local"}}
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0700))
			original := []byte(`{"type":"user","message":"fixture"}` + "\n")
			require.NoError(t, os.WriteFile(name, original, 0600))
			sidecar := filepath.Join(strings.TrimSuffix(name, ".jsonl"), "tool-results", "result.txt")
			require.NoError(t, os.MkdirAll(filepath.Dir(sidecar), 0700))
			require.NoError(t, os.WriteFile(sidecar, []byte("tool output"), 0600))
			result, err := Materialize(t.Context(), cfg, s, cwd)
			require.NoError(t, err)
			assert.Equal(t, "installed", result.Status)
			got, err := os.ReadFile(filepath.Join(home, "projects", "test-project", id+".jsonl"))
			require.NoError(t, err)
			assert.Equal(t, original, got)
			got, err = os.ReadFile(filepath.Join(home, "projects", "test-project", id, "tool-results", "result.txt"))
			require.NoError(t, err)
			assert.Equal(t, "tool output", string(got))
			result, err = Materialize(t.Context(), cfg, s, cwd)
			require.NoError(t, err)
			assert.Equal(t, "unchanged", result.Status)
		})
	}
}

func TestS3RejectsUnconfiguredRoot(t *testing.T) {
	_, err := readS3Source(t.Context(), config.Config{}, "remote", "s3://other/project/12345678-1234-1234-1234-123456789abc.jsonl", "12345678-1234-1234-1234-123456789abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configured roots")
}

func TestS3ConfiguredSource(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	const root = "s3://test/host/raw/claude"
	const contents = "{\"type\":\"user\",\"message\":\"fixture\"}\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
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
	files, err := readS3Source(t.Context(), cfg, "host", root+"/project/"+id+".jsonl", id)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, contents, string(files[0].data))
}
