package sessionmaterialize

import (
	"context"
	"encoding/json"
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
	t.Run("relative destination", func(t *testing.T) {
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
		assert.Contains(t, got, filepath.Join(cwd, "relative-agent", "sessions"))
		if filepath.Separator == '/' {
			got, err = piDestination("/work dir", "id.jsonl")
			require.NoError(t, err)
			assert.Equal(t, "/work dir/relative-agent/sessions/--work dir--/id.jsonl", got)
		}

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
	// An unsupported matching candidate aborts discovery.
	require.NoError(t, os.WriteFile(other, []byte(`{"type":"session","id":"target","version":4,"cwd":"/old"}`+"\n"), 0o600))
	_, err = Materialize(t.Context(), cfg, s, cwd)
	require.ErrorContains(t, err, "version")
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
	require.NoError(t, os.WriteFile(good, data, 0o600))
	require.NoError(t, os.Remove(good))
	fallback := filepath.Join(root, "target.jsonl")
	require.NoError(t, os.WriteFile(fallback, []byte(`{"type":"session","id":null,"cwd":"/old"}`+"\n"), 0o600))
	_, _, err = findPiSource(t.Context(), cfg, s, stale, "target")
	require.NoError(t, err)
}

func TestPiSourceLookupRejectsUncertainCandidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{{"unreadable", os.ErrPermission}, {"large header", errPiHeaderSize}} {
		t.Run(tc.name, func(t *testing.T) {
			root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
			t.Setenv("PI_CODING_AGENT_DIR", "")
			t.Setenv("PI_DIR", "")
			data := []byte(`{"type":"session","id":"target","cwd":"/old"}` + "\n")
			require.NoError(t, os.WriteFile(filepath.Join(root, "readable.jsonl"), data, 0o600))
			candidate := filepath.Join(root, "candidate.jsonl")
			if tc.name == "unreadable" {
				require.NoError(t, os.WriteFile(candidate, data, 0o000))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(candidate, 0o600)) })
				f, err := os.Open(candidate)
				if err == nil {
					require.NoError(t, f.Close())
					t.Skip("file permissions do not prevent reads")
				}
				require.ErrorIs(t, err, os.ErrPermission)
			} else {
				header := `{"type":"session","extra":"` + strings.Repeat("a", 1<<20) + `","id":"target","cwd":"/old"}` + "\n"
				require.NoError(t, os.WriteFile(candidate, []byte(header), 0o600))
			}
			cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
			stale := filepath.Join(root, "missing.jsonl")
			session := &db.Session{ID: "pi:target", Agent: "pi", Machine: "local", FilePath: &stale}
			_, err := Materialize(t.Context(), cfg, session, cwd)
			require.ErrorIs(t, err, tc.want)
			_, err = os.Stat(filepath.Join(dest, "target.jsonl"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
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
			original := `{"type":"session", "id":"abc-session", "cwd" : "/old path", "parentSession":"/parent.jsonl"}` + "\n" + `{"type":"message","id":"branch","parentId":"first","message":{"role":"user","content":"hello"}}` + "\n"
			require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
			result, err := Materialize(t.Context(), cfg, s, cwd)
			require.NoError(t, err)
			require.Equal(t, "installed", result.Status)
			require.Equal(t, []string{filepath.Join(dest, basename)}, result.Paths)
			got, err := os.ReadFile(result.Paths[0])
			require.NoError(t, err)
			// All records and header fields except the cwd value must stay byte-identical.
			assert.Equal(t, `{"type":"session", "id":"abc-session", "cwd" : `+string(mustJSON(t, cwd))+`, "parentSession":"/parent.jsonl"}`+"\n"+`{"type":"message","id":"branch","parentId":"first","message":{"role":"user","content":"hello"}}`+"\n", string(got))
			appended := append([]byte(nil), got...)
			appended = append(appended, []byte("{\"type\":\"message\"}\n")...)
			require.NoError(t, os.WriteFile(result.Paths[0], appended, 0o600))
			_, err = Materialize(t.Context(), cfg, s, cwd)
			require.Error(t, err)
			got, err = os.ReadFile(result.Paths[0])
			require.NoError(t, err)
			assert.Equal(t, appended, got)
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
	missing := filepath.Join(root, "missing.jsonl")
	s.FilePath = &missing
	result, err = Materialize(t.Context(), cfg, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, linked, result.Source)
	// A saved path outside the provider roots is not itself authoritative.
	s.FilePath = &original
	result, err = Materialize(t.Context(), cfg, s, cwd)
	require.NoError(t, err)
	assert.Equal(t, linked, result.Source)
}

func TestPiHeaderIdentityAndVersion(t *testing.T) {
	data := []byte(`{"type":"session", "cwd":"/old", "extra":1}` + "\n")
	got, err := relocatePiHeader(t.Context(), data, "id.jsonl", "id", "/new")
	require.NoError(t, err)
	assert.Equal(t, `{"type":"session", "cwd":"/new","id":"id", "extra":1}`+"\n", string(got))
	_, err = relocatePiHeader(t.Context(), data, "other.jsonl", "id", "/new")
	require.ErrorIs(t, err, errPiIdentity)
	prefix := "\nnot-json\nnull\nfalse\n0.0\n\"\"\n"
	got, err = relocatePiHeader(t.Context(), []byte(prefix+`{"type":"session","id":"id","cwd":"/old"}`+"\n"), "id.jsonl", "id", "/new")
	require.NoError(t, err)
	assert.Equal(t, prefix+`{"type":"session","id":"id","cwd":"/new"}`+"\n", string(got))
	_, err = relocatePiHeader(t.Context(), []byte(`{"type":"session","id":"id","cwd":"/old"}`), "id.jsonl", "id", string([]byte{0xff}))
	require.ErrorContains(t, err, "UTF-8")
	for _, tc := range []struct{ header, id, want string }{
		{`{"type":"session","id":"other","cwd":"/old"}`, "id", "identity"},
		{`{"type":"session","id":"id","cwd":"/a","cwd":"/b"}`, "id", "one cwd"},
		{`{"type":"session","id":"id","cwd":"/a","c\u0077d":"/b"}`, "id", "one cwd"},
		{`{"type":"session","id":"id"}`, "id", "one cwd"},
		{`{"type":"message","id":"id","cwd":"/old"}`, "id", "header"},
		{`{"type":"session","cwd":"/old"}`, "id", "identity"},
		{`{"type":"session","id":"id","version":4,"cwd":"/old"}`, "id", "version"},
		{`{"type":"title"}` + "\n" + `{"type":"session","id":"id","cwd":"/old"}`, "id", "header"},
		{`{"type":123,"id":"id"}`, "id", "header"},
		{`{"type":"session","id":123}`, "id", "identity"},
		{`{"type":"session","id":"id","version":"3"}`, "id", "header"},
		{`{"Type":"session","id":"id"}`, "id", "header"},
		{`{"type":"session","ID":"id"}`, "id", "identity"},
		{"", "id", "header"},
		{"not-json", "id", "header"},
	} {
		t.Run(tc.want, func(t *testing.T) {
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
}

func TestPiHeaderFilenameIdentityPreservesBytes(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"empty", `"id" : ""`},
		{"null", `"id" : null`},
		{"object", `"id" : {"nested":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("\n" + `{"type":"session", ` + tc.field + `, "cwd" : "/old", "other": [1, 2]}` + "\n" + `{"type":"message"}` + "\n")
			got, err := relocatePiHeader(t.Context(), data, "id.jsonl", "id", "/new")
			require.NoError(t, err)
			assert.Equal(t, "\n"+`{"type":"session", "id" : "id", "cwd" : "/new", "other": [1, 2]}`+"\n"+`{"type":"message"}`+"\n", string(got))
			if tc.name == "object" {
				_, err = relocatePiHeader(t.Context(), data, "other.jsonl", "id", "/new")
				require.ErrorIs(t, err, errPiIdentity)
			}
		})
	}
	got, err := relocatePiHeader(t.Context(), []byte(`{"type":"session","cwd":"/old","id":null}`), "id.jsonl", "id", "/new")
	require.NoError(t, err)
	assert.Equal(t, `{"type":"session","cwd":"/new","id":"id"}`, string(got))
	_, err = relocatePiHeader(t.Context(), []byte(`{"type":"session","id":"other","cwd":"/old"}`), "id.jsonl", "id", "/new")
	require.ErrorIs(t, err, errPiIdentity)
}

func TestPiStoredSourceWithLargeHeader(t *testing.T) {
	root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	name := filepath.Join(root, "id.jsonl")
	header := `{"type":"session","id":"id","extra":"` + strings.Repeat("a", 1<<20) + `","cwd":"/old"}` + "\n"
	require.NoError(t, os.WriteFile(name, []byte(header), 0o600))
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	session := &db.Session{ID: "pi:id", Agent: "pi", Machine: "local", FilePath: &name}
	result, err := Materialize(t.Context(), cfg, session, cwd)
	require.NoError(t, err)
	got, err := os.ReadFile(result.Paths[0])
	require.NoError(t, err)
	assert.Contains(t, string(got), `"cwd":`+string(mustJSON(t, result.Cwd)))
}

func TestPiUsesIdentityForNonuniqueNames(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	for _, tc := range []struct{ id, rel, wantBase string }{
		{"child-one", "child-one/session.jsonl", "child-one.jsonl"},
		{"work.v2", "work.v2/session.jsonl", "work.v2.jsonl"},
	} {
		t.Run(tc.rel, func(t *testing.T) {
			id := tc.id
			name := filepath.Join(root, filepath.FromSlash(tc.rel))
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
			original := `{"type":"session","id":"` + id + `","cwd":"/old"}` + "\n"
			require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
			t.Cleanup(func() { assert.NoError(t, os.Remove(name)) })
			s := &db.Session{ID: "pi:" + id, Agent: "pi", Machine: "local", FilePath: &name}
			result, err := Materialize(t.Context(), cfg, s, cwd)
			require.NoError(t, err)
			require.Equal(t, []string{filepath.Join(dest, tc.wantBase)}, result.Paths)
			got, err := os.ReadFile(result.Paths[0])
			require.NoError(t, err)
			assert.Equal(t, `{"type":"session","id":"`+id+`","cwd":`+string(mustJSON(t, cwd))+"}\n", string(got))
		})
	}
}

func TestPiRejectsUnsafeIdentityFilenames(t *testing.T) {
	root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cfg := config.Config{InstallationID: "local", LocalMachineName: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentPi: {root}}}
	for i, id := range []string{"..", ".work", "work.", "a/b", `a\b`} {
		name := filepath.Join(root, string(rune('0'+i)), "session.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
		original := `{"type":"session","id":` + string(mustJSON(t, id)) + `,"cwd":"/old"}` + "\n"
		require.NoError(t, os.WriteFile(name, []byte(original), 0o600))
		s := &db.Session{ID: "pi:" + id, Agent: "pi", Machine: "local", FilePath: &name}
		_, err := Materialize(t.Context(), cfg, s, cwd)
		require.ErrorContains(t, err, "cannot be used as a filename")
		require.NoError(t, os.Remove(name))
	}
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	assert.Empty(t, entries)
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
	result, err := Materialize(t.Context(), cfg, s, link)
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
