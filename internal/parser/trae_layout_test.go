package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func traeProfileRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "Trae", "User")
}

func writeTraeModularData(t *testing.T, root string, header string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(root), "ModularData", "ai-agent", "database.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(header), 0o644))
}

func TestClassifyTraeLayout(t *testing.T) {
	root := traeProfileRoot(t)
	writeTraeModularData(t, root, "encrypted header")
	tests := []struct {
		name     string
		root     string
		snapshot traeSessionSnapshot
		want     traeLayoutState
	}{
		{name: "supported", root: root, snapshot: traeSessionSnapshot{records: []traeSessionRecord{{SessionID: "one"}}}, want: traeLayoutSupported},
		{name: "valid empty", root: root, snapshot: traeSessionSnapshot{authoritative: true, complete: true}, want: traeLayoutValidEmpty},
		{name: "unsupported", root: root, snapshot: traeSessionSnapshot{complete: false}, want: traeLayoutUnsupported},
		{name: "malformed", root: root, snapshot: traeSessionSnapshot{complete: false, malformed: true}, want: traeLayoutIncomplete},
		{name: "incomplete without evidence", root: filepath.Join(t.TempDir(), "custom", "User"), snapshot: traeSessionSnapshot{complete: false}, want: traeLayoutIncomplete},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, classifyTraeLayout(test.root, test.snapshot))
		})
	}
}

func TestTraeLayoutStateMatrix(t *testing.T) {
	tests := []struct {
		name  string
		root  func(t *testing.T) string
		setup func(t *testing.T, root string)
		check func(t *testing.T, outcome ParseOutcome)
	}{
		{
			name: "supported",
			root: traeProfileRoot,
			setup: func(t *testing.T, root string) {
				writeTraeModularData(t, root, "encrypted header")
				writeTraeDB(t, filepath.Join(root, "globalStorage", traeStateDBName), traeStoreValue(t, []any{
					map[string]any{
						"sessionId": "real",
						"messages":  []any{map[string]any{"role": "user", "content": "legacy"}},
					},
				}), "memento/unrelated-chat-storage")
			},
			check: func(t *testing.T, outcome ParseOutcome) {
				require.Len(t, outcome.Results, 1)
				assert.Equal(t, SkipNone, outcome.SkipReason)
			},
		},
		{
			name: "valid empty",
			root: traeProfileRoot,
			setup: func(t *testing.T, root string) {
				writeTraeModularData(t, root, "encrypted header")
				writeTraeDB(t, filepath.Join(root, "globalStorage", traeStateDBName), traeStoreValue(t, []any{}), "memento/unrelated-chat-storage")
			},
			check: func(t *testing.T, outcome ParseOutcome) {
				assert.Equal(t, SkipNoSession, outcome.SkipReason)
				assert.True(t, outcome.ResultSetComplete)
				assert.True(t, outcome.ForceReplace)
			},
		},
		{
			name: "unsupported",
			root: traeProfileRoot,
			setup: func(t *testing.T, root string) {
				writeTraeModularData(t, root, "encrypted header")
				writeTraeDBWithoutStorageKey(t, filepath.Join(root, "globalStorage", traeStateDBName), "memento/unrelated-chat-storage")
			},
			check: func(t *testing.T, outcome ParseOutcome) {
				assert.Equal(t, SkipUnsupportedSource, outcome.SkipReason)
				assert.False(t, outcome.ResultSetComplete)
				assert.False(t, outcome.ForceReplace)
			},
		},
		{
			name: "malformed",
			root: traeProfileRoot,
			setup: func(t *testing.T, root string) {
				writeTraeModularData(t, root, "encrypted header")
				writeTraeDB(t, filepath.Join(root, "globalStorage", traeStateDBName), traeStoreValue(t, []any{
					map[string]any{
						"sessionId": "unknown-role",
						"messages":  []any{map[string]any{"role": "system", "content": "ignored"}},
					},
				}), "memento/unrelated-chat-storage")
			},
			check: func(t *testing.T, outcome ParseOutcome) {
				assert.Equal(t, SkipNoSession, outcome.SkipReason)
				assert.False(t, outcome.ResultSetComplete)
				assert.False(t, outcome.ForceReplace)
			},
		},
		{
			name: "incomplete without evidence",
			root: func(t *testing.T) string { return filepath.Join(t.TempDir(), "custom", "User") },
			setup: func(t *testing.T, root string) {
				writeTraeDBWithoutStorageKey(t, filepath.Join(root, "globalStorage", traeStateDBName), "memento/unrelated-chat-storage")
			},
			check: func(t *testing.T, outcome ParseOutcome) {
				assert.Equal(t, SkipNoSession, outcome.SkipReason)
				assert.False(t, outcome.ResultSetComplete)
				assert.False(t, outcome.ForceReplace)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := test.root(t)
			test.setup(t, root)
			factory, ok := ProviderFactoryByType(AgentTrae)
			require.True(t, ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			test.check(t, outcome)
		})
	}
}

func TestTraeLayoutNegativeSpace(t *testing.T) {
	root := traeProfileRoot(t)
	writeTraeModularData(t, root, string(sqliteHeaderMagic))
	assert.False(t, traeEncryptedModularData(root))
	assert.False(t, traeEncryptedModularData(filepath.Join(t.TempDir(), "custom", "User")))

	root = filepath.Join(t.TempDir(), "Trae", "User")
	writeTraeModularData(t, root, "encrypted header")
	assert.Equal(t, traeLayoutValidEmpty, classifyTraeLayout(root, traeSessionSnapshot{authoritative: true, complete: true}))
	assert.Equal(t, traeLayoutSupported, classifyTraeLayout(root, traeSessionSnapshot{records: []traeSessionRecord{{SessionID: "real"}}}))

	unsupportedRoot := traeProfileRoot(t)
	writeTraeModularData(t, unsupportedRoot, "encrypted header")
	unsupportedPath := filepath.Join(unsupportedRoot, "globalStorage", traeStateDBName)
	writeTraeDBWithoutStorageKey(t, unsupportedPath, "memento/unrelated-chat-storage")
	assert.True(t, TraeEncryptedLayoutDetected(unsupportedRoot))

	validEmptyRoot := traeProfileRoot(t)
	writeTraeModularData(t, validEmptyRoot, "encrypted header")
	workspacePath := filepath.Join(validEmptyRoot, "workspaceStorage", "one", traeStateDBName)
	writeTraeDBWithoutStorageKey(t, workspacePath, "memento/unrelated-chat-storage")
	globalPath := filepath.Join(validEmptyRoot, "globalStorage", traeStateDBName)
	writeTraeDB(t, globalPath, traeStoreValue(t, []any{}), "memento/unrelated-chat-storage")
	assert.True(t, TraeEncryptedLayoutDetected(validEmptyRoot))

	errorRoot := traeProfileRoot(t)
	writeTraeModularData(t, errorRoot, "encrypted header")
	errorGlobal := filepath.Join(errorRoot, "globalStorage", traeStateDBName)
	writeTraeDBWithoutStorageKey(t, errorGlobal, "memento/unrelated-chat-storage")
	errorWorkspace := filepath.Join(errorRoot, "workspaceStorage", "one", traeStateDBName)
	writeTraeDBWithoutStorageKey(t, errorWorkspace, "memento/unrelated-chat-storage")
	orig := traeLoadSessionSnapshot
	defer func() { traeLoadSessionSnapshot = orig }()
	traeLoadSessionSnapshot = func(path string) (traeSessionSnapshot, error) {
		if path == errorWorkspace {
			return traeSessionSnapshot{}, os.ErrPermission
		}
		return orig(path)
	}
	assert.True(t, TraeEncryptedLayoutDetected(errorRoot))
}
