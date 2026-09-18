package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceSetProviderPreservesSQLiteEventRelevance(t *testing.T) {
	root := t.TempDir()
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	for _, tc := range []struct {
		path string
		want ChangedPathRelevance
	}{
		{"opencode.db-shm", ChangedPathNonData},
		{"opencode.db-wal", ChangedPathNonData},
		{"opencode.db", ChangedPathDataBearing},
		{"opencode.db-backup", ChangedPathUnclassified},
	} {
		t.Run(tc.path, func(t *testing.T) {
			relevance, err := ResolveChangedPathRelevance(t.Context(), provider, ChangedPathRequest{
				Path: filepath.Join(root, tc.path), WatchRoot: root,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, relevance)
		})
	}
}

func TestSourceSetProviderPreservesSQLiteDiscoveryState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath, seeder, database := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	t.Cleanup(func() { require.NoError(database.Close()) })
	seeder.AddProject("project", "/workspace/project")
	seeder.AddSession("session", "project", "", "SQLite session", 1700000000000, 1700000010000)
	seeder.AddMessage("message", "session", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("part", "message", "session", 1700000000000, 1700000000000,
		`{"type":"text","text":"SQLite message"}`)
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	listed, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	require.NotEmpty(listed.Hash)
	stateProvider, ok := provider.(ReconciliationSourceStateProvider)
	require.True(ok, "the shared provider must preserve discovery state across the spool")
	state, ok := stateProvider.ReconciliationSourceState(t.Context(), sources[0])
	require.True(ok)
	resolver, ok := provider.(ReconciliationSourceStateResolver)
	require.True(ok)
	before := OpenCodeSessionChildLookups()
	source, found, err := resolver.SourceForReconciliationWithState(t.Context(), dbPath+"#session", "", state)
	require.NoError(err)
	require.True(found)
	require.NoError(stateProvider.ApplyReconciliationSourceState(t.Context(), &source, state))
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(listed, fingerprint)
	assert.Equal(before, OpenCodeSessionChildLookups(), "rehydration must not reread the child tables")

	// A JSON session appearing after discovery becomes canonical. SQLite
	// metadata from the spool must not be attached to that replacement.
	storagePath := writeOpenCodeProviderStorageSession(t, root, "session", "session", "project", "JSON session")
	source, found, err = resolver.SourceForReconciliationWithState(t.Context(), dbPath+"#session", "", state)
	require.NoError(err)
	require.True(found)
	require.NoError(stateProvider.ApplyReconciliationSourceState(t.Context(), &source, state))
	assert.Equal(storagePath, source.DisplayPath)
	_, carried := stateProvider.ReconciliationSourceState(t.Context(), source)
	assert.False(carried, "SQLite discovery state must not survive selection of a JSON source")
	parsed, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	require.Len(parsed.Results, 1)
	assert.Equal(storagePath, parsed.Results[0].Result.Session.File.Path)
	require.Len(parsed.Results[0].Result.Messages, 1)
	assert.Equal("Hello from storage", parsed.Results[0].Result.Messages[0].Content)
	assert.False(parsed.ForceReplace)
}

func TestSourceSetProviderReconcilesWithoutDiscoveryState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := shelleyProviderReadFixture(t)
	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{fixture.Root}})
	require.True(ok)
	path := ShelleyVirtualPath(fixture.DBPath, "cMAIN1")
	source, found, err := provider.(ReconciliationSourceStateResolver).
		SourceForReconciliationWithState(t.Context(), path, "", ReconciliationSourceState{})
	require.NoError(err)
	require.True(found)
	assert.Equal(path, source.DisplayPath)
	stateProvider := provider.(ReconciliationSourceStateProvider)
	_, carried := stateProvider.ReconciliationSourceState(t.Context(), source)
	assert.False(carried)
	require.NoError(stateProvider.ApplyReconciliationSourceState(t.Context(), &source, ReconciliationSourceState{}))
	var unsupported UnsupportedProviderFeatureError
	assert.ErrorAs(stateProvider.ApplyReconciliationSourceState(t.Context(), &source, ReconciliationSourceState{Version: 1}), &unsupported)
	parsed, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	require.Len(parsed.Results, 1)
	assert.Equal("shelley:cMAIN1", parsed.Results[0].Result.Session.ID)
}
