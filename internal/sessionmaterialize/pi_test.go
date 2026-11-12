package sessionmaterialize

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
			require.NoError(t, os.WriteFile(result.Paths[0], []byte("different"), 0o600))
			_, err = Materialize(t.Context(), cfg, nil, s, cwd)
			require.ErrorContains(t, err, "differs")
		})
	}
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

func TestPiHeaderIdentityAndDestination(t *testing.T) {
	for _, tc := range []struct{ header, id, basename, want string }{
		{`{"type":"session","id":"other","cwd":"/old"}`, "id", "id.jsonl", "identity"},
		{`{"type":"session","id":"id","cwd":"/a","cwd":"/b"}`, "id", "id.jsonl", "one cwd"},
		{`{"type":"session","id":"id"}`, "id", "id.jsonl", "one cwd"},
		{`{"type":"message","id":"id","cwd":"/old"}`, "id", "id.jsonl", "header"},
	} {
		t.Run(tc.want+tc.header, func(t *testing.T) {
			_, err := relocatePiHeader([]byte(tc.header), tc.id, tc.basename, "/new")
			require.ErrorContains(t, err, tc.want)
		})
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", home)
	cwd := filepath.Join(string(filepath.Separator), "work", "project")
	dest, err := piDestination(cwd, "file.jsonl")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "sessions", "--work-project--", "file.jsonl"), dest)
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
