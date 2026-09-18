package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeClineDiscoverySession(t *testing.T, sessionsDir, sessionID string) string {
	t.Helper()
	dir := filepath.Join(sessionsDir, sessionID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	metaPath := filepath.Join(dir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"session_id":"`+sessionID+`"}`), 0o644))
	return metaPath
}

func clineDiscoverPaths(t *testing.T, provider Provider) ([]string, error) {
	t.Helper()
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	var paths []string
	err := discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
		paths = append(paths, source.DisplayPath)
		return nil
	})
	return paths, err
}

func TestClineDiscovery(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	meta1 := writeClineDiscoverySession(t, sessionsDir, "1789000000001_aaa")
	meta2 := writeClineDiscoverySession(t, sessionsDir, "1789000000002_bbb")

	// Create entries that should be skipped: underscore dir, dot dir, stray file, empty dir
	require.NoError(os.MkdirAll(filepath.Join(sessionsDir, "_index"), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(sessionsDir, "empty_session"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(sessionsDir, "stray.json"), []byte("{}"), 0o644))
	writeClineDiscoverySession(t, sessionsDir, ".secret")

	provider, ok := NewProvider(AgentCline, ProviderConfig{
		Roots: []string{root},
	})
	require.True(ok)

	paths, err := clineDiscoverPaths(t, provider)
	require.NoError(err)
	assert.ElementsMatch(t, []string{meta1, meta2}, paths)
}

func TestClineDiscovery_DirectSessionsRoot(t *testing.T) {
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	meta := writeClineDiscoverySession(t, sessionsDir, "1789000000003_ccc")

	provider, ok := NewProvider(AgentCline, ProviderConfig{
		Roots: []string{sessionsDir},
	})
	require.True(t, ok)

	paths, err := clineDiscoverPaths(t, provider)
	require.NoError(t, err)
	assert.Equal(t, []string{meta}, paths)
}

func TestClineClassifyPath(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	sessionID := "1789000000004_ddd"
	metaPath := writeClineDiscoverySession(t, sessionsDir, sessionID)
	msgPath := filepath.Join(sessionsDir, sessionID, sessionID+".messages.json")
	require.NoError(t, os.WriteFile(msgPath, []byte(`{"messages":[]}`), 0o644))

	tests := []struct {
		name         string
		path         string
		allowMissing bool
		wantMatch    bool
		wantPath     string
	}{
		{
			name:         "exact metadata file",
			path:         metaPath,
			allowMissing: false,
			wantMatch:    true,
			wantPath:     metaPath,
		},
		{
			name:         "companion messages file maps to metadata",
			path:         msgPath,
			allowMissing: false,
			wantMatch:    true,
			wantPath:     metaPath,
		},
		{
			name:         "missing metadata file is not a source",
			path:         filepath.Join(sessionsDir, "missing", "missing.json"),
			allowMissing: true,
			wantMatch:    false,
		},
		{
			name:         "underscore dir skipped",
			path:         filepath.Join(sessionsDir, "_internal", "_internal.json"),
			allowMissing: true,
			wantMatch:    false,
		},
		{
			name:         "dot-prefixed dir skipped",
			path:         filepath.Join(sessionsDir, ".secret", ".secret.json"),
			allowMissing: true,
			wantMatch:    false,
		},
		{
			name:         "unrelated file",
			path:         filepath.Join(sessionsDir, sessionID, "notes.txt"),
			allowMissing: true,
			wantMatch:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := clineClassifyPath(root, tt.path, tt.allowMissing)
			assert.Equal(t, tt.wantMatch, ok)
			if tt.wantMatch {
				assert.Equal(t, tt.wantPath, m.Path)
			}
		})
	}
}

func TestClineFindFile(t *testing.T) {
	assert := assert.New(t)

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	sessionID := "1789000000005_eee"
	metaPath := writeClineDiscoverySession(t, sessionsDir, sessionID)

	m, ok := clineFindFile(root, sessionID)
	assert.True(ok)
	assert.Equal(metaPath, m.Path)

	_, ok = clineFindFile(root, "non-existent")
	assert.False(ok)

	for _, hostileID := range []string{
		"",
		".",
		"..",
		"../outside",
		"../../etc/passwd",
		"foo/bar",
		"foo\\bar",
		"/etc/passwd",
	} {
		_, ok := clineFindFile(root, hostileID)
		assert.False(ok, "hostile rawID %q must be rejected", hostileID)
	}
}

func TestClineFingerprintSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionID := "1789000000006_fff"
	dir := filepath.Join(root, sessionID)
	require.NoError(os.MkdirAll(dir, 0o755))

	metaPath := filepath.Join(dir, sessionID+".json")
	msgPath := filepath.Join(dir, sessionID+".messages.json")

	require.NoError(os.WriteFile(metaPath, []byte(`{"session_id":"test"}`), 0o644))
	require.NoError(os.WriteFile(msgPath, []byte(`{"messages":[]}`), 0o644))

	fp1, err := clineFingerprintSource(metaPath)
	require.NoError(err)
	assert.NotEmpty(fp1.Hash)

	// Modifying messages file must change composite fingerprint
	require.NoError(os.WriteFile(msgPath, []byte(`{"messages":[{"id":"1"}]}`), 0o644))
	fp2, err := clineFingerprintSource(metaPath)
	require.NoError(err)
	assert.NotEqual(fp1.Hash, fp2.Hash)
	assert.NotEqual(fp1.Size, fp2.Size)
}

func TestClinePrimarySymlinksAreRejected(t *testing.T) {
	parentAssert := assert.New(t)
	parentRequire := require.New(t)

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	sessionID := "1789000000007_symlink"
	sessDir := filepath.Join(sessionsDir, sessionID)
	parentRequire.NoError(os.MkdirAll(sessDir, 0o755))
	metaPath := filepath.Join(sessDir, sessionID+".json")
	msgPath := filepath.Join(sessDir, sessionID+".messages.json")
	metaTarget := filepath.Join(t.TempDir(), "metadata.json")
	msgTarget := filepath.Join(t.TempDir(), "messages.json")
	parentRequire.NoError(os.WriteFile(metaTarget, []byte(`{"session_id":"`+sessionID+`"}`), 0o644))
	parentRequire.NoError(os.WriteFile(msgTarget, []byte(`{"messages":[]}`), 0o644))
	parentRequire.NoError(os.WriteFile(metaPath, []byte(`{"session_id":"`+sessionID+`"}`), 0o644))

	t.Run("messages symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		symlinkOrSkip(t, msgTarget, msgPath)
		provider, ok := NewProvider(AgentCline, ProviderConfig{Roots: []string{root}})
		require.True(ok)
		paths, err := clineDiscoverPaths(t, provider)
		require.NoError(err)
		assert.NotContains(paths, metaPath)
		_, err = clineFingerprintSource(metaPath)
		assert.Error(err)
		require.NoError(os.Remove(msgPath))
	})

	t.Run("metadata symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		symlinkOrSkip(t, metaTarget, metaPath)
		provider, ok := NewProvider(AgentCline, ProviderConfig{Roots: []string{root}})
		require.True(ok)
		paths, err := clineDiscoverPaths(t, provider)
		require.NoError(err)
		assert.NotContains(paths, metaPath)
		match, ok := clineClassifyPath(root, metaPath, false)
		assert.False(ok)
		assert.Empty(match)
		require.NoError(os.Remove(metaPath))
	})

	// A changed symlink path is rejected before it can be mapped to the parent
	// metadata source, even when the target contains valid JSON.
	parentRequire.NoError(os.WriteFile(metaPath, []byte(`{"session_id":"`+sessionID+`"}`), 0o644))
	symlinkOrSkip(t, msgTarget, msgPath)
	_, ok := clineClassifyPath(root, msgPath, false)
	parentAssert.False(ok)
	_, err := clineFingerprintSource(metaPath)
	parentAssert.Error(err)
	parentRequire.NoError(os.Remove(msgPath))
	// Changing the former symlink target must not make the source fingerprint
	// readable through the link.
	parentRequire.NoError(os.WriteFile(msgTarget, []byte(`{"messages":[{"id":"changed"}]}`), 0o644))
}

func TestClineSessionDirectorySymlinkEscapingRootIsRejected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	require.NoError(os.MkdirAll(sessionsDir, 0o755))

	sessionID := "1789000000008_session_escape"
	outsideSession := filepath.Join(t.TempDir(), sessionID)
	require.NoError(os.MkdirAll(outsideSession, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(outsideSession, sessionID+".json"),
		[]byte(`{"session_id":"`+sessionID+`"}`), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(outsideSession, sessionID+".messages.json"),
		[]byte(`{"messages":[]}`), 0o644,
	))

	linkedSession := filepath.Join(sessionsDir, sessionID)
	symlinkOrSkip(t, outsideSession, linkedSession)
	metaPath := filepath.Join(linkedSession, sessionID+".json")

	provider, ok := NewProvider(AgentCline, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	paths, err := clineDiscoverPaths(t, provider)
	require.NoError(err)
	assert.Empty(paths, "a session directory symlink must not be discovered")

	_, ok = clineClassifyPath(root, metaPath, false)
	assert.False(ok, "a changed path through a session-directory symlink must be rejected")
	_, ok = clineFindFile(root, sessionID)
	assert.False(ok, "lookup must not resolve a session-directory symlink")

	scopes := provider.(StoredSourceHintScopeProvider).StoredSourceHintScopes(
		ChangedPathRequest{Path: metaPath},
	)
	assert.Empty(scopes, "an escaping session directory must have no ownership scope")

	_, _, err = clineParseFile(singleFileSource{Root: root, Path: metaPath}, ParseRequest{})
	assert.Error(err, "parsing must reject the path before reading the external metadata")
}

func TestClineDiscovery_MissingDataSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "settings"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(root, "settings", "settings.json"), []byte("{}"), 0o644))

	assert.Equal(filepath.Join(root, "data", "sessions"), clineResolveSessionsDir(root))

	provider, ok := NewProvider(AgentCline, ProviderConfig{
		Roots: []string{root},
	})
	require.True(ok)

	paths, err := clineDiscoverPaths(t, provider)
	require.NoError(err)
	assert.Empty(paths)
}

func TestClineFingerprint_Teammates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sessionID := "sess-fp-test"
	sessDir := filepath.Join(dir, sessionID)
	require.NoError(os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, sessionID+".json")
	msgPath := filepath.Join(sessDir, sessionID+".messages.json")
	require.NoError(os.WriteFile(metaPath, []byte(`{"session_id":"sess-fp-test"}`), 0o644))
	require.NoError(os.WriteFile(msgPath, []byte(`{"messages":[]}`), 0o644))

	fp1, err := clineFingerprintSource(metaPath)
	require.NoError(err)

	// Adding a teammate file must invalidate fingerprint
	teammatePath := filepath.Join(sessDir, "git-scout__t1.messages.json")
	require.NoError(os.WriteFile(teammatePath, []byte(`{"messages":[{"id":"m1"}]}`), 0o644))

	fp2, err := clineFingerprintSource(metaPath)
	require.NoError(err)
	assert.NotEqual(fp1.Hash, fp2.Hash)
	assert.True(fp2.Size > fp1.Size)

	// Modifying teammate file must invalidate fingerprint again
	require.NoError(os.WriteFile(teammatePath, []byte(`{"messages":[{"id":"m1"},{"id":"m2"}]}`), 0o644))
	fp3, err := clineFingerprintSource(metaPath)
	require.NoError(err)
	assert.NotEqual(fp2.Hash, fp3.Hash)
}

func TestClineClassifyPath_Teammates(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	sessionID := "sess-classify-1"
	sessDir := filepath.Join(root, "data", "sessions", sessionID)
	require.NoError(os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, sessionID+".json")
	require.NoError(os.WriteFile(metaPath, []byte(`{}`), 0o644))

	teammatePath := filepath.Join(sessDir, "scout__t1.messages.json")
	match, ok := clineClassifyPath(root, teammatePath, false)
	require.True(ok)
	assert.Equal(t, metaPath, match.Path)
}

func TestClineProviderCapabilities(t *testing.T) {
	assert := assert.New(t)

	provider, ok := NewProvider(AgentCline, ProviderConfig{Roots: []string{t.TempDir()}})
	require.True(t, ok)
	caps := provider.Capabilities()

	assert.Equal(CapabilitySupported, caps.Source.StoredSourceHints)
	assert.Equal(CapabilityNotApplicable, caps.Source.MultiSessionSource)
	assert.Equal(CapabilityNotApplicable, caps.Source.ExcludedSessions)
	assert.Equal(CapabilityNotApplicable, caps.Source.ForceReplaceOnParse)
}

func TestClineStoredSourceHintScope(t *testing.T) {
	parentRequire := require.New(t)

	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	sessionID := "1789000000001_scope"
	sessDir := filepath.Join(sessionsDir, sessionID)
	parentRequire.NoError(os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, sessionID+".json")
	parentRequire.NoError(os.WriteFile(metaPath, []byte(`{}`), 0o644))
	msgPath := filepath.Join(sessDir, sessionID+".messages.json")
	parentRequire.NoError(os.WriteFile(msgPath, []byte(`{}`), 0o644))
	tmPath := filepath.Join(sessDir, "worker__t1.messages.json")
	parentRequire.NoError(os.WriteFile(tmPath, []byte(`{}`), 0o644))

	deletedSessionID := "1789000000002_deleted"
	deletedSessDir := filepath.Join(sessionsDir, deletedSessionID)
	parentRequire.NoError(os.MkdirAll(deletedSessDir, 0o755))
	deletedMetaPath := filepath.Join(deletedSessDir, deletedSessionID+".json")
	parentRequire.NoError(os.WriteFile(deletedMetaPath, []byte(`{}`), 0o644))
	parentRequire.NoError(os.Remove(deletedMetaPath))

	provider, ok := NewProvider(AgentCline, ProviderConfig{Roots: []string{root}})
	parentRequire.True(ok)
	resolver, ok := provider.(StoredSourceHintScopeProvider)
	parentRequire.True(ok, "cline provider must implement StoredSourceHintScopeProvider")

	tests := []struct {
		name      string
		path      string
		wantScope bool
		wantPath  string
	}{
		{
			name:      "metadata path resolves to session directory",
			path:      metaPath,
			wantScope: true,
			wantPath:  sessDir,
		},
		{
			name:      "deleted metadata path resolves to session directory",
			path:      deletedMetaPath,
			wantScope: true,
			wantPath:  deletedSessDir,
		},
		{
			name:      "companion messages path resolves to session directory",
			path:      msgPath,
			wantScope: true,
			wantPath:  sessDir,
		},
		{
			name:      "teammate transcript path resolves to same session directory",
			path:      tmPath,
			wantScope: true,
			wantPath:  sessDir,
		},
		{
			name:      "path outside cline layout returns no scope",
			path:      filepath.Join(root, "unrelated.txt"),
			wantScope: false,
		},
		{
			name:      "sessions directory root itself returns no scope",
			path:      sessionsDir,
			wantScope: false,
		},
		{
			name:      "hidden file returns no scope",
			path:      filepath.Join(sessDir, ".secret.json"),
			wantScope: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			scopes := resolver.StoredSourceHintScopes(ChangedPathRequest{Path: tt.path})
			if !tt.wantScope {
				assert.Empty(scopes)
				return
			}
			require.Len(t, scopes, 1)
			assert.Equal(tt.wantPath, scopes[0].Path)
			assert.False(scopes[0].IncludeVirtualMembers, "IncludeVirtualMembers must remain false for Cline")
		})
	}
}

func TestClineFindFile_Teammates(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-find-1"
	sessDir := filepath.Join(root, "data", "sessions", sessionID)
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{}`), 0o644))

	validTests := []struct {
		name  string
		rawID string
	}{
		{name: "parent ID", rawID: sessionID},
		{name: "teamtask ID", rawID: sessionID + "__teamtask__scout__t1"},
		{name: "teamtask ID with long nonce", rawID: sessionID + "__teamtask__scout__1789207950000_ab12cd"},
	}

	for _, tt := range validTests {
		t.Run("valid/"+tt.name, func(t *testing.T) {
			m, ok := clineFindFile(root, tt.rawID)
			require.True(t, ok, "expected %s to resolve", tt.rawID)
			assert.Equal(t, metaPath, m.Path)
		})
	}

	hostileTests := []struct {
		name  string
		rawID string
	}{
		{name: "empty ID", rawID: ""},
		{name: "dot", rawID: "."},
		{name: "dot-dot", rawID: ".."},
		{name: "parent traversal", rawID: "../" + sessionID},
		{name: "teamtask with empty suffix", rawID: sessionID + "__teamtask__"},
		{name: "teamtask escape with forward slash", rawID: sessionID + "__teamtask__scout/escape"},
		{name: "teamtask escape with backslash", rawID: sessionID + "__teamtask__scout\\bar"},
		{name: "teamtask with colon", rawID: sessionID + "__teamtask__scout:t1"},
		{name: "parent with colon", rawID: "cline:" + sessionID},
		{name: "parent with backslash", rawID: "sess-find\\evil__teamtask__scout__t1"},
		{name: "parent with forward slash", rawID: "sess-find/evil__teamtask__scout__t1"},
		{name: "dot-prefixed hidden session", rawID: ".secret__teamtask__scout__t1"},
		{name: "underscore-prefixed session", rawID: "_hidden__teamtask__scout__t1"},
		{name: "teamtask with traversal", rawID: "../" + sessionID + "__teamtask__scout__t1"},
		{name: "old teammate form is not an ID", rawID: sessionID + "__teammate__scout"},
		{name: "non-existent session", rawID: "non-existent-session-id"},
	}

	for _, tt := range hostileTests {
		t.Run("hostile/"+tt.name, func(t *testing.T) {
			_, ok := clineFindFile(root, tt.rawID)
			assert.False(t, ok, "hostile rawID %q must be rejected", tt.rawID)
		})
	}
}
