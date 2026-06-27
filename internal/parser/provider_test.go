package parser

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderConfigCloneCopiesRoots(t *testing.T) {
	cfg := ProviderConfig{
		Roots:   []string{"one", "two"},
		Machine: "devbox",
	}

	clone := cfg.Clone()
	rootsCopy := cfg.RootsCopy()
	cfg.Roots[0] = "mutated"
	clone.Roots[1] = "clone-mutated"
	rootsCopy[1] = "copy-mutated"

	assert.Equal(t, []string{"one", "clone-mutated"}, clone.Roots)
	assert.Equal(t, []string{"one", "copy-mutated"}, rootsCopy)
	assert.Equal(t, []string{"mutated", "two"}, cfg.Roots)
	assert.Equal(t, "devbox", clone.Machine)
}

func TestProviderBaseZeroValueOptionalMethods(t *testing.T) {
	ctx := context.Background()
	var base ProviderBase

	discovered, err := base.Discover(ctx)
	require.NoError(t, err)
	assert.Empty(t, discovered)

	plan, err := base.WatchPlan(ctx)
	require.NoError(t, err)
	assert.Empty(t, plan.Roots)

	changed, err := base.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path:      "/tmp/session.jsonl",
		EventKind: "write",
		WatchRoot: "/tmp",
	})
	require.NoError(t, err)
	assert.Empty(t, changed)

	source, found, err := base.FindSource(ctx, FindSourceRequest{
		RawSessionID:       "raw",
		FullSessionID:      "agent:raw",
		StoredFilePath:     "/tmp/session.jsonl",
		FingerprintKey:     "/tmp/session.jsonl",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, source)

	fingerprint, err := base.Fingerprint(ctx, SourceRef{
		Provider: AgentCodex,
		Key:      "source",
	})
	require.Error(t, err)
	assert.Empty(t, fingerprint)
	assert.True(t, errors.Is(err, ErrUnsupportedProviderFeature))
	var unsupported UnsupportedProviderFeatureError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, AgentType(""), unsupported.Provider)
	assert.Equal(t, ProviderFeatureFingerprint, unsupported.Feature)

	incremental, status, err := base.ParseIncremental(ctx, IncrementalRequest{
		Source:       SourceRef{Provider: AgentCodex, Key: "source"},
		Fingerprint:  SourceFingerprint{Key: "source"},
		SessionID:    "codex:session",
		Offset:       1024,
		StartOrdinal: 7,
		Machine:      "devbox",
	})
	require.NoError(t, err)
	assert.Equal(t, IncrementalUnsupported, status)
	assert.Empty(t, incremental)

	_, ok := any(base).(Provider)
	assert.False(t, ok, "ProviderBase must not satisfy Provider without Parse")
}

func TestUnsupportedProviderFeatureErrorWrapsSentinel(t *testing.T) {
	err := UnsupportedProviderFeatureError{
		Provider: AgentCodex,
		Feature:  ProviderFeatureFingerprint,
	}

	assert.True(t, errors.Is(err, ErrUnsupportedProviderFeature))
	assert.Contains(t, err.Error(), string(AgentCodex))
	assert.Contains(t, err.Error(), ProviderFeatureFingerprint)
}

func TestCapabilitySupportTextAndJSON(t *testing.T) {
	assert.Equal(t, "unsupported", CapabilityUnsupported.String())
	assert.Equal(t, "supported", CapabilitySupported.String())
	assert.Equal(t, "not_applicable", CapabilityNotApplicable.String())

	marshaled, err := json.Marshal(CapabilitySupported)
	require.NoError(t, err)
	assert.JSONEq(t, `"supported"`, string(marshaled))

	var decoded CapabilitySupport
	require.NoError(t, json.Unmarshal([]byte(`"not_applicable"`), &decoded))
	assert.Equal(t, CapabilityNotApplicable, decoded)

	text, err := CapabilitySupported.MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "supported", string(text))

	require.NoError(t, decoded.UnmarshalText([]byte("unsupported")))
	assert.Equal(t, CapabilityUnsupported, decoded)
	assert.Error(t, decoded.UnmarshalText([]byte("bogus")))
}

func TestProviderRegistryMirrorsAgentRegistry(t *testing.T) {
	factories := ProviderFactories()
	require.Len(t, factories, len(Registry))

	seen := make(map[AgentType]bool, len(factories))
	for _, factory := range factories {
		def := factory.Definition()
		require.Falsef(t, seen[def.Type], "duplicate provider factory for %s", def.Type)
		seen[def.Type] = true

		registryDef, ok := AgentByType(def.Type)
		require.Truef(t, ok, "provider factory for unknown agent %s", def.Type)
		assertAgentDefMetadataEqual(t, registryDef, def)

		provider := factory.NewProvider(ProviderConfig{
			Roots:   []string{"/tmp/root"},
			Machine: "devbox",
		})
		require.NotNil(t, provider)
		assertAgentDefMetadataEqual(t, def, provider.Definition())
	}

	for _, def := range Registry {
		assert.Truef(t, seen[def.Type], "missing provider factory for %s", def.Type)
	}
}

func TestLegacyProviderCapabilitiesMatchBaseDefaults(t *testing.T) {
	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)

	assert.Equal(t, Capabilities{}, provider.Capabilities())

	ctx := context.Background()
	discovered, err := provider.Discover(ctx)
	require.NoError(t, err)
	assert.Empty(t, discovered)

	plan, err := provider.WatchPlan(ctx)
	require.NoError(t, err)
	assert.Empty(t, plan.Roots)

	changed, err := provider.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path:      "/tmp/session.jsonl",
		EventKind: "write",
		WatchRoot: "/tmp",
	})
	require.NoError(t, err)
	assert.Empty(t, changed)

	source, found, err := provider.FindSource(ctx, FindSourceRequest{
		RawSessionID:   "session",
		FullSessionID:  "codex:session",
		StoredFilePath: "/tmp/session.jsonl",
		FingerprintKey: "/tmp/session.jsonl",
	})
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, source)

	_, err = provider.Fingerprint(ctx, SourceRef{
		Provider:       AgentCodex,
		Key:            "session",
		DisplayPath:    "/tmp/session.jsonl",
		FingerprintKey: "/tmp/session.jsonl",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedProviderFeature))

	incremental, status, err := provider.ParseIncremental(ctx, IncrementalRequest{
		Source:       SourceRef{Provider: AgentCodex, Key: "session"},
		Fingerprint:  SourceFingerprint{Key: "/tmp/session.jsonl"},
		SessionID:    "codex:session",
		StartOrdinal: 1,
		Machine:      "devbox",
	})
	require.NoError(t, err)
	assert.Equal(t, IncrementalUnsupported, status)
	assert.Empty(t, incremental)
}

func TestProviderFactoryLookupAndConfigSnapshot(t *testing.T) {
	cfg := ProviderConfig{
		Roots:   []string{"/tmp/one", "/tmp/two"},
		Machine: "devbox",
	}

	factory, ok := ProviderFactoryByType(AgentCodex)
	require.True(t, ok)
	assert.Equal(t, AgentCodex, factory.Definition().Type)

	provider, ok := NewProvider(AgentCodex, cfg)
	require.True(t, ok)
	require.NotNil(t, provider)

	cfg.Roots[0] = "/tmp/mutated"
	legacy, ok := provider.(*legacyProvider)
	require.True(t, ok)
	assert.Equal(t, []string{"/tmp/one", "/tmp/two"}, legacy.Config.Roots)
	assert.Equal(t, "devbox", legacy.Config.Machine)

	_, ok = ProviderFactoryByType("missing")
	assert.False(t, ok)
	_, ok = NewProvider("missing", cfg)
	assert.False(t, ok)
}

func TestLegacyProviderParseReturnsUnsupported(t *testing.T) {
	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: SourceRef{
			Provider:       AgentCodex,
			Key:            "source",
			DisplayPath:    "/tmp/source.jsonl",
			FingerprintKey: "/tmp/source.jsonl",
		},
		Fingerprint: SourceFingerprint{
			Key:     "/tmp/source.jsonl",
			MTimeNS: time.Now().UnixNano(),
		},
		Machine: "devbox",
	})
	require.Error(t, err)
	assert.Empty(t, outcome)
	assert.True(t, errors.Is(err, ErrUnsupportedProviderFeature))
	var unsupported UnsupportedProviderFeatureError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, AgentCodex, unsupported.Provider)
	assert.Equal(t, ProviderFeatureParse, unsupported.Feature)
}

func TestProviderMigrationModesCoverRegistry(t *testing.T) {
	err := ValidateProviderMigrationModes(
		ProviderFactories(),
		ProviderMigrationModes(),
	)
	require.NoError(t, err)
}

func TestProviderMigrationModesRejectConcreteProviderLeftLegacyOnly(t *testing.T) {
	factory := testProviderFactory{
		def: AgentDef{
			Type:        AgentCodex,
			DisplayName: "Codex",
		},
	}
	modes := map[AgentType]ProviderMigrationMode{
		AgentCodex: ProviderMigrationLegacyOnly,
	}

	err := ValidateProviderMigrationModes([]ProviderFactory{factory}, modes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(AgentCodex))
	assert.Contains(t, err.Error(), string(ProviderMigrationShadowCompare))
}

func TestProviderMigrationModesRejectConcreteModeForLegacyFactory(t *testing.T) {
	factory := legacyProviderFactory{
		def: AgentDef{
			Type:        AgentCodex,
			DisplayName: "Codex",
		},
	}
	modes := map[AgentType]ProviderMigrationMode{
		AgentCodex: ProviderMigrationShadowCompare,
	}

	err := ValidateProviderMigrationModes([]ProviderFactory{factory}, modes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(AgentCodex))
	assert.Contains(t, err.Error(), string(ProviderMigrationLegacyOnly))
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
	def AgentDef
}

func (f testProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f testProviderFactory) Capabilities() Capabilities {
	return Capabilities{}
}

func (f testProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &testProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Config: cfg.Clone(),
		},
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
	assert.Equal(t, want.DiscoverFunc == nil, got.DiscoverFunc == nil)
	assert.Equal(t, want.FindSourceFunc == nil, got.FindSourceFunc == nil)
	assert.Equal(t, want.WatchRootsFunc == nil, got.WatchRootsFunc == nil)
	assert.Equal(t, want.ShallowWatchRootsFunc == nil, got.ShallowWatchRootsFunc == nil)
}
