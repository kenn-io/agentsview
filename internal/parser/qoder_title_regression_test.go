package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQoderTitleRegressionMirrorRootMustNotBindLocalDatabase(t *testing.T) {
	root := filepath.Join(t.TempDir(), "remote-mirror", ".qoder-cn", "projects")
	got := resolveQoderAppSQLitePath(filepath.Join(root, "p", "s.jsonl"), []string{root})
	assert.Empty(t, got, "a mirror directory does not prove ownership of this machine's Application Support database")
}

func TestQoderTitleRegressionProviderBindsOnlyLocalSources(t *testing.T) {
	for _, mode := range []string{"local", "other machine", "rewritten mirror"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := filepath.Join(home, ".qoder", "projects")
			writeQoderTitleTestSession(t, root, qoderTitleTestID)
			title := "local database title"
			path := filepath.Join(home, "Library", "Application Support", qoderIntlClientApp, "main.sqlite")
			writeQoderTitleDB(t, path, map[string]*string{qoderTitleTestID: &title})
			cfg := ProviderConfig{Roots: []string{root}, Machine: "local"}
			if mode == "other machine" {
				cfg.SourceMachines = map[string]string{root: "remote"}
			}
			if mode == "rewritten mirror" {
				cfg.PathRewriter = func(s string) string { return "remote:" + s }
			}
			provider, ok := NewProvider(AgentQoder, cfg)
			require.True(t, ok)
			result := qoderTitleTestParse(t, provider, root)[0]
			assert.Equal(t, mode == "local", result.Result.Session.SessionNamePresent)
			watchRoots, err := ResolveWatchRoots(t.Context(), provider)
			require.NoError(t, err)
			found := false
			for _, watch := range watchRoots {
				if watch.Path == filepath.Dir(path) {
					found = true
				}
			}
			assert.Equal(t, mode == "local", found)
		})
	}
}
