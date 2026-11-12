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

func TestPiFallbackReadsOnlyHeadersAndRejectsUncertainCandidates(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	stale := filepath.Join(root, "missing.jsonl")
	s := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &stale}
	good := filepath.Join(root, "one_target.jsonl")
	require.NoError(t, os.WriteFile(good, []byte(`{"type":"session","id":"target","cwd":"/old"}`+"\n"), 0o600))
	large := filepath.Join(root, "unrelated.jsonl")
	require.NoError(t, os.WriteFile(large, []byte(`{"type":"session","id":"other","cwd":"/old"}`+"\n"), 0o600))
	f, err := os.OpenFile(large, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxSourceBytes+1))
	require.NoError(t, f.Close())
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = readPiFile(cancelled, root, large)
	require.ErrorIs(t, err, context.Canceled)
	result, err := Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, good, result.Source)
	require.NoError(t, os.WriteFile(large, []byte(`{"type":"session","id":"target","version":4,"cwd":"/old"}`+"\n"), 0o600))
	_, err = Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.ErrorContains(t, err, "version")
	require.NoError(t, os.Remove(good))
	require.NoError(t, os.WriteFile(large, []byte(`{"type":"session","id":"target","cwd":"/old"}`+"\n"), 0o600))
	f, err = os.OpenFile(large, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxSourceBytes+1))
	require.NoError(t, f.Close())
	_, err = Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.ErrorContains(t, err, "exceeds size limit")
	require.NoError(t, os.WriteFile(large, []byte(strings.Repeat(" ", (1<<20)+1)+`{"type":"session","id":"target","cwd":"/old"}`+"\n"), 0o600))
	_, err = Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.ErrorContains(t, err, "header exceeds")
}

func TestPiSourceFallbackAndIdentity(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	stale := filepath.Join(root, "stale.jsonl")
	s := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &stale}
	data := []byte(`{"type":"session","id":"target","cwd":"/old"}` + "\n")
	a := filepath.Join(root, "a", "timestamp_target.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(a), 0o700))
	require.NoError(t, os.WriteFile(a, data, 0o600))
	result, err := Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, a, result.Source)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Materialize(cancelled, cfg, nil, s, t.TempDir())
	require.ErrorIs(t, err, context.Canceled)
	invalid := filepath.Join(root, "invalid.jsonl")
	require.NoError(t, os.WriteFile(invalid, []byte(`{"type":"session","id":"other","cwd":"/old"}`+"\n"), 0o600))
	s.FilePath = &invalid
	_, err = Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.ErrorContains(t, err, "identity differs")
	s.FilePath = &stale
	b := filepath.Join(root, "b", "timestamp_target.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(b), 0o700))
	require.NoError(t, os.WriteFile(b, data, 0o600))
	_, err = Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.ErrorContains(t, err, "ambiguous")
	stored := "archive://root/source"
	s.FilePath = &stored
	_, err = Materialize(t.Context(), cfg, nil, s, t.TempDir())
	require.ErrorContains(t, err, "Pi keeps no")
}

func TestPiHeaderIdentityAndVersion(t *testing.T) {
	for _, tc := range []struct{ header, id, want string }{
		{`{"type":"session","id":"other","cwd":"/old"}`, "id", "identity"},
		{`{"type":"session","id":"id","cwd":"/a","cwd":"/b"}`, "id", "one cwd"},
		{`{"type":"session","id":"id"}`, "id", "one cwd"},
		{`{"type":"message","id":"id","cwd":"/old"}`, "id", "header"},
		{`{"type":"session","cwd":"/old"}`, "id", "no id"},
		{`{"type":"session","id":"id","version":4,"cwd":"/old"}`, "id", "version"},
		{`{"type":"title"}` + "\n" + `{"type":"session","id":"id","cwd":"/old"}`, "id", "header"},
	} {
		t.Run(tc.want+tc.header, func(t *testing.T) {
			_, err := relocatePiHeader(t.Context(), []byte(tc.header), tc.id, "/new")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestPiDefaultDestination(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", home)
	cwd := filepath.Join(string(filepath.Separator), "work", "project")
	dest, err := piDestination(cwd, "file.jsonl")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "sessions", "--work-project--", "file.jsonl"), dest)
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
		original := "\nnot-json\n" + `{"type":"session","id":"` + id + `","cwd":"/old"}` + "\n"
		require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
		s := &db.Session{ID: "pi:" + id, Agent: "pi", Machine: "local", FilePath: &name}
		result, err := Materialize(t.Context(), cfg, nil, s, cwd)
		require.NoError(t, err)
		require.Equal(t, []string{filepath.Join(dest, id+".jsonl")}, result.Paths)
		got, err := os.ReadFile(result.Paths[0])
		require.NoError(t, err)
		assert.Equal(t, "\nnot-json\n"+`{"type":"session","id":"`+id+`","cwd":`+string(mustJSON(t, cwd))+"}\n", string(got))
	}
}

func TestPiRelativeDestinationUsesCheckout(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "relative-sessions")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	got, err := piDestination(cwd, "id.jsonl")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "relative-sessions", "id.jsonl"), got)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "relative-agent")
	got, err = piDestination(cwd, "id.jsonl")
	require.NoError(t, err)
	encoded := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(strings.TrimLeft(cwd, `/\`))
	assert.Equal(t, filepath.Join(cwd, "relative-agent", "sessions", "--"+encoded+"--", "id.jsonl"), got)
	urlDir := t.TempDir()
	urlPath := filepath.ToSlash(urlDir)
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", (&url.URL{Scheme: "file", Path: urlPath}).String())
	got, err = piDestination(cwd, "id.jsonl")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(urlDir, "id.jsonl"), got)
}

func TestPiResolvesCheckoutSymlinkAndRejectsUnsupportedHeaders(t *testing.T) {
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
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	name := filepath.Join(root, "session.jsonl")
	s := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &name}
	for _, tc := range []struct{ header, message string }{
		{`{"type":"session","cwd":"/old"}`, "no id"},
		{`{"type":"session","id":"target","version":4,"cwd":"/old"}`, "version"},
	} {
		require.NoError(t, os.WriteFile(name, []byte(tc.header+"\n"), 0o600))
		_, err := Materialize(t.Context(), cfg, nil, s, link)
		require.ErrorContains(t, err, tc.message)
		_, err = os.Stat(filepath.Join(dest, "target.jsonl"))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
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
