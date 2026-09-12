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
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	meta1 := writeClineDiscoverySession(t, sessionsDir, "1789000000001_aaa")
	meta2 := writeClineDiscoverySession(t, sessionsDir, "1789000000002_bbb")

	// Create entries that should be skipped: underscore dir, dot dir, stray file, empty dir
	require.NoError(t, os.MkdirAll(filepath.Join(sessionsDir, "_index"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(sessionsDir, "empty_session"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessionsDir, "stray.json"), []byte("{}"), 0o644))
	writeClineDiscoverySession(t, sessionsDir, ".secret")

	provider, ok := NewProvider(AgentCline, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	paths, err := clineDiscoverPaths(t, provider)
	require.NoError(t, err)
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
			name:         "missing metadata file with allowMissing",
			path:         filepath.Join(sessionsDir, "missing", "missing.json"),
			allowMissing: true,
			wantMatch:    true,
			wantPath:     filepath.Join(sessionsDir, "missing", "missing.json"),
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
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	sessionID := "1789000000005_eee"
	metaPath := writeClineDiscoverySession(t, sessionsDir, sessionID)

	m, ok := clineFindFile(root, sessionID)
	assert.True(t, ok)
	assert.Equal(t, metaPath, m.Path)

	_, ok = clineFindFile(root, "non-existent")
	assert.False(t, ok)

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
		assert.False(t, ok, "hostile rawID %q must be rejected", hostileID)
	}
}

func TestClineFingerprintSource(t *testing.T) {
	root := t.TempDir()
	sessionID := "1789000000006_fff"
	dir := filepath.Join(root, sessionID)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	metaPath := filepath.Join(dir, sessionID+".json")
	msgPath := filepath.Join(dir, sessionID+".messages.json")

	require.NoError(t, os.WriteFile(metaPath, []byte(`{"session_id":"test"}`), 0o644))
	require.NoError(t, os.WriteFile(msgPath, []byte(`{"messages":[]}`), 0o644))

	fp1, err := clineFingerprintSource(metaPath)
	require.NoError(t, err)
	assert.NotEmpty(t, fp1.Hash)

	// Modifying messages file must change composite fingerprint
	require.NoError(t, os.WriteFile(msgPath, []byte(`{"messages":[{"id":"1"}]}`), 0o644))
	fp2, err := clineFingerprintSource(metaPath)
	require.NoError(t, err)
	assert.NotEqual(t, fp1.Hash, fp2.Hash)
	assert.NotEqual(t, fp1.Size, fp2.Size)
}

func TestClineDiscovery_MissingDataSessions(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "settings"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "settings", "settings.json"), []byte("{}"), 0o644))

	assert.Equal(t, filepath.Join(root, "data", "sessions"), clineResolveSessionsDir(root))

	provider, ok := NewProvider(AgentCline, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	paths, err := clineDiscoverPaths(t, provider)
	require.NoError(t, err)
	assert.Empty(t, paths)
}

func TestClineFingerprint_Teammates(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-fp-test"
	sessDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, sessionID+".json")
	msgPath := filepath.Join(sessDir, sessionID+".messages.json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"session_id":"sess-fp-test"}`), 0o644))
	require.NoError(t, os.WriteFile(msgPath, []byte(`{"messages":[]}`), 0o644))

	fp1, err := clineFingerprintSource(metaPath)
	require.NoError(t, err)

	// Adding a teammate file must invalidate fingerprint
	teammatePath := filepath.Join(sessDir, "git-scout__t1.messages.json")
	require.NoError(t, os.WriteFile(teammatePath, []byte(`{"messages":[{"id":"m1"}]}`), 0o644))

	fp2, err := clineFingerprintSource(metaPath)
	require.NoError(t, err)
	assert.NotEqual(t, fp1.Hash, fp2.Hash)
	assert.True(t, fp2.Size > fp1.Size)

	// Modifying teammate file must invalidate fingerprint again
	require.NoError(t, os.WriteFile(teammatePath, []byte(`{"messages":[{"id":"m1"},{"id":"m2"}]}`), 0o644))
	fp3, err := clineFingerprintSource(metaPath)
	require.NoError(t, err)
	assert.NotEqual(t, fp2.Hash, fp3.Hash)
}

func TestClineClassifyPath_Teammates(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-classify-1"
	sessDir := filepath.Join(root, "data", "sessions", sessionID)
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{}`), 0o644))

	teammatePath := filepath.Join(sessDir, "scout__t1.messages.json")
	match, ok := clineClassifyPath(root, teammatePath, false)
	require.True(t, ok)
	assert.Equal(t, metaPath, match.Path)
}

func TestClineFindFile_Teammates(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-find-1"
	sessDir := filepath.Join(root, "data", "sessions", sessionID)
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{}`), 0o644))

	// Looking up raw teammate session ID resolves to the parent session metadata file
	rawTeammateID := "sess-find-1__teamtask__scout__t1"
	match, ok := clineFindFile(root, rawTeammateID)
	require.True(t, ok)
	assert.Equal(t, metaPath, match.Path)
}
