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
		require.NoError(t, os.WriteFile(filepath.Join(cwd, ".pi", "settings.json"), []byte(`{"sessionDir":`), 0o600))
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "global-sessions", "id.jsonl"), got)
		require.NoError(t, os.WriteFile(filepath.Join(cwd, ".pi", "settings.json"), []byte(`{"sessionDir":null}`), 0o600))
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Contains(t, got, filepath.Join(agent, "sessions"))
		require.NoError(t, os.Remove(filepath.Join(agent, "settings.json")))
		require.NoError(t, os.Mkdir(filepath.Join(agent, "settings.json"), 0o700))
		_, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.ErrorContains(t, err, "read Pi settings")
		t.Setenv("PI_CODING_AGENT_SESSION_DIR", "environment-sessions")
		got, err = piDestination(t.Context(), cwd, "id.jsonl")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "environment-sessions", "id.jsonl"), got)
	})
	for _, tc := range []struct{ name, cwd, component string }{
		{"255 byte checkout component", "/" + strings.Repeat("a", 251), "--" + strings.Repeat("a", 251) + "--"},
		{"long checkout", "/" + strings.Repeat("0", 150) + "/" + strings.Repeat("0", 150), "--8b198fcfb9c0abfa873a9f4c46fc3a59--"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if filepath.Separator != '/' {
				t.Skip("fixture uses a POSIX absolute path")
			}
			agent := t.TempDir()
			t.Setenv("PI_CODING_AGENT_DIR", agent)
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
			got, err := piDestination(t.Context(), tc.cwd, "id.jsonl")
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(agent, "sessions", tc.component, "id.jsonl"), got)
		})
	}

	cwd, encoded := "/work dir", "--work dir--"
	if filepath.Separator == '\\' {
		cwd, encoded = `C:\work dir`, "--C--work dir--"
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "relative-agent")
	got, err := piDestination(t.Context(), cwd, "id.jsonl")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "relative-agent", "sessions", encoded, "id.jsonl"), got)
	urlDir := t.TempDir()
	urlPath := filepath.ToSlash(urlDir)
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", (&url.URL{Scheme: "file", Path: urlPath}).String())
	got, err = piDestination(t.Context(), cwd, "id.jsonl")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(urlDir, "id.jsonl"), got)
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
	_, err = Materialize(cancelled, cfg, s, cwd)
	require.ErrorIs(t, err, context.Canceled)
	result, err := Materialize(t.Context(), cfg, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, good, result.Source)
	// An invalid stored identity is returned rather than falling back.
	require.NoError(t, os.WriteFile(other, []byte(`{"type":"session","id":"other","cwd":"/old"}`+"\n"), 0o600))
	s.FilePath = &other
	_, err = Materialize(t.Context(), cfg, s, cwd)
	require.ErrorIs(t, err, errPiIdentity)
	s.FilePath = &stale
	require.NoError(t, os.WriteFile(other, data, 0o600))
	_, err = Materialize(t.Context(), cfg, s, cwd)
	require.ErrorContains(t, err, "ambiguous")
	require.NoError(t, os.Remove(good))
	f, err = os.OpenFile(other, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxSourceBytes+1))
	require.NoError(t, f.Close())
	_, err = Materialize(t.Context(), cfg, s, cwd)
	require.ErrorContains(t, err, "exceeds size limit")
	assert.Contains(t, err.Error(), "reading native Pi transcript")
	require.NoError(t, os.Remove(other))
	fallback := filepath.Join(root, "target.jsonl")
	require.NoError(t, os.WriteFile(fallback, []byte(`{"type":"session","id":null,"cwd":"/old"}`+"\n"), 0o600))
	selected, _, err := findPiSource(t.Context(), cfg, s, stale, "target")
	require.NoError(t, err)
	assert.Equal(t, fallback, selected)
}

func TestPiSourceLookupCandidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{{"unreadable", os.ErrPermission}, {"large header", errPiHeaderSize}, {"large unrelated header", nil}, {"unsupported version", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
			t.Setenv("PI_CODING_AGENT_DIR", "")
			t.Setenv("PI_DIR", "")
			data := []byte(`{"type":"session","id":"target","cwd":"/old"}` + "\n")
			target := filepath.Join(root, "readable.jsonl")
			require.NoError(t, os.WriteFile(target, data, 0o600))
			candidate := filepath.Join(root, "candidate.jsonl")
			switch tc.name {
			case "unreadable":
				require.NoError(t, os.WriteFile(candidate, data, 0o000))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(candidate, 0o600)) })
				f, err := os.Open(candidate)
				if err == nil {
					require.NoError(t, f.Close())
					t.Skip("file permissions do not prevent reads")
				}
				require.ErrorIs(t, err, os.ErrPermission)
			case "unsupported version":
				require.NoError(t, os.WriteFile(candidate, []byte(`{"type":"session","id":"target","version":4,"cwd":"/old"}`+"\n"), 0o600))
			default:
				size, id := 64<<20, "target"
				if tc.name != "large header" {
					size = 1 << 20
				}
				switch tc.name {
				case "large unrelated header":
					id = "other"
				}
				header := `{"type":"session","parentSession":"` + strings.Repeat("a", size) + `","id":"` + id + `","cwd":"/old"}` + "\n"
				require.NoError(t, os.WriteFile(candidate, []byte(header), 0o600))
			}
			cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
			stale := filepath.Join(root, "missing.jsonl")
			session := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &stale}
			result, err := Materialize(t.Context(), cfg, session, cwd)
			if tc.name == "unsupported version" {
				require.ErrorContains(t, err, "version")
			} else if tc.want == nil {
				require.NoError(t, err)
				assert.Equal(t, target, result.Source)
				assert.Equal(t, []string{filepath.Join(dest, "target.jsonl")}, result.Paths)
				return
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			_, err = os.Stat(filepath.Join(dest, "target.jsonl"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestMaterializePiConfiguredSources(t *testing.T) {
	for _, mirror := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "mirror"}[mirror], func(t *testing.T) {
			root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			checkout := cwd
			if !mirror {
				parent, err := filepath.EvalSymlinks(t.TempDir())
				require.NoError(t, err)
				checkout = filepath.Join(parent, "checkout")
				require.NoError(t, os.Mkdir(checkout, 0o700))
				cwd = filepath.Join(parent, "link")
				if err := os.Symlink(checkout, cwd); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
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
			result, err := Materialize(t.Context(), cfg, s, cwd)
			require.NoError(t, err)
			if !mirror {
				assert.Equal(t, checkout, result.Cwd)
			}
			require.Equal(t, "installed", result.Status)
			require.Equal(t, []string{filepath.Join(dest, basename)}, result.Paths)
			got, err := os.ReadFile(result.Paths[0])
			require.NoError(t, err)
			// All records and header fields except the cwd value must stay byte-identical.
			assert.Equal(t, `{"type":"session", "id":"abc-session", "cwd" : `+string(mustJSON(t, result.Cwd))+`, "parentSession":"/parent.jsonl"}`+"\n"+`{"type":"message","id":"branch","parentId":"first","message":{"role":"user","content":"hello"}}`+"\n", string(got))
			got, err = os.ReadFile(name)
			require.NoError(t, err)
			assert.Equal(t, original, string(got))
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
	result, err := Materialize(t.Context(), cfg, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, linked, result.Source)
	// A saved path outside the provider roots is not itself authoritative.
	s.FilePath = &original
	result, err = Materialize(t.Context(), cfg, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, linked, result.Source)
}

func TestPiHeaderIdentityAndVersion(t *testing.T) {
	large := `{"type":"session","parentSession":"` + strings.Repeat("a", 1<<20) + `","id":"id","cwd":"/old"}` + "\n"
	got, err := relocatePiHeader(t.Context(), []byte(large), "id.jsonl", "id", "/new")
	require.NoError(t, err)
	assert.Equal(t, strings.Replace(large, `"cwd":"/old"`, `"cwd":"/new"`, 1), string(got))
	data := []byte(`{"type":"session", "cwd":"/old", "extra":1}` + "\n")
	got, err = relocatePiHeader(t.Context(), data, "id.jsonl", "id", "/new")
	require.NoError(t, err)
	assert.Equal(t, `{"type":"session", "cwd":"/new","id":"id", "extra":1}`+"\n", string(got))
	prefix := "\nnot-json\nnull\nfalse\n0.0\n\"\"\n"
	got, err = relocatePiHeader(t.Context(), []byte(prefix+`{"type":"session","id":"id","cwd":"/old"}`+"\n"), "id.jsonl", "id", "/new")
	require.NoError(t, err)
	assert.Equal(t, prefix+`{"type":"session","id":"id","cwd":"/new"}`+"\n", string(got))
	_, err = relocatePiHeader(t.Context(), []byte(`{"type":"session","id":"id","cwd":"/old"}`), "id.jsonl", "id", string([]byte{0xff}))
	require.ErrorContains(t, err, "UTF-8")
	for _, tc := range []struct{ header, id, want string }{
		{`{"type":"session","id":"other","cwd":"/old"}`, "id", "identity"},
		{`{"type":"session","id":"id"}`, "id", "one cwd"},
		{`{"type":"session","cwd":"/old"}`, "id", "identity"},
		{`{"type":"session","id":"id","version":4,"cwd":"/old"}`, "id", "version"},
		{`{"type":"title"}` + "\n" + `{"type":"session","id":"id","cwd":"/old"}`, "id", "header"},
		{`{"type":123,"id":"id"}`, "id", "header"},
		{`{"type":"session","id":"id","version":"3"}`, "id", "header"},
		{"", "id", "header"},
	} {
		t.Run(tc.header, func(t *testing.T) {
			_, err := relocatePiHeader(t.Context(), []byte(tc.header), "other.jsonl", tc.id, "/new")
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
	for _, tc := range []struct{ name, before, after string }{
		{"empty", `"id" : "", "cwd" : "/old"`, `"id" : "id", "cwd" : "/new"`},
		{"null", `"id" : null, "cwd" : "/old"`, `"id" : "id", "cwd" : "/new"`},
		{"id after cwd", `"cwd" : "/old", "id" : null`, `"cwd" : "/new", "id" : "id"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("\n" + `{"type":"session", ` + tc.before + `, "other": [1, 2]}` + "\n" + `{"type":"message"}` + "\n")
			got, err := relocatePiHeader(t.Context(), data, "id.jsonl", "id", "/new")
			require.NoError(t, err)
			assert.Equal(t, "\n"+`{"type":"session", `+tc.after+`, "other": [1, 2]}`+"\n"+`{"type":"message"}`+"\n", string(got))
		})
	}
}

func TestPiUsesIdentityForNonuniqueNames(t *testing.T) {
	for _, tc := range []struct{ id, wantBase string }{
		{"child-one", "child-one.jsonl"},
		{"work.v2", "work.v2.jsonl"},
		{"..", ""},
		{".work", ""},
		{"work.", ""},
		{"a/b", ""},
		{`a\b`, ""},
	} {
		t.Run(tc.id, func(t *testing.T) {
			root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
			t.Setenv("PI_CODING_AGENT_DIR", "")
			cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
			name := filepath.Join(root, "session.jsonl")
			original := `{"type":"session","id":` + string(mustJSON(t, tc.id)) + `,"cwd":"/old"}` + "\n"
			require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
			session := &db.Session{ID: "pi:" + tc.id, Agent: "pi", Machine: "local", FilePath: &name}
			result, err := Materialize(t.Context(), cfg, session, cwd)
			if tc.wantBase != "" {
				require.NoError(t, err)
				assert.Equal(t, []string{filepath.Join(dest, tc.wantBase)}, result.Paths)
				return
			}
			require.ErrorContains(t, err, "cannot be used as a filename")
			entries, err := os.ReadDir(dest)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
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
