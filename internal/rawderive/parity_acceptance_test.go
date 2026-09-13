package rawderive

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawtest"
)

func TestParityAcceptanceEveryCaptureProviderHasIndependentOracle(t *testing.T) {
	var supported []parser.AgentType
	for _, factory := range parser.ProviderFactories() {
		if factory.Capabilities().RawCapture.Support == parser.CapabilitySupported {
			supported = append(supported, factory.Definition().Type)
		}
	}
	require.ElementsMatch(t, []parser.AgentType{parser.AgentClaude, parser.AgentCodex, parser.AgentEvener, parser.AgentGoose, parser.AgentForge, parser.AgentPiebald, parser.AgentWarp, parser.AgentZCode}, supported)
	fixtures := rawtest.ParityFixtures()
	var covered []parser.AgentType
	for agent := range fixtures {
		covered = append(covered, agent)
	}
	require.ElementsMatch(t, supported, covered)
	for _, agent := range supported {
		t.Run(string(agent), func(t *testing.T) {
			fixture := fixtures[agent](t, t.TempDir())
			oracle, engine := rawtest.Oracle(t, agent, fixture.Root, config.ArchiveContentFull, "")
			require.Positive(t, engine.SyncAll(t.Context(), nil).Synced)
			fixture.AssertOracle(t, oracle)
		})
	}
}

// The accepted boundary must publish all members/generations; the very next
// element must fail before any partial member list is published.
func TestParityAcceptanceWorkerHistoryAndFanoutBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		history, members int
		complete         bool
	}{{"history1024", 1024, 1, true}, {"history1025", 1025, 1, false}, {"fanout4096", 1, 4096, true}, {"fanout4097", 1, 4097, false}} {
		t.Run(tc.name, func(t *testing.T) {
			first := parityWorkerManifests(t, 1)[0]
			manifests := make([]rawsync.CanonicalManifest, tc.history)
			byID := make(map[string]rawsync.CanonicalManifest, tc.history)
			for i := range manifests {
				input := first.Manifest
				input.CaptureID = fmt.Sprintf("generation-%d", i)
				var err error
				manifests[i], err = rawsync.ValidateAndCanonicalize(first.Identity, input, rawsync.DefaultManifestLimits())
				require.NoError(t, err)
				byID[manifests[i].ManifestID] = manifests[i]
			}
			source := parityWorkerSource(manifests)
			evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
			outcome := parser.ParseOutcome{ResultSetComplete: true}
			for i := 0; i < tc.members; i++ {
				outcome.Results = append(outcome.Results, parser.ParseResultOutcome{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: fmt.Sprintf("member-%04d", i), Agent: parser.AgentClaude}, Messages: []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: "Inspect fixture."}}}})
			}
			binding := parityWorkerBinding()
			reads, parses := 0, 0
			worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence, Binding: binding, AttemptTimeout: 30 * time.Second,
				Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
					reads++
					return byID[id], nil
				}),
				Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
					return &Materialization{}, nil
				}),
				Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
					parses++
					return ParsedManifest{Outcome: outcome}, nil
				}),
			})
			require.NoError(t, err)
			processed, err := worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
			require.NoError(t, err)
			require.Equal(t, 1, processed)
			require.Len(t, evidence.recorded, 1)
			result := evidence.recorded[0]
			assert.Equal(t, tc.complete, result.Complete)
			if tc.complete {
				assert.Len(t, result.Members, tc.members)
				assert.Equal(t, "pending", result.Code)
				assert.Equal(t, tc.history, reads)
				assert.Equal(t, tc.history, parses)
			} else {
				assert.Equal(t, "limit_exceeded", result.Code)
				assert.Empty(t, result.Members)
				if tc.history > 1024 {
					assert.Zero(t, reads)
					assert.Zero(t, parses)
				} else {
					assert.Equal(t, 1, reads)
					assert.Equal(t, 1, parses)
				}
			}
		})
	}
}

type parityAcceptanceEvidence struct {
	*parityEvidenceFixture
	sourceCalls, historyCalls, sourceLimit int
}

func (e *parityAcceptanceEvidence) NextParitySources(_ context.Context, _ ParityLease, _ string, limit int) ([]ParitySource, error) {
	e.sourceCalls++
	e.sourceLimit = limit
	return append([]ParitySource(nil), e.sources[:min(limit, len(e.sources))]...), nil
}
func (e *parityAcceptanceEvidence) NextParityHistory(ctx context.Context, lease ParityLease, id string, after int64, limit int) ([]ParityHistoryEntry, error) {
	e.historyCalls++
	return e.parityEvidenceFixture.NextParityHistory(ctx, lease, id, after, limit)
}

func TestParityAcceptanceWorkerSourceWorkDoesNotScaleWithUnselectedSources(t *testing.T) {
	root := t.TempDir()
	rawtest.Claude(t, root)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}, Machine: "fixture"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	manifest, objects := manifestFromCapturePlan(t, parser.AgentClaude, provider, sources[0])
	require.Len(t, objects, 2)
	source := parityWorkerSource([]rawsync.CanonicalManifest{manifest})
	binding := parityWorkerBinding()
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "fixture")
	require.NoError(t, err)
	var small float64
	for _, size := range []int{10, 10000} {
		corpus := make([]ParitySource, size+1)
		corpus[0] = source
		for i := 1; i < len(corpus); i++ {
			corpus[i] = source
			corpus[i].ID = fmt.Sprintf("unselected-%d", i)
		}
		evidence := &parityAcceptanceEvidence{parityEvidenceFixture: &parityEvidenceFixture{sources: corpus, history: map[string][]ParityHistoryEntry{source.ID: {{ManifestID: manifest.ManifestID, Generation: 1}}}}}
		custody := &materializerStore{objects: objects}
		loads := 0
		worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence, Manifests: exactManifestSourceFunc(func(_ context.Context, identity rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
			require.Equal(t, manifest.Identity, identity)
			require.True(t, manifest.ManifestID == id, "worker requested a different retained manifest")
			loads++
			return manifest, nil
		}), Materializer: Materializer{Store: custody, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}, Parser: dispatch, Binding: binding, AttemptTimeout: time.Minute})
		require.NoError(t, err)
		allocations := testing.AllocsPerRun(3, func() {
			evidence.sourceCalls = 0
			evidence.historyCalls = 0
			loads = 0
			evidence.recorded = evidence.recorded[:0]
			evidence.recordedSources = evidence.recordedSources[:0]
			evidence.preparedGraphs = evidence.preparedGraphs[:0]
			custody.opened = custody.opened[:0]
			custody.readers = custody.readers[:0]
			processed, err := worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
			require.NoError(t, err)
			require.Equal(t, 1, processed)
			require.Len(t, evidence.recorded, 1)
			require.True(t, evidence.recorded[0].Complete)
			require.Len(t, evidence.recorded[0].Members, 1)
			require.Equal(t, 1, evidence.sourceCalls)
			require.Equal(t, 1, evidence.sourceLimit)
			require.Equal(t, 1, evidence.historyCalls)
			require.Equal(t, 1, loads)
			require.Len(t, custody.opened, 2)
		})
		if size == 10 {
			small = allocations
		} else {
			assert.LessOrEqual(t, allocations, small+128, "selected-source reconstruction allocations grew with unrelated sources")
		}
		t.Logf("unselected=%d worker allocations/op=%.0f source/history/manifest calls=1/1/1 object reads=2", size, allocations)
	}
}
