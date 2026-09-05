package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestCodexSidecarSignatureCoversAliasHomeIndexes(t *testing.T) {
	t.Cleanup(func() { parser.SetCodexRootAliases(nil) })
	base := t.TempDir()
	primary := filepath.Join(base, "codex")
	alias := filepath.Join(base, "codex-alt")
	require.NoError(t, os.MkdirAll(filepath.Join(primary, "sessions"), 0o755))
	require.NoError(t, os.MkdirAll(alias, 0o755))
	require.NoError(t, os.Symlink(
		filepath.Join(primary, "sessions"), filepath.Join(alias, "sessions")))
	parser.SetCodexRootAliases(map[string][]string{
		filepath.Join(primary, "sessions"): {filepath.Join(alias, "sessions")},
	})
	session := filepath.Join(primary, "sessions", "2026", "09", "04",
		"rollout-2026-09-04T10-00-00-019f0000-0000-7000-8000-000000000001.jsonl")

	primaryIndex := filepath.Join(primary, parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(primaryIndex, []byte("{}\n"), 0o600))
	before, latest, ok := codexSidecarSignature(session)
	require.True(t, ok)
	assert.Equal(t, before.sidecarMtime, latest)
	assert.Zero(t, before.sidecarAliases, "no alias index yet")
	assert.NotZero(t, before.sidecarMtime, "primary index is fingerprinted")

	aliasIndex := filepath.Join(alias, parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(aliasIndex, []byte("{}\n"), 0o600))
	withAlias, _, ok := codexSidecarSignature(session)
	require.True(t, ok)
	assert.NotZero(t, withAlias.sidecarAliases)
	assert.Equal(t, before.sidecarMtime, withAlias.sidecarMtime,
		"primary fields do not change when an alias index appears")

	// A title written in the second home changes the signature.
	later := time.Now().Add(2 * time.Second)
	require.NoError(t, os.WriteFile(aliasIndex,
		[]byte(`{"id":"019f0000-0000-7000-8000-000000000001","thread_name":"renamed"}`+"\n"), 0o600))
	require.NoError(t, os.Chtimes(aliasIndex, later, later))
	renamed, latest, ok := codexSidecarSignature(session)
	require.True(t, ok)
	assert.NotEqual(t, withAlias.sidecarAliases, renamed.sidecarAliases)
	assert.Equal(t, withAlias.sidecarMtime, renamed.sidecarMtime)
	// The trusted mtime follows the newest index in any home, matching
	// what the archive stores through CodexEffectiveMtime.
	assert.Equal(t, later.UnixNano(), latest)
	assert.Equal(t, latest, parser.CodexEffectiveMtime(session, 0))
}
