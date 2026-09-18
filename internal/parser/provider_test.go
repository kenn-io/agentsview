package parser

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderConfigCloneCopiesRoots(t *testing.T) {
	assert := assert.New(t)

	cfg := ProviderConfig{
		Roots:   []string{"one", "two"},
		Machine: "devbox",
	}

	clone := cfg.Clone()
	rootsCopy := cfg.RootsCopy()
	cfg.Roots[0] = "mutated"
	clone.Roots[1] = "clone-mutated"
	rootsCopy[1] = "copy-mutated"

	assert.Equal([]string{"one", "clone-mutated"}, clone.Roots)
	assert.Equal([]string{"one", "copy-mutated"}, rootsCopy)
	assert.Equal([]string{"mutated", "two"}, cfg.Roots)
	assert.Equal("devbox", clone.Machine)
}

func TestProviderBaseZeroValueOptionalMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	var base ProviderBase

	discovered, err := base.Discover(ctx)
	require.NoError(err)
	assert.Empty(discovered)

	plan, err := base.WatchPlan(ctx)
	require.NoError(err)
	assert.Empty(plan.Roots)

	changed, err := base.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path:      "/tmp/session.jsonl",
		EventKind: "write",
		WatchRoot: "/tmp",
	})
	require.NoError(err)
	assert.Empty(changed)

	source, found, err := base.FindSource(ctx, FindSourceRequest{
		RawSessionID:       "raw",
		FullSessionID:      "agent:raw",
		StoredFilePath:     "/tmp/session.jsonl",
		FingerprintKey:     "/tmp/session.jsonl",
		RequireFreshSource: true,
	})
	require.NoError(err)
	assert.False(found)
	assert.Empty(source)

	fingerprint, err := base.Fingerprint(ctx, SourceRef{
		Provider: AgentCodex,
		Key:      "source",
	})
	require.Error(err)
	assert.Empty(fingerprint)
	assert.ErrorIs(err, ErrUnsupportedProviderFeature)
	var unsupported UnsupportedProviderFeatureError
	require.ErrorAs(err, &unsupported)
	assert.Equal(AgentType(""), unsupported.Provider)
	assert.Equal(ProviderFeatureFingerprint, unsupported.Feature)

	incremental, status, err := base.ParseIncremental(ctx, IncrementalRequest{
		Source:       SourceRef{Provider: AgentCodex, Key: "source"},
		Fingerprint:  SourceFingerprint{Key: "source"},
		SessionID:    "codex:session",
		Offset:       1024,
		StartOrdinal: 7,
		Machine:      "devbox",
	})
	require.NoError(err)
	assert.Equal(IncrementalUnsupported, status)
	assert.Empty(incremental)

	_, ok := any(base).(Provider)
	assert.False(ok, "ProviderBase must not satisfy Provider without Parse")
}

func TestUnsupportedProviderFeatureErrorWrapsSentinel(t *testing.T) {
	err := UnsupportedProviderFeatureError{
		Provider: AgentCodex,
		Feature:  ProviderFeatureFingerprint,
	}

	assert.ErrorIs(t, err, ErrUnsupportedProviderFeature)
	assert.Contains(t, err.Error(), string(AgentCodex))
	assert.Contains(t, err.Error(), ProviderFeatureFingerprint)
}

func TestCapabilitySupportTextAndJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	assert.Equal("unsupported", CapabilityUnsupported.String())
	assert.Equal("supported", CapabilitySupported.String())
	assert.Equal("not_applicable", CapabilityNotApplicable.String())

	marshaled, err := json.Marshal(CapabilitySupported)
	require.NoError(err)
	assert.JSONEq(`"supported"`, string(marshaled))

	var decoded CapabilitySupport
	require.NoError(json.Unmarshal([]byte(`"not_applicable"`), &decoded))
	assert.Equal(CapabilityNotApplicable, decoded)

	text, err := CapabilitySupported.MarshalText()
	require.NoError(err)
	assert.Equal("supported", string(text))

	require.NoError(decoded.UnmarshalText([]byte("unsupported")))
	assert.Equal(CapabilityUnsupported, decoded)
	assert.Error(decoded.UnmarshalText([]byte("bogus")))
}

func TestProviderRegistryMirrorsAgentRegistry(t *testing.T) {
	require := require.New(t)

	factories := ProviderFactories()
	require.Len(factories, len(Registry))

	seen := make(map[AgentType]bool, len(factories))
	for _, factory := range factories {
		def := factory.Definition()
		require.Falsef(seen[def.Type], "duplicate provider factory for %s", def.Type)
		seen[def.Type] = true

		registryDef, ok := AgentByType(def.Type)
		require.Truef(ok, "provider factory for unknown agent %s", def.Type)
		assertAgentDefMetadataEqual(t, registryDef, def)

		provider := factory.NewProvider(ProviderConfig{
			Roots:   []string{"/tmp/root"},
			Machine: "devbox",
		})
		require.NotNil(provider)
		assertAgentDefMetadataEqual(t, def, provider.Definition())
	}

	for _, def := range Registry {
		assert.Truef(t, seen[def.Type], "missing provider factory for %s", def.Type)
	}
}

func TestStoredSourceHintCapabilitiesMatchConsumers(t *testing.T) {
	assert := assert.New(t)

	wantSupported := map[AgentType]bool{
		AgentCursorIDE: true,
		AgentDevin:     true,
		AgentForge:     true,
		AgentKiro:      true,
		AgentPiebald:   true,
		AgentShelley:   true,
		AgentTrae:      true,
		AgentVSCopilot: true,
		AgentWarp:      true,
		AgentWindsurf:  true,
		AgentZCode:     true,
		AgentZed:       true,
		AgentCline:     true,
	}

	for _, factory := range ProviderFactories() {
		agent := factory.Definition().Type
		got := factory.Capabilities().Source.StoredSourceHints
		if wantSupported[agent] {
			assert.Equalf(CapabilitySupported, got, "%s consumes stored path hints", agent)
			provider := factory.NewProvider(ProviderConfig{})
			assert.Equalf(CapabilitySupported,
				provider.Capabilities().Source.StoredSourceHints,
				"%s configured provider consumes stored path hints", agent)
			assert.Implementsf((*StoredSourceHintScopeProvider)(nil), provider,
				"%s must scope stored hints before the engine queries them", agent)
		} else {
			assert.Equalf(CapabilityUnsupported, got, "%s must not schedule stored path hints", agent)
		}
	}
}

func TestStoredSourceHintScopesDistinguishContainersFromExactMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	forge, ok := NewProvider(AgentForge, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	forgeScopes := forge.(StoredSourceHintScopeProvider)
	dbPath := filepath.Join(root, ForgeDBFilename)
	assert.Equal([]StoredSourceHintScope{{
		Path: dbPath, IncludeVirtualMembers: true,
	}}, forgeScopes.StoredSourceHintScopes(ChangedPathRequest{
		Path: dbPath + "-wal", WatchRoot: root,
	}))
	virtualPath := VirtualSourcePath(dbPath, "conversation-a")
	assert.Equal([]StoredSourceHintScope{{Path: virtualPath}},
		forgeScopes.StoredSourceHintScopes(ChangedPathRequest{
			Path: virtualPath, WatchRoot: root,
		}))

	visualStudio, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	visualStudioScopes := visualStudio.(StoredSourceHintScopeProvider)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	container := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	assert.Equal([]StoredSourceHintScope{{
		Path: container, IncludeVirtualMembers: true,
	}}, visualStudioScopes.StoredSourceHintScopes(ChangedPathRequest{Path: container}))
	member := VisualStudioCopilotVirtualPath(container, conversationID)
	assert.Equal([]StoredSourceHintScope{{Path: member}},
		visualStudioScopes.StoredSourceHintScopes(ChangedPathRequest{Path: member}))
}

func TestVerifiedLocalStatCapabilitiesMatchConsumers(t *testing.T) {
	assert.Equal(t, CapabilityUnsupported,
		(SourceCapabilities{}).VerifiedLocalStat,
		"new providers must opt in explicitly")

	wantSupported := map[AgentType]bool{
		AgentClaude: true,
		AgentCodex:  true,
		// TraeX and Augure Code share the Codex provider; the gate stats the
		// transcript and only looks for a session_index.jsonl sidecar under
		// Codex itself.
		AgentTraeX:      true,
		AgentAugureCode: true,
	}
	for _, factory := range ProviderFactories() {
		agent := factory.Definition().Type
		got := factory.Capabilities().Source.VerifiedLocalStat
		if wantSupported[agent] {
			assert.Equalf(t, CapabilitySupported, got,
				"%s supports verified local stat trust", agent)
		} else {
			assert.Equalf(t, CapabilityUnsupported, got,
				"%s must not schedule verified local stat trust", agent)
		}
	}
}

func TestProviderFactoryLookupRejectsMissingAgent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	require.NotEmpty(Registry)
	agent := Registry[0].Type

	factory, ok := ProviderFactoryByType(agent)
	require.True(ok)
	assert.Equal(agent, factory.Definition().Type)

	provider, ok := NewProvider(agent, ProviderConfig{
		Roots:   []string{"/tmp/one", "/tmp/two"},
		Machine: "devbox",
	})
	require.True(ok)
	require.NotNil(provider)

	_, ok = ProviderFactoryByType("missing")
	assert.False(ok)
	_, ok = NewProvider("missing", ProviderConfig{})
	assert.False(ok)
}

func TestProviderFactoryByTypeDevin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	factory, ok := ProviderFactoryByType(AgentDevin)
	require.True(ok)
	assert.Equal(AgentDevin, factory.Definition().Type)

	provider := factory.NewProvider(ProviderConfig{
		Roots:   []string{"/tmp/devin"},
		Machine: "devbox",
	})
	require.NotNil(provider)
	assert.Equal(AgentDevin, provider.Definition().Type)
}

func TestProviderMigrationModesCoverRegistry(t *testing.T) {
	err := ValidateProviderMigrationModes(
		ProviderFactories(),
		ProviderMigrationModes(),
	)
	require.NoError(t, err)
}

func TestProviderMigrationModesRestrictImportOnlyMode(t *testing.T) {
	factory := testProviderFactory{
		def: AgentDef{
			Type:        AgentCodex,
			DisplayName: "Codex",
		},
	}
	modes := map[AgentType]ProviderMigrationMode{
		AgentCodex: ProviderMigrationImportOnly,
	}

	err := ValidateProviderMigrationModes([]ProviderFactory{factory}, modes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(AgentCodex))
	assert.Contains(t, err.Error(), string(ProviderMigrationImportOnly))
}

type testProviderFactory struct {
	def  AgentDef
	caps Capabilities
}

func (f testProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f testProviderFactory) Capabilities() Capabilities {
	return f.caps
}

func (f testProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &testProvider{
		Def:    cloneAgentDef(f.def),
		Caps:   f.caps,
		Config: cfg.Clone(),
	}
}

type testProvider struct {
	ProviderBase
}

func (p *testProvider) Parse(context.Context, ParseRequest) (ParseOutcome, error) {
	return ParseOutcome{}, nil
}

func assertAgentDefMetadataEqual(t *testing.T, want, got AgentDef) {
	t.Helper()

	assert.Equal(t, want.Type, got.Type)
	assert.Equal(t, want.DisplayName, got.DisplayName)
	assert.Equal(t, want.EnvVar, got.EnvVar)
	assert.Equal(t, want.ConfigKey, got.ConfigKey)
	assert.Equal(t, want.DefaultDirs, got.DefaultDirs)
	assert.Equal(t, want.IDPrefix, got.IDPrefix)
	assert.Equal(t, want.WatchSubdirs, got.WatchSubdirs)
	assert.Equal(t, want.ShallowWatch, got.ShallowWatch)
	assert.Equal(t, want.FileBased, got.FileBased)
	assert.Equal(t, want.WatchRootsFunc == nil, got.WatchRootsFunc == nil)
	assert.Equal(t, want.ShallowWatchRootsFunc == nil, got.ShallowWatchRootsFunc == nil)
}
