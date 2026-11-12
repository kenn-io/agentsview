package sessionmaterialize

import (
	"context"
	"encoding/json"
	"net/url"
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

func TestPiDestination(t *testing.T) {
	t.Run("settings precedence", func(t *testing.T) {
		cwd, agent := t.TempDir(), t.TempDir()
		t.Setenv("PI_CODING_AGENT_DIR", agent)
		t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
		require.NoError(t, os.WriteFile(filepath.Join(agent, "settings.json"), []byte("\xef\xbb\xbf"+`{"sessionDir":"global-sessions"}`), 0o600))
		got, err := piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "global-sessions", "id.jsonl"), got)
		require.NoError(t, os.Mkdir(filepath.Join(cwd, ".pi"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(cwd, ".pi", "settings.json"), []byte(`{"sessionDir":"project-sessions"}`), 0o600))
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "project-sessions", "id.jsonl"), got)
		require.NoError(t, os.WriteFile(filepath.Join(cwd, ".pi", "settings.json"), []byte(`{"sessionDir":null}`), 0o600))
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Contains(t, got, filepath.Join(agent, "sessions"))
		t.Setenv("PI_CODING_AGENT_SESSION_DIR", "environment-sessions")
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "environment-sessions", "id.jsonl"), got)
	})
	t.Run("relative destination", func(t *testing.T) {
		cwd := t.TempDir()
		t.Setenv("PI_CODING_AGENT_SESSION_DIR", "relative-sessions")
		t.Setenv("PI_CODING_AGENT_DIR", "")
		got, err := piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "relative-sessions", "id.jsonl"), got)
		t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
		t.Setenv("PI_CODING_AGENT_DIR", "relative-agent")
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Contains(t, got, filepath.Join(cwd, "relative-agent", "sessions"))
		if filepath.Separator == '/' {
			got, err = piDestination(t.Context(), "/work dir", "id.jsonl")
			require.NoError(t, err)
			assert.Equal(t, "/work dir/relative-agent/sessions/--work dir--/id.jsonl", got)
		}
		urlDir := t.TempDir()
		urlPath := filepath.ToSlash(urlDir)
		if !strings.HasPrefix(urlPath, "/") {
			urlPath = "/" + urlPath
		}
		t.Setenv("PI_CODING_AGENT_SESSION_DIR", (&url.URL{Scheme: "file", Path: urlPath}).String())
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(urlDir, "id.jsonl"), got)
	})
}

func TestPiSourceLookup(t *testing.T) {
	root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	stale := filepath.Join(root, "missing.jsonl")
	s := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &stale}
	good := filepath.Join(root, "one_target.jsonl")
	data := []byte(`{"type":"session","id":"target","cwd":"/old"}` + "\n")
	require.NoError(t, os.WriteFile(good, data, 0o600))
	other := filepath.Join(root, "unrelated.jsonl")
	require.NoError(t, os.WriteFile(other, []byte(`{"type":"session","id":"other","cwd":"/old"}`+"\n"), 0o600))
	f, err := os.OpenFile(other, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxSourceBytes+1))
	require.NoError(t, f.Close())
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Materialize(cancelled, cfg, nil, s, cwd)
	require.ErrorIs(t, err, context.Canceled)
	result, err := Materialize(t.Context(), cfg, nil, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, good, result.Source)
	// An invalid stored identity is returned rather than falling back.
	require.NoError(t, os.WriteFile(other, []byte(`{"type":"session","id":"other","cwd":"/old"}`+"\n"), 0o600))
	s.FilePath = &other
	_, err = Materialize(t.Context(), cfg, nil, s, cwd)
	require.ErrorIs(t, err, errPiIdentity)
	s.FilePath = &stale
	// An unsupported matching candidate aborts discovery.
	require.NoError(t, os.WriteFile(other, []byte(`{"type":"session","id":"target","version":4,"cwd":"/old"}`+"\n"), 0o600))
	_, err = Materialize(t.Context(), cfg, nil, s, cwd)
	require.ErrorContains(t, err, "version")
	require.NoError(t, os.WriteFile(other, data, 0o600))
	_, err = Materialize(t.Context(), cfg, nil, s, cwd)
	require.ErrorContains(t, err, "ambiguous")
	require.NoError(t, os.Remove(good))
	f, err = os.OpenFile(other, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxSourceBytes+1))
	require.NoError(t, f.Close())
	_, err = Materialize(t.Context(), cfg, nil, s, cwd)
	require.ErrorContains(t, err, "exceeds size limit")
	require.NoError(t, os.Remove(other))
	require.NoError(t, os.WriteFile(good, data, 0o600))
	uncertain := filepath.Join(root, "uncertain.jsonl")
	require.NoError(t, os.WriteFile(uncertain, []byte(strings.Repeat(" ", (1<<20)+1)), 0o600))
	_, err = Materialize(t.Context(), cfg, nil, s, cwd)
	require.ErrorContains(t, err, "header exceeds")
}

func TestMaterializePiConfiguredSources(t *testing.T) {
	for _, mirror := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "mirror"}[mirror], func(t *testing.T) {
			root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			cwd, err := filepath.EvalSymlinks(cwd)
			require.NoError(t, err)
			t.Setenv("PI_CODING_AGENT_DIR", "")
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
			cfg := config.Config{DataDir: root, InstallationID: "local", LocalMachineName: "local"}
			const native = "abc-session"
			const basename = "2026-01-01T00-00-00_abc-session.jsonl"
			name := filepath.Join(root, "original", "project", basename)
			stored := name
			s := &db.Session{ID: "pi:" + native, Agent: "pi", Machine: "local", FilePath: &stored}
			if mirror {
				cfg.RemoteHosts = []config.RemoteHost{{Host: "remote"}}
				s.ID, s.Machine = "remote~pi:"+native, "remote"
				remote := "/old/project/" + basename
				var err error
				name, err = remotesync.SafeRemappedRemotePath(remotesync.MirrorDir(root, "remote"), remote)
				require.NoError(t, err)
				stored = "remote:" + remote
			} else {
				cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentPi: {filepath.Join(root, "original")}}
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
			if mirror {
				require.NoError(t, os.Mkdir(name, 0o700))
				_, err = Materialize(t.Context(), cfg, nil, s, cwd)
				var pathErr *os.PathError
				require.ErrorAs(t, err, &pathErr)
				assert.Equal(t, name, pathErr.Path)
				_, err = os.Stat(filepath.Join(dest, basename))
				require.ErrorIs(t, err, os.ErrNotExist)
				require.NoError(t, os.Remove(name))
			}
			original := `{"type":"session", "id":"abc-session", "cwd" : "/old path", "parentSession":"/parent.jsonl"}` + "\n" + `{"type":"message","id":"branch","parentId":"first","message":{"role":"user","content":"hello"}}` + "\n"
			require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
			result, err := Materialize(t.Context(), cfg, nil, s, cwd)
			require.NoError(t, err)
			require.Equal(t, "installed", result.Status)
			require.Equal(t, []string{filepath.Join(dest, basename)}, result.Paths)
			got, err := os.ReadFile(result.Paths[0])
			require.NoError(t, err)
			// All records and header fields except the cwd value must stay byte-identical.
			assert.Equal(t, `{"type":"session", "id":"abc-session", "cwd" : `+string(mustJSON(t, cwd))+`, "parentSession":"/parent.jsonl"}`+"\n"+`{"type":"message","id":"branch","parentId":"first","message":{"role":"user","content":"hello"}}`+"\n", string(got))
			result, err = Materialize(t.Context(), cfg, nil, s, cwd)
			require.NoError(t, err)
			assert.Equal(t, "unchanged", result.Status)
		})
	}
}

func TestPiLinkedSources(t *testing.T) {
	root, external, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", t.TempDir())
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	original := filepath.Join(external, "target.jsonl")
	require.NoError(t, os.WriteFile(original, []byte(`{"type":"session","id":"target","cwd":"/old"}`+"\n"), 0o600))
	link := filepath.Join(root, "linked")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	linked := filepath.Join(link, "target.jsonl")
	s := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &linked}
	result, err := Materialize(t.Context(), cfg, nil, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, linked, result.Source)
	missing := filepath.Join(root, "missing.jsonl")
	s.FilePath = &missing
	result, err = Materialize(t.Context(), cfg, nil, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, linked, result.Source)
	// A saved path outside the provider roots is not itself authoritative.
	s.FilePath = &original
	result, err = Materialize(t.Context(), cfg, nil, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, linked, result.Source)
}

func TestPiHeaderIdentityAndVersion(t *testing.T) {
	_, err := relocatePiHeader(t.Context(), []byte(`{"type":"session","id":"id","cwd":"/old"}`), "id", string([]byte{0xff}))
	require.ErrorContains(t, err, "UTF-8")
	for _, tc := range []struct{ header, id, want string }{
		{`{"type":"session","id":"other","cwd":"/old"}`, "id", "identity"},
		{`{"type":"session","id":"id","cwd":"/a","cwd":"/b"}`, "id", "one cwd"},
		{`{"type":"session","id":"id"}`, "id", "one cwd"},
		{`{"type":"message","id":"id","cwd":"/old"}`, "id", "header"},
		{`{"type":"session","cwd":"/old"}`, "id", "identity"},
		{`{"type":"session","id":"id","version":4,"cwd":"/old"}`, "id", "version"},
		{`{"type":"title"}` + "\n" + `{"type":"session","id":"id","cwd":"/old"}`, "id", "header"},
		{`{"type":123,"id":"id"}`, "id", "header"},
		{`{"type":"session","id":123}`, "id", "header"},
		{`{"type":"session","id":"id","version":"3"}`, "id", "header"},
		{`{"Type":"session","id":"id"}`, "id", "header"},
		{`{"type":"session","ID":"id"}`, "id", "identity"},
		{"", "id", "header"},
		{"not-json", "id", "header"},
		{strings.Repeat(" ", (1<<20)+1), "id", "header exceeds"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			_, err := relocatePiHeader(t.Context(), []byte(tc.header), tc.id, "/new")
			switch tc.want {
			case "identity":
				require.ErrorIs(t, err, errPiIdentity)
			case "header":
				require.ErrorIs(t, err, errPiNotSession)
			default:
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestPiPreservesPrefixAndUsesIdentityForNonuniqueNames(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	for _, id := range []string{"child-one", "child-two"} {
		name := filepath.Join(root, id, "session.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
		original := "\nnot-json\nnull\nfalse\n0.0\n\"\"\n" + `{"type":"session","id":"` + id + `","cwd":"/old"}` + "\n"
		require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
		s := &db.Session{ID: "pi:" + id, Agent: "pi", Machine: "local", FilePath: &name}
		result, err := Materialize(t.Context(), cfg, nil, s, cwd)
		require.NoError(t, err)
		require.Equal(t, []string{filepath.Join(dest, id+".jsonl")}, result.Paths)
		got, err := os.ReadFile(result.Paths[0])
		require.NoError(t, err)
		assert.Equal(t, "\nnot-json\nnull\nfalse\n0.0\n\"\"\n"+`{"type":"session","id":"`+id+`","cwd":`+string(mustJSON(t, cwd))+"}\n", string(got))
	}
}

func TestPiResolvesCheckoutSymlink(t *testing.T) {
	root, dest, parent := t.TempDir(), t.TempDir(), t.TempDir()
	realDir := filepath.Join(parent, "checkout")
	require.NoError(t, os.Mkdir(realDir, 0o700))
	link := filepath.Join(parent, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(realDir)
	require.NoError(t, err)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	name := filepath.Join(root, "session.jsonl")
	s := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &name}
	require.NoError(t, os.WriteFile(name, []byte(`{"type":"session","id":"target","cwd":"/old"}`+"\n"), 0o600))
	result, err := Materialize(t.Context(), cfg, nil, s, link)
	require.NoError(t, err)
	assert.Equal(t, resolved, result.Cwd)
	data, err := os.ReadFile(result.Paths[0])
	require.NoError(t, err)
	assert.Equal(t, `{"type":"session","id":"target","cwd":`+string(mustJSON(t, resolved))+"}\n", string(data))
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

// piResolveDir invokes this normalization only on Windows.
func TestPiWindowsShellPaths(t *testing.T) {
	for input, want := range map[string]string{
		"/c/work": `C:\work`, "/mnt/d/work/file": `D:\work\file`,
		"/cygdrive/e": `E:\`, "/Z": `Z:\`, "/MNT/f/root": `F:\root`,
		"//server/share": "//server/share", "/mnt/example": "/mnt/example",
		`/c/mixed\path`: `/c/mixed\path`,
	} {
		assert.Equal(t, want, piWindowsShellPath(input), input)
	}
}
