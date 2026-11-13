package sessionmaterialize

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestMaterializeSourceErrorGuidance(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	root := t.TempDir()
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	session := &db.Session{ID: id, Agent: "claude", Machine: "local"}
	cwd := t.TempDir()
	_, err := Materialize(t.Context(), cfg, session, cwd)
	require.ErrorContains(t, err, "native resume requires the original transcript")

	name := filepath.Join(root, "project", id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
	file, err := os.Create(name)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(maxSourceBytes+1))
	require.NoError(t, file.Close())
	session.FilePath = &name
	_, err = Materialize(t.Context(), cfg, session, cwd)
	require.ErrorIs(t, err, errSourceSize)
	assert.Contains(t, err.Error(), "reading native transcript")
	assert.NotContains(t, err.Error(), "native resume requires the original transcript")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Materialize(ctx, cfg, session, cwd)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "native resume requires the original transcript")
}

func TestMaterializeMissingMirrorRequiresOriginalTranscript(t *testing.T) {
	for _, tc := range []struct{ agent, id, source string }{
		{"claude", "12345678-1234-1234-1234-123456789abc", "/project/12345678-1234-1234-1234-123456789abc.jsonl"},
		{"pi", "pi:example", "/project/example.jsonl"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			data := t.TempDir()
			require.NoError(t, os.MkdirAll(remotesync.MirrorDir(data, "remote"), 0o700))
			cfg := config.Config{DataDir: data, RemoteHosts: []config.RemoteHost{{Host: "remote"}}}
			source := "remote:" + tc.source
			session := &db.Session{ID: "remote~" + tc.id, Agent: tc.agent, Machine: "remote", FilePath: &source}
			_, err := Materialize(t.Context(), cfg, session, t.TempDir())
			require.ErrorContains(t, err, "native resume requires the original transcript")
		})
	}
}

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
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
			original := []byte(`{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"fixture"}}` + "\n" + `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{"worktreePath":"/old"}}` + "\n")
			require.NoError(t, os.WriteFile(name, original, 0o600))
			if !mirror {
				// A copy elsewhere must not make a valid archived path ambiguous.
				other := filepath.Join(root, "original", "other-project", id+".jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o700))
				require.NoError(t, os.WriteFile(other, original, 0o600))
			}
			sidecar := filepath.Join(strings.TrimSuffix(name, ".jsonl"), "tool-results", "result.txt")
			require.NoError(t, os.MkdirAll(filepath.Dir(sidecar), 0o700))
			require.NoError(t, os.WriteFile(sidecar, []byte("tool output"), 0o600))
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
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				_, err = Materialize(ctx, cfg, s, cwd)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(sidecar), "result2.txt"), []byte("more"), 0o600))
				_, err = readLocalSource(t.Context(), filepath.Join(root, "original"), name, id, int64(len(original)+len("tool output")+3))
				require.ErrorContains(t, err, "exceeds size limit")
				require.ErrorContains(t, err, "result2.txt")
			}
		})
	}
	for _, tc := range []struct {
		name      string
		hint      string
		duplicate bool
		wantError string
	}{
		{name: "moved root", hint: "stale"},
		{name: "missing path"},
		{name: "empty path", hint: "empty"},
		{name: "ambiguous fallback", hint: "stale", duplicate: true, wantError: "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "test-project")
			cfg := config.Config{
				InstallationID: "local", LocalMachineName: "local",
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
			}
			name := filepath.Join(root, "project", id+".jsonl")
			original := []byte(`{"type":"user","sessionId":"` + id + `","message":{"content":"fixture"}}` + "\n")
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
			require.NoError(t, os.WriteFile(name, original, 0o600))
			if tc.duplicate {
				other := filepath.Join(root, "other-project", id+".jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o700))
				require.NoError(t, os.WriteFile(other, original, 0o600))
			}
			s := &db.Session{ID: id, Agent: "claude", Machine: "local"}
			switch tc.hint {
			case "stale":
				stored := filepath.Join(t.TempDir(), "old-project", id+".jsonl")
				s.FilePath = &stored
			case "empty":
				s.FilePath = new(string)
			}
			result, err := Materialize(t.Context(), cfg, s, t.TempDir())
			dest := filepath.Join(home, "projects", "test-project", id+".jsonl")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				_, err = os.Stat(dest)
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			expectedInfo, err := os.Stat(name)
			require.NoError(t, err)
			actualInfo, err := os.Stat(result.Source)
			require.NoError(t, err)
			assert.True(t, os.SameFile(expectedInfo, actualInfo))
		})
	}
}

func TestConfiguredSourceFallbackExcludesNestedForeignMachine(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentPi} {
		for _, current := range []bool{true, false} {
			name := "foreign only"
			if current {
				name = "current and foreign copies"
			}
			t.Run(string(agent)+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				foreign := filepath.Join(root, "foreign")
				basename := id + ".jsonl"
				content := []byte(`{"type":"user","sessionId":"` + id + `","message":{"content":"fixture"}}` + "\n")
				sessionID := id
				foreignName := filepath.Join(foreign, basename)
				if agent == parser.AgentPi {
					basename = "timestamp_" + basename
					foreignName = filepath.Join(foreign, "project", basename)
					sessionID = "pi:" + id
					content = []byte(`{"type":"session","version":3,"id":"` + id + `","cwd":"/old"}` + "\n" + `{"type":"message","id":"m1","parentId":null,"message":{"role":"user","content":[{"type":"text","text":"fixture"}],"timestamp":0}}` + "\n")
				}
				currentName := filepath.Join(root, "current", basename)
				for _, path := range []string{foreignName, currentName} {
					if path == currentName && !current {
						continue
					}
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
					require.NoError(t, os.WriteFile(path, content, 0o600))
				}
				stale := filepath.Join(root, "old", basename)
				cfg := config.Config{
					InstallationID: "viewer",
					AgentDirs:      map[parser.AgentType][]string{agent: {root, foreign}},
					SourceMachines: map[parser.AgentType]map[string]string{agent: {root: "machine-a", foreign: "machine-b"}},
				}
				session := &db.Session{ID: sessionID, Agent: string(agent), Machine: "machine-a", FilePath: &stale}
				var selected string
				var err error
				if agent == parser.AgentPi {
					selected, _, err = findPiSource(t.Context(), cfg, session, stale, id)
				} else {
					_, selected, err = readConfiguredLocal(t.Context(), cfg, session, stale, id)
				}
				if !current {
					require.ErrorContains(t, err, "unavailable")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, currentName, selected)
			})
		}
	}
}

func TestMaterializeRecordedSourceAfterMachineRelabel(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentPi} {
		t.Run(string(agent), func(t *testing.T) {
			root, destination := t.TempDir(), t.TempDir()
			if agent == parser.AgentClaude {
				t.Setenv("CLAUDE_CONFIG_DIR", destination)
				t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
			} else {
				t.Setenv("PI_CODING_AGENT_DIR", "")
				t.Setenv("PI_CODING_AGENT_SESSION_DIR", destination)
			}
			name := filepath.Join(root, "project", id+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
			content := `{"type":"user","sessionId":"` + id + `","message":{"content":"fixture"}}` + "\n"
			sessionID := id
			if agent == parser.AgentPi {
				sessionID = "pi:" + id
				content = `{"type":"session","id":"` + id + `","cwd":"/old"}` + "\n"
			}
			require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
			cfg := config.Config{InstallationID: "local", AgentDirs: map[parser.AgentType][]string{agent: {root}}, SourceMachines: map[parser.AgentType]map[string]string{agent: {root: "renamed"}}}
			session := &db.Session{ID: sessionID, Agent: string(agent), Machine: "old-name", FilePath: &name}
			result, err := Materialize(t.Context(), cfg, session, t.TempDir())
			require.NoError(t, err)
			assert.Equal(t, name, result.Source)
			// A stale path must not search a root assigned to another machine.
			stale := filepath.Join(root, "project", "missing.jsonl")
			session.FilePath = &stale
			_, err = Materialize(t.Context(), cfg, session, t.TempDir())
			require.ErrorContains(t, err, "unavailable")
		})
	}
}

func TestS3ConfiguredSource(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	const root = "s3://test/host/raw/claude"
	const contents = "{\"type\":\"user\",\"sessionId\":\"" + id + "\",\"message\":{\"content\":\"fixture\"}}\n"
	const base = "host/raw/claude/project/" + id
	var extraKey atomic.Value
	extraKey.Store("")
	var companionGets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			assert.True(t, strings.HasSuffix(r.URL.Query().Get("prefix"), "/"))
			w.Header().Set("Content-Type", "application/xml")
			if strings.Contains(r.URL.Query().Get("prefix"), "tool-results") {
				if key := extraKey.Load().(string); key != "" {
					_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>`+key+`</Key><Size>4</Size></Contents></ListBucketResult>`)
					return
				}
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>`+base+`/tool-results/</Key><Size>0</Size></Contents><Contents><Key>`+base+`/tool-results/result.txt</Key><Size>4</Size></Contents><Contents><Key>`+base+`/tool-results/result2.txt</Key><Size>4</Size></Contents></ListBucketResult>`)
			} else {
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>`+base+`/subagents/</Key><Size>0</Size></Contents></ListBucketResult>`)
			}
			return
		}
		if r.URL.Path == "/test/"+base+"/tool-results/result.txt" || r.URL.Path == "/test/"+base+"/tool-results/result2.txt" || r.URL.Path == "/test/"+extraKey.Load().(string) {
			companionGets.Add(1)
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
	_, err := readS3Source(t.Context(), config.Config{}, "remote", "s3://other/project/"+id+".jsonl", id, maxSourceBytes)
	require.ErrorContains(t, err, "configured roots")
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	files, err := readS3Source(t.Context(), cfg, "host", root+"/project/"+id+".jsonl", id, maxSourceBytes)
	require.NoError(t, err)
	require.Len(t, files, 3)
	assert.Equal(t, contents, string(files[0].data))
	assert.Equal(t, "tool-results/result.txt", files[1].rel)
	assert.Equal(t, "tool", string(files[1].data))
	assert.Equal(t, "tool-results/result2.txt", files[2].rel)
	assert.Equal(t, "tool", string(files[2].data))
	companionGets.Store(0)
	_, err = readS3Source(t.Context(), cfg, "host", root+"/project/"+id+".jsonl", id, int64(len(contents)+len("tool")+3))
	require.ErrorContains(t, err, "exceeds size limit")
	assert.EqualValues(t, 1, companionGets.Load())
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project")
	uri := root + "/project/" + id + ".jsonl"
	s := &db.Session{ID: "host~" + id, Agent: "claude", Machine: "host", FilePath: &uri}
	cwd := t.TempDir()
	_, err = Materialize(t.Context(), cfg, s, cwd)
	require.NoError(t, err)
	installed, err := os.ReadFile(filepath.Join(home, "projects", "project", id+".jsonl"))
	require.NoError(t, err)
	assert.Equal(t, contents, string(installed))
	extraKey.Store(base + "/tool-results/a/./b")
	_, err = readS3Source(t.Context(), cfg, "host", uri, id, maxSourceBytes)
	require.ErrorContains(t, err, "unsafe S3 file path")
	if runtime.GOOS == "windows" {
		for _, invalid := range []string{"CON", "file:stream"} {
			extraKey.Store(base + "/tool-results/" + invalid)
			_, err = readS3Source(t.Context(), cfg, "host", uri, id, maxSourceBytes)
			require.ErrorContains(t, err, "unsafe S3 file path")
		}
	} else {
		extraKey.Store(base + "/tool-results/file:stream")
		files, err = readS3Source(t.Context(), cfg, "host", uri, id, maxSourceBytes)
		require.NoError(t, err)
		require.Len(t, files, 2)
		assert.Equal(t, "tool-results/file:stream", files[1].rel)
	}
}

func TestMaterializeParentSource(t *testing.T) {
	const selected = "12345678-1234-1234-1234-123456789abc"
	const parent = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	root := t.TempDir()
	home := t.TempDir()
	name := filepath.Join(root, "project", selected+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
	original := []byte(`{"type":"user","sessionId":"` + parent + `","message":{"content":"parent"}}` + "\n" + `{"type":"worktree-state","sessionId":"` + parent + `","worktreeSession":{"worktreePath":"/old"}}` + "\n")
	require.NoError(t, os.WriteFile(name, original, 0o600))
	cfg := config.Config{InstallationID: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	s := &db.Session{ID: selected, Agent: "claude", Machine: "local", FilePath: &name, SourceSessionID: parent}
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project")
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	_, err := Materialize(t.Context(), cfg, s, t.TempDir())
	require.NoError(t, err)
	installed, err := os.ReadFile(filepath.Join(home, "projects", "project", selected+".jsonl"))
	require.NoError(t, err)
	assert.Equal(t, original, installed)
	for _, invalid := range []string{"other~" + parent, "local~bad", "~" + parent} {
		s.SourceSessionID = invalid
		_, err = Materialize(t.Context(), cfg, s, t.TempDir())
		require.ErrorContains(t, err, "invalid source session identity")
	}
}

func TestMaterializeLinkedClaudeSources(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	for _, layout := range []string{"external saved", "external moved", "in-root session directory"} {
		t.Run(layout, func(t *testing.T) {
			root, home, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
			project := filepath.Join(root, "project")
			var companionRoot string
			if layout == "in-root session directory" {
				require.NoError(t, os.MkdirAll(project, 0o700))
				companionRoot = filepath.Join(root, "other-project", "session")
				require.NoError(t, os.MkdirAll(companionRoot, 0o700))
				if err := os.Symlink(companionRoot, filepath.Join(project, id)); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				target := t.TempDir()
				if err := os.Symlink(target, project); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				companionRoot = filepath.Join(target, id)
			}
			original := `{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"linked session"}}` + "\n"
			name := filepath.Join(project, id+".jsonl")
			require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
			for relative, content := range map[string]string{"tool-results/result.txt": "tool output", "subagents/agent.jsonl": "child transcript"} {
				file := filepath.Join(companionRoot, filepath.FromSlash(relative))
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
				require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
			}
			stored := name
			if layout == "external moved" {
				stored = filepath.Join(root, "old-project", id+".jsonl")
			}
			session := &db.Session{ID: id, Agent: "claude", Machine: "local", FilePath: &stored}
			cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
			result, err := Materialize(t.Context(), cfg, session, cwd)
			require.NoError(t, err)
			assert.Equal(t, "installed", result.Status)
			got, err := os.ReadFile(filepath.Join(home, "projects", "fixture", id+".jsonl"))
			require.NoError(t, err)
			assert.Equal(t, original, string(got))
			for relative, content := range map[string]string{"tool-results/result.txt": "tool output", "subagents/agent.jsonl": "child transcript"} {
				got, err := os.ReadFile(filepath.Join(home, "projects", "fixture", id, filepath.FromSlash(relative)))
				require.NoError(t, err)
				assert.Equal(t, content, string(got))
			}
			if layout == "external saved" || layout == "in-root session directory" {
				cfg.SourceMachines = map[parser.AgentType]map[string]string{parser.AgentClaude: {root: "renamed"}}
				result, err = Materialize(t.Context(), cfg, session, cwd)
				require.NoError(t, err)
				assert.Equal(t, name, result.Source)
				assert.Equal(t, "unchanged", result.Status)
			}
		})
	}
}

func TestReadFileBudgetAndInvalidCompanionPath(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		name := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(name, []byte("data"), 0o600))
		got, _, err := readFile(t.Context(), name, 4)
		require.NoError(t, err)
		assert.Equal(t, "data", string(got))
		_, _, err = readFile(t.Context(), name, 3)
		require.ErrorIs(t, err, errSourceSize)
		_, _, err = readFile(t.Context(), name, 0)
		require.ErrorIs(t, err, errSourceSize)
	})
	t.Run("invalid companion", func(t *testing.T) {
		const id = "12345678-1234-1234-1234-123456789abc"
		root := t.TempDir()
		primary := filepath.Join(root, id+".jsonl")
		require.NoError(t, os.WriteFile(primary, []byte("x"), 0o600))
		companion := filepath.Join(root, id, "tool-results")
		require.NoError(t, os.MkdirAll(filepath.Dir(companion), 0o700))
		if err := os.Symlink(t.TempDir(), companion); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		_, err := readLocalSource(t.Context(), root, primary, id, 32)
		require.ErrorContains(t, err, "invalid session file")
		require.ErrorContains(t, err, companion)
	})
}
