package parser

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderCapabilitiesWatchRootsDefaultUnsupported(t *testing.T) {
	assert.Equal(t, CapabilityUnsupported, (SourceCapabilities{}).WatchRoots)
}

func TestProviderCapabilitiesStreamingDiscoveryDefaultUnsupported(t *testing.T) {
	assert.Equal(t, CapabilityUnsupported, (SourceCapabilities{}).StreamingDiscovery)
}

func TestProviderCapabilitiesPersistentArchiveDefaultUnsupported(t *testing.T) {
	assert.Equal(t, CapabilityUnsupported, (SourceCapabilities{}).PersistentArchive)
}

func TestWatchSourceProvidersDiscoverEachDirectly(t *testing.T) {
	for _, factory := range ProviderFactories() {
		t.Run(string(factory.Definition().Type), func(t *testing.T) {
			caps := factory.Capabilities().Source
			if caps.WatchSources != CapabilitySupported ||
				caps.DiscoverSources != CapabilitySupported {
				t.Skip("provider does not participate in watched discovery")
			}
			assert.Equal(t, CapabilitySupported, caps.StreamingDiscovery)
			assert.Equal(t, CapabilitySupported, caps.WatchSources)
			assert.Equal(t, CapabilitySupported, caps.DiscoverSources)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{t.TempDir()}})
			_, ok := provider.(StreamingDiscoverer)
			assert.True(t, ok)
			if caps.SharedContainerSource == CapabilitySupported {
				_, exact := provider.(ReconciliationSourceResolver)
				assert.True(t, exact,
					"every shared-container streaming provider must advertise exact rehydration")
			}
			if ok {
				require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(
					t.Context(), func(SourceRef) error { return nil },
				))
			}
		})
	}
}

func TestSourceSetProviderDiscoverEachDoesNotCallCollectingDiscover(t *testing.T) {
	sources := &streamingSourceSetTestDouble{}
	provider := &SourceSetProvider{
		ProviderBase: ProviderBase{Def: AgentDef{Type: "stream-test"}},
		sources:      sources,
	}

	var got []SourceRef
	err := provider.DiscoverEach(t.Context(), func(source SourceRef) error {
		got = append(got, source)
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, []SourceRef{{Provider: "stream-test", Key: "one"}}, got)
	assert.Zero(t, sources.discoverCalls)
}

func TestSourceSetFactoryDowngradesStreamingForCollectingSourceSet(t *testing.T) {
	factory := NewSourceSetFactory(
		AgentDef{Type: "non-streaming-source-set"},
		Capabilities{},
		func(ProviderConfig) SourceSet { return nonStreamingSourceSetTestDouble{} },
	)

	assert.Equal(t, CapabilitySupported,
		factory.Capabilities().Source.StreamingDiscovery)
	assert.Equal(t, CapabilityUnsupported,
		factory.NewProvider(ProviderConfig{}).Capabilities().Source.StreamingDiscovery)
}

func TestProviderMigrationRejectsSourceSetWrapperWithoutUnderlyingStreaming(t *testing.T) {
	factory := NewSourceSetFactory(
		AgentDef{Type: "invalid-source-set-streaming"},
		Capabilities{Source: SourceCapabilities{
			DiscoverSources: CapabilitySupported,
			FindSource:      CapabilitySupported,
		}},
		func(ProviderConfig) SourceSet { return nonStreamingSourceSetTestDouble{} },
	)

	err := ValidateProviderMigrationModes(
		[]ProviderFactory{factory},
		map[AgentType]ProviderMigrationMode{
			"invalid-source-set-streaming": ProviderMigrationProviderAuthoritative,
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "underlying source set")
}

func TestProviderMigrationRejectsStreamingCapabilityWithoutInterface(t *testing.T) {
	factory := testProviderFactory{
		def: AgentDef{Type: "invalid-streaming-provider"},
		caps: Capabilities{Source: SourceCapabilities{
			DiscoverSources:    CapabilitySupported,
			FindSource:         CapabilitySupported,
			StreamingDiscovery: CapabilitySupported,
		}},
	}

	err := ValidateProviderMigrationModes(
		[]ProviderFactory{factory},
		map[AgentType]ProviderMigrationMode{
			"invalid-streaming-provider": ProviderMigrationProviderAuthoritative,
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "StreamingDiscoverer")
}

func TestProviderMigrationRejectsSharedContainerWithoutExactRehydration(t *testing.T) {
	factory := streamingWithoutExactFactory{testProviderFactory{
		def: AgentDef{Type: "invalid-shared-container-provider"}, caps: Capabilities{Source: SourceCapabilities{
			DiscoverSources:       CapabilitySupported,
			FindSource:            CapabilitySupported,
			StreamingDiscovery:    CapabilitySupported,
			SharedContainerSource: CapabilitySupported,
		}},
	}}

	err := ValidateProviderMigrationModes(
		[]ProviderFactory{factory},
		map[AgentType]ProviderMigrationMode{
			"invalid-shared-container-provider": ProviderMigrationProviderAuthoritative,
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exact reconciliation rehydration")
}

func TestSourceSetFactoryDowngradesAndRejectsMissingExactRehydration(t *testing.T) {
	factory := NewSourceSetFactory(
		AgentDef{Type: "invalid-shared-source-set"},
		Capabilities{Source: SourceCapabilities{
			DiscoverSources:       CapabilitySupported,
			FindSource:            CapabilitySupported,
			SharedContainerSource: CapabilitySupported,
		}},
		func(ProviderConfig) SourceSet { return &streamingSourceSetTestDouble{} },
	)
	provider := factory.NewProvider(ProviderConfig{})

	assert.Equal(t, CapabilityUnsupported,
		provider.Capabilities().Source.SharedContainerSource)
	err := ValidateProviderMigrationModes(
		[]ProviderFactory{factory},
		map[AgentType]ProviderMigrationMode{
			"invalid-shared-source-set": ProviderMigrationProviderAuthoritative,
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "underlying source set")
	assert.Contains(t, err.Error(), "exact reconciliation rehydration")
}

func TestProviderCapabilitiesRequireWatchRootPlanner(t *testing.T) {
	provider := &watchRootCapabilityTestProvider{
		ProviderBase: ProviderBase{
			Def: AgentDef{Type: "watch-root-test"},
			Caps: Capabilities{Source: SourceCapabilities{
				WatchRoots: CapabilitySupported,
			}},
		},
		plan: WatchPlan{Roots: []WatchRoot{{Path: "/fallback"}}},
	}

	_, err := ResolveWatchRoots(context.Background(), provider)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedProviderFeature)
	assert.Zero(t, provider.watchPlanCalls,
		"a false capability advertisement must not silently take the legacy path")
}

func TestProviderCapabilitiesFallbackWatchPlanRetainsOnlyRootMetadata(t *testing.T) {
	provider := &watchRootCapabilityTestProvider{
		ProviderBase: ProviderBase{Def: AgentDef{Type: "legacy-watch-root-test"}},
		plan: WatchPlan{Roots: []WatchRoot{{
			Path:         "/sessions",
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl"},
			ExcludeGlobs: []string{"*.tmp"},
			DebounceKey:  "legacy:sessions",
		}}},
	}

	roots, err := ResolveWatchRoots(context.Background(), provider)
	require.NoError(t, err)
	assert.Equal(t, []WatchRoot{{
		Path:        "/sessions",
		Recursive:   true,
		DebounceKey: "legacy:sessions",
	}}, roots)
	assert.Equal(t, 1, provider.watchPlanCalls)
}

func TestProviderCapabilitiesSourceSetAdapterImplementsWatchRootPlanner(t *testing.T) {
	root := filepath.Clean("/sessions")
	factory := NewSourceSetFactory(
		AgentDef{Type: "source-set-watch-root-test"},
		Capabilities{},
		func(cfg ProviderConfig) SourceSet {
			return NewJSONLSourceSet("source-set-watch-root-test", cfg.Roots)
		},
	)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})

	assert.Equal(t, CapabilitySupported, provider.Capabilities().Source.WatchRoots)
	planner, ok := provider.(WatchRootPlanner)
	require.True(t, ok)
	roots, err := planner.WatchRoots(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []WatchRoot{{
		Path:        root,
		DebounceKey: "source-set-watch-root-test:jsonl:" + root,
	}}, roots)
}

type watchRootCapabilityTestProvider struct {
	ProviderBase
	plan           WatchPlan
	watchPlanCalls int
}

type streamingSourceSetTestDouble struct {
	discoverCalls int
}

type streamingWithoutExactFactory struct{ testProviderFactory }

func (factory streamingWithoutExactFactory) NewProvider(cfg ProviderConfig) Provider {
	return &streamingWithoutExactProvider{testProvider: testProvider{ProviderBase: ProviderBase{
		Def: factory.def, Caps: factory.caps, Config: cfg.Clone(),
	}}}
}

type streamingWithoutExactProvider struct{ testProvider }

func (*streamingWithoutExactProvider) DiscoverEach(
	context.Context, func(SourceRef) error,
) error {
	return nil
}

type nonStreamingSourceSetTestDouble struct{}

func (nonStreamingSourceSetTestDouble) Discover(context.Context) ([]SourceRef, error) {
	return nil, nil
}

func (nonStreamingSourceSetTestDouble) WatchPlan(context.Context) (WatchPlan, error) {
	return WatchPlan{}, nil
}

func (nonStreamingSourceSetTestDouble) SourcesForChangedPath(
	context.Context, ChangedPathRequest,
) ([]SourceRef, error) {
	return nil, nil
}

func (nonStreamingSourceSetTestDouble) FindSource(
	context.Context, FindSourceRequest,
) (SourceRef, bool, error) {
	return SourceRef{}, false, nil
}

func (nonStreamingSourceSetTestDouble) Fingerprint(
	context.Context, SourceRef,
) (SourceFingerprint, error) {
	return SourceFingerprint{}, nil
}

func (nonStreamingSourceSetTestDouble) Parse(
	context.Context, ParseRequest,
) (ParseOutcome, error) {
	return ParseOutcome{}, nil
}

func (s *streamingSourceSetTestDouble) Discover(context.Context) ([]SourceRef, error) {
	s.discoverCalls++
	return nil, errors.New("collecting discovery must not be called")
}

func (s *streamingSourceSetTestDouble) DiscoverEach(
	_ context.Context, yield func(SourceRef) error,
) error {
	return yield(SourceRef{Provider: "stream-test", Key: "one"})
}

func (*streamingSourceSetTestDouble) WatchPlan(context.Context) (WatchPlan, error) {
	return WatchPlan{}, nil
}

func (*streamingSourceSetTestDouble) SourcesForChangedPath(
	context.Context, ChangedPathRequest,
) ([]SourceRef, error) {
	return nil, nil
}

func (*streamingSourceSetTestDouble) FindSource(
	context.Context, FindSourceRequest,
) (SourceRef, bool, error) {
	return SourceRef{}, false, nil
}

func (*streamingSourceSetTestDouble) Fingerprint(
	context.Context, SourceRef,
) (SourceFingerprint, error) {
	return SourceFingerprint{}, nil
}

func (*streamingSourceSetTestDouble) Parse(
	context.Context, ParseRequest,
) (ParseOutcome, error) {
	return ParseOutcome{}, nil
}

func (p *watchRootCapabilityTestProvider) WatchPlan(context.Context) (WatchPlan, error) {
	p.watchPlanCalls++
	return p.plan, nil
}

func (p *watchRootCapabilityTestProvider) Parse(
	context.Context,
	ParseRequest,
) (ParseOutcome, error) {
	return ParseOutcome{}, nil
}
