package rawderive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/secrets"
)

func TestParityWorkerReplaysCompleteHistorySequentially(t *testing.T) {
	manifests := parityWorkerManifests(t, 2)
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{
		source.ID: parityWorkerHistory(manifests),
	}}
	var active, maximum atomic.Int32
	binding := parityWorkerBinding()
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
			for _, manifest := range manifests {
				if manifest.ManifestID == id {
					return manifest, nil
				}
			}
			return rawsync.CanonicalManifest{}, rawsync.ErrMissingObject
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{}, nil
		}),
		Parser: sourceParserFunc(func(_ context.Context, manifest rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if n <= old || maximum.CompareAndSwap(old, n) {
					break
				}
			}
			return ParsedManifest{Outcome: parser.ParseOutcome{
				ResultSetComplete: true,
				Results: []parser.ParseResultOutcome{{
					DataVersion: parser.DataVersionCurrent,
					Result: parser.ParseResult{Session: parser.ParsedSession{
						ID: "session-a", Agent: parser.AgentClaude,
					}},
				}},
			}}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	lease := parityWorkerLease(binding, source, 128)

	processed, err := worker.RunBatch(t.Context(), lease)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Equal(t, int32(1), maximum.Load(), "a parity batch must run one parser at a time")
	require.Len(t, evidence.recorded, 1)
	assert.True(t, evidence.recorded[0].Complete, evidence.recorded[0].Code)
	require.Len(t, evidence.recorded[0].Members, 1)
	assert.NotNil(t, evidence.recorded[0].Members[0].Fingerprint)
	assert.Equal(t, source.DependencyDigest, evidence.recordedSources[0].DependencyDigest)
}

func TestParityWorkerReconstructsHistoryPreserveFromRetainedGenerations(t *testing.T) {
	const secret = "ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg"
	manifests, bodies := parityWorkerBodyManifests(t, parser.AgentRooCode, "history-source", []string{"generation-one", "generation-two"})
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
	manifestByID := map[string]rawsync.CanonicalManifest{manifests[0].ManifestID: manifests[0], manifests[1].ManifestID: manifests[1]}
	binding := parityWorkerBinding()
	binding.Request.Cohort.Provider = parser.AgentRooCode
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
			return manifestByID[id], nil
		}),
		Materializer: Materializer{Store: &materializerStore{objects: bodies}, BaseDir: t.TempDir(), MaxTotalBytes: 1024},
		Parser: sourceParserFunc(func(_ context.Context, manifest rawsync.CanonicalManifest, tree *Materialization) (ParsedManifest, error) {
			path, pathErr := tree.EntryPath("tasks/task-1/history_item.json")
			require.NoError(t, pathErr)
			body, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			if string(body) == "generation-one" {
				return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{
					Session:     parser.ParsedSession{ID: "task-1", Agent: parser.AgentRooCode, SessionName: "retained title"},
					UsageEvents: []parser.ParsedUsageEvent{{SessionID: "task-1", Source: "provider", Model: "model", InputTokens: 7, OutputTokens: 3}},
					Messages:    []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: "retained transcript " + secret, ContentLength: len("retained transcript ") + len(secret)}},
				}}}}}, nil
			}
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: "task-1", Agent: parser.AgentRooCode}}}}}}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	lease := parityWorkerLease(binding, source, 1)
	_, err = worker.RunBatch(t.Context(), lease)
	require.NoError(t, err)
	require.Len(t, evidence.preparedGraphs, 1)
	prepared := evidence.preparedGraphs[0].Prepared
	require.NotNil(t, prepared.Session.SessionName)
	assert.Equal(t, "retained title", *prepared.Session.SessionName)
	require.Len(t, prepared.Messages, 1)
	assert.Contains(t, prepared.Messages[0].Content, "retained transcript")
	require.Len(t, prepared.UsageEvents, 1)
	assert.Equal(t, 7, prepared.UsageEvents[0].InputTokens)
	assert.NotEmpty(t, prepared.Findings, "HistoryPreserve must retain findings derived from generation one")
}

func TestParityWorkerCannotBorrowMissingGenerationFromBaselineGraph(t *testing.T) {
	manifests := parityWorkerManifests(t, 2)
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
	binding := parityWorkerBinding()
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
			if id == manifests[0].ManifestID {
				return rawsync.CanonicalManifest{}, rawsync.ErrMissingObject
			}
			return manifests[1], nil
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{}, nil
		}),
		Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	require.Len(t, evidence.recorded, 1)
	assert.Equal(t, "missing_object", evidence.recorded[0].Code)
	assert.Empty(t, evidence.preparedGraphs, "baseline preparation must not be consulted after retained history is missing")
}

func TestParityWorkerRejectsLeaseWithDifferentBinding(t *testing.T) {
	binding := parityWorkerBinding()
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: &parityEvidenceFixture{},
		Manifests: exactManifestSourceFunc(func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error) {
			return rawsync.CanonicalManifest{}, nil
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) { return nil, nil }),
		Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	lease := parityWorkerLease(binding, ParitySource{}, 1)
	lease.BindingDigest[0]++
	_, err = worker.RunBatch(t.Context(), lease)
	require.ErrorContains(t, err, "binding")
}

func TestParityWorkerMissingProviderExclusionProofIsPartial(t *testing.T) {
	manifests := parityWorkerManifests(t, 1)
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{
		sources:    []ParitySource{source},
		history:    map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)},
		prepareErr: ErrParityExclusionProvenanceUnavailable,
	}
	binding := parityWorkerBinding()
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error) {
			return manifests[0], nil
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{}, nil
		}),
		Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ExcludedSessionIDs: []string{"provider-excluded"}}}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	processed, err := worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	require.Len(t, evidence.recorded, 1)
	assert.Equal(t, ParityPartial, evidence.recorded[0].Blocker)
	assert.Equal(t, "exclusion_provenance_unavailable", evidence.recorded[0].Code)
	require.Len(t, evidence.recorded[0].Members, 1)
	assert.Equal(t, "provider-excluded", evidence.recorded[0].Members[0].Key.LogicalKey)
	assert.Equal(t, ParityPartial, evidence.recorded[0].Members[0].Verdict)
}

func TestParityWorkerSourceTimeoutAdvancesWithoutRepeatingOrExceedingBudget(t *testing.T) {
	firstManifests := parityWorkerManifestsForSource(t, "source-timeout", 1)
	secondManifests := parityWorkerManifestsForSource(t, "source-next", 1)
	first, second := parityWorkerSource(firstManifests), parityWorkerSource(secondManifests)
	evidence := &parityEvidenceFixture{
		sources: []ParitySource{first, second},
		history: map[string][]ParityHistoryEntry{
			first.ID: parityWorkerHistory(firstManifests), second.ID: parityWorkerHistory(secondManifests),
		},
	}
	byID := map[string]rawsync.CanonicalManifest{
		firstManifests[0].ManifestID: firstManifests[0], secondManifests[0].ManifestID: secondManifests[0],
	}
	binding := parityWorkerBinding()
	var firstCalls, secondCalls atomic.Int32
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
			return byID[id], nil
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{}, nil
		}),
		Parser: sourceParserFunc(func(ctx context.Context, manifest rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			if manifest.Manifest.SourceKey == "source-timeout" {
				firstCalls.Add(1)
				<-ctx.Done()
				return ParsedManifest{}, ctx.Err()
			}
			secondCalls.Add(1)
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: "session-next", Agent: parser.AgentClaude}}}}}}, nil
		}),
		Binding: binding, AttemptTimeout: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	processed, err := worker.RunBatch(t.Context(), parityWorkerLease(binding, first, 2))
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	assert.EqualValues(t, 1, firstCalls.Load())
	assert.EqualValues(t, 1, secondCalls.Load())
	require.Len(t, evidence.recorded, 2)
	assert.Equal(t, "parse_failed", evidence.recorded[0].Code)
	assert.True(t, evidence.recorded[1].Complete, evidence.recorded[1].Code)
}

func TestParityWorkerRecordsBoundedSafeFailures(t *testing.T) {
	manifests := parityWorkerManifests(t, 1)
	source := parityWorkerSource(manifests)
	tests := []struct {
		name     string
		history  []ParityHistoryEntry
		loadErr  error
		parsed   ParsedManifest
		parseErr error
		wantCode string
	}{
		{name: "missing history", wantCode: "missing_history"},
		{name: "missing object", history: parityWorkerHistory(manifests), loadErr: rawsync.ErrMissingObject, wantCode: "missing_object"},
		{name: "sandbox unavailable", history: parityWorkerHistory(manifests), parseErr: ErrSandboxUnavailable, wantCode: "sandbox_unavailable"},
		{name: "incomplete", history: parityWorkerHistory(manifests), parsed: ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: false}}, wantCode: "parse_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: tc.history}}
			binding := parityWorkerBinding()
			worker, err := NewParityWorker(ParityWorkerConfig{
				Evidence: evidence,
				Manifests: exactManifestSourceFunc(func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error) {
					if tc.loadErr != nil {
						return rawsync.CanonicalManifest{}, tc.loadErr
					}
					return manifests[0], nil
				}),
				Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
					return &Materialization{}, nil
				}),
				Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
					return tc.parsed, tc.parseErr
				}),
				Binding: binding, AttemptTimeout: time.Second,
			})
			require.NoError(t, err)
			processed, runErr := worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
			require.NoError(t, runErr)
			assert.Equal(t, 1, processed)
			require.Len(t, evidence.recorded, 1)
			assert.Equal(t, ParityPartial, evidence.recorded[0].Blocker)
			assert.Equal(t, tc.wantCode, evidence.recorded[0].Code)
		})
	}
}

func TestParityWorkerCleanupFailureAndDiagnosticsAreSafe(t *testing.T) {
	manifests := parityWorkerManifests(t, 1)
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
	binding := parityWorkerBinding()
	var removals atomic.Int32
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error) {
			return manifests[0], nil
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{root: "/private/transcript-sentinel", removeTree: func(string) error { removals.Add(1); return errors.New("private transcript sentinel") }}, nil
		}),
		Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	assert.EqualValues(t, 2, removals.Load(), "cleanup gets its existing retry")
	require.Len(t, evidence.recorded, 1)
	assert.Equal(t, "cleanup_failed", evidence.recorded[0].Code)
	assert.NotContains(t, evidence.recorded[0].Code, "private")
}

func TestParityWorkerRetainedCandidateByteLimitPrecedesPublication(t *testing.T) {
	manifests := parityWorkerManifests(t, 1)
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
	binding := parityWorkerBinding()
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error) {
			return manifests[0], nil
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{}, nil
		}),
		Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: "large", Agent: parser.AgentClaude, SessionName: strings.Repeat("x", maxParityCandidateBytes+1)}}}}}}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	require.Len(t, evidence.recorded, 1)
	assert.Equal(t, "limit_exceeded", evidence.recorded[0].Code)
	assert.Len(t, evidence.preparedGraphs, 0)
}

func TestParityWorkerCompleteDisappearanceAndTombstonePublishEmptyMembership(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "tombstone"}[tombstone], func(t *testing.T) {
			manifests := parityWorkerManifests(t, 2)
			if tombstone {
				manifests[1].Manifest.Kind = rawsync.ManifestTombstone
			}
			source := parityWorkerSource(manifests)
			evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
			binding := parityWorkerBinding()
			worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence,
				Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
					for _, m := range manifests {
						if m.ManifestID == id {
							return m, nil
						}
					}
					return rawsync.CanonicalManifest{}, rawsync.ErrMissingObject
				}),
				Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
					return &Materialization{}, nil
				}),
				Parser: sourceParserFunc(func(_ context.Context, m rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
					if m.ManifestID == manifests[0].ManifestID {
						return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: "gone", Agent: parser.AgentClaude}}}}}}, nil
					}
					return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}}, nil
				}),
				Binding: binding, AttemptTimeout: time.Second})
			require.NoError(t, err)
			_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
			require.NoError(t, err)
			require.Len(t, evidence.recorded, 1)
			assert.True(t, evidence.recorded[0].Complete)
			assert.Empty(t, evidence.recorded[0].Members)
		})
	}
}

func TestParityWorkerRealProviderCompleteAndPartialDisappearanceSandboxReplay(t *testing.T) {
	t.Run("complete replacement removes prior member", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "project")
		require.NoError(t, os.MkdirAll(project, 0o755))
		userLine := func(uuid, parent, timestamp, sessionID, kind, content string) string {
			parentJSON := "null"
			if parent != "" {
				parentJSON = strconv.Quote(parent)
			}
			kindField := ""
			if kind != "" {
				kindField = `"sessionKind":` + strconv.Quote(kind) + ","
			}
			return `{"type":"user","uuid":` + strconv.Quote(uuid) + `,"parentUuid":` + parentJSON +
				`,"timestamp":` + strconv.Quote(timestamp) + `,"sessionId":` + strconv.Quote(sessionID) + `,` + kindField +
				`"cwd":"/work/project","message":{"content":` + strconv.Quote(content) + `}}`
		}
		assistantLine := func(uuid, parent, timestamp, sessionID, kind, content string) string {
			kindField := ""
			if kind != "" {
				kindField = `"sessionKind":` + strconv.Quote(kind) + ","
			}
			return `{"type":"assistant","uuid":` + strconv.Quote(uuid) + `,"parentUuid":` + strconv.Quote(parent) +
				`,"timestamp":` + strconv.Quote(timestamp) + `,"sessionId":` + strconv.Quote(sessionID) + `,` + kindField +
				`"message":{"id":"msg_` + uuid + `","content":[{"type":"text","text":` + strconv.Quote(content) + `}]}}`
		}
		original := strings.Join([]string{
			userLine("u1", "", "2026-09-01T10:00:00Z", "orig-1111", "", "original question"),
			assistantLine("a1", "u1", "2026-09-01T10:00:05Z", "orig-1111", "", "original answer"),
		}, "\n") + "\n"
		forkReplay := strings.Join([]string{
			userLine("u1", "", "2026-09-01T10:00:00Z", "fork-2222", "bg", "original question"),
			assistantLine("a1", "u1", "2026-09-01T10:00:05Z", "fork-2222", "bg", "original answer"),
		}, "\n") + "\n"
		forkWithOwnTurn := forkReplay + strings.Join([]string{
			userLine("u2", "a1", "2026-09-01T11:00:00Z", "fork-2222", "bg", "fork question"),
			assistantLine("a2", "u2", "2026-09-01T11:00:05Z", "fork-2222", "bg", "fork answer"),
		}, "\n") + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(project, "orig-1111.jsonl"), []byte(original), 0o644))
		forkPath := filepath.Join(project, "fork-2222.jsonl")
		require.NoError(t, os.WriteFile(forkPath, []byte(forkWithOwnTurn), 0o644))
		provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}, Machine: "hosted-worker"})
		require.True(t, ok)
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 2)
		var forkSource parser.SourceRef
		for _, source := range sources {
			if strings.HasSuffix(source.Key, "fork-2222.jsonl") {
				forkSource = source
			}
		}
		require.NotEmpty(t, forkSource.Key)
		local, err := provider.Parse(t.Context(), parser.ParseRequest{Source: forkSource})
		require.NoError(t, err)
		require.True(t, local.ResultSetComplete)
		require.Len(t, local.Results, 1)
		assert.Equal(t, "fork-2222", local.Results[0].Result.Session.ID)
		assert.Empty(t, local.ExcludedSessionIDs)
		first, objects := manifestFromCapturePlan(t, parser.AgentClaude, provider, forkSource)

		require.NoError(t, os.WriteFile(forkPath, []byte(forkReplay), 0o644))
		local, err = provider.Parse(t.Context(), parser.ParseRequest{Source: forkSource})
		require.NoError(t, err)
		assert.True(t, local.ResultSetComplete)
		assert.Empty(t, local.Results)
		assert.Equal(t, []string{"fork-2222"}, local.ExcludedSessionIDs)
		second, secondObjects := manifestFromCapturePlan(t, parser.AgentClaude, provider, forkSource)
		maps.Copy(objects, secondObjects)
		store := &parityRetainedCustody{manifests: map[string]rawsync.CanonicalManifest{first.ManifestID: first, second.ManifestID: second}, objects: objects}
		bound, err := NewBoundSubprocessParser(20 * time.Second)
		if err != nil {
			if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
				require.NoError(t, err)
			}
			t.Skip("bound parser unavailable")
		}
		defer bound.Close()
		if err = bound.Preflight(t.Context()); err != nil {
			if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
				require.NoError(t, err)
			}
			t.Skip("kernel isolation unavailable")
		}
		binding := parityWorkerBinding()
		binding.Request.Cohort.Provider = parser.AgentClaude
		binding.Versions.ParserBuild, err = bound.BuildIdentity()
		require.NoError(t, err)
		source := parityWorkerSource([]rawsync.CanonicalManifest{first, second})
		evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory([]rawsync.CanonicalManifest{first, second})}}
		worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence, Manifests: ManifestLoader{Store: store, Limits: rawsync.DefaultManifestLimits()}, Materializer: Materializer{Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}, Parser: bound, Binding: binding, AttemptTimeout: 20 * time.Second})
		require.NoError(t, err)
		_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
		require.NoError(t, err)
		require.Len(t, evidence.recorded, 1)
		assert.True(t, evidence.recorded[0].Complete)
		require.Len(t, evidence.recorded[0].Members, 1)
		assert.Equal(t, "exclusion", evidence.recorded[0].Members[0].Key.Kind)
		assert.Equal(t, "fork-2222", evidence.recorded[0].Members[0].Key.LogicalKey)
		assert.Len(t, evidence.preparedGraphs, 1)
		assert.True(t, evidence.preparedGraphs[0].Overlay.ProviderExcluded)
	})

	t.Run("partial replacement cannot publish disappearance", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "sessions", "good.transcript.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		header := `{"kind":"header","format_version":2,"session_id":"good","created_at":"2026-09-01T10:00:00Z","working_dir":"/workspace/example"}` + "\n"
		entry := `{"kind":"entry","seq":1,"turn":{"kind":"USER_INPUT","timestamp":"2026-09-01T10:00:01Z","message":{"role":"user","content":[{"kind":"text","text":"Hello"}]}}}` + "\n"
		require.NoError(t, os.WriteFile(path, []byte(header+entry), 0o644))
		provider, ok := parser.NewProvider(parser.AgentEvener, parser.ProviderConfig{Roots: []string{root}, Machine: "hosted-worker"})
		require.True(t, ok)
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		first, objects := manifestFromCapturePlan(t, parser.AgentEvener, provider, sources[0])
		require.NoError(t, os.WriteFile(path, []byte(header+entry+`{"kind":"entry"`), 0o644))
		local, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
		require.NoError(t, err)
		assert.False(t, local.ResultSetComplete)
		assert.Empty(t, local.Results)
		second, secondObjects := manifestFromCapturePlan(t, parser.AgentEvener, provider, sources[0])
		maps.Copy(objects, secondObjects)
		store := &parityRetainedCustody{manifests: map[string]rawsync.CanonicalManifest{first.ManifestID: first, second.ManifestID: second}, objects: objects}
		bound, err := NewBoundSubprocessParser(20 * time.Second)
		if err != nil {
			if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
				require.NoError(t, err)
			}
			t.Skip("bound parser unavailable")
		}
		defer bound.Close()
		if err = bound.Preflight(t.Context()); err != nil {
			if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
				require.NoError(t, err)
			}
			t.Skip("kernel isolation unavailable")
		}
		binding := parityWorkerBinding()
		binding.Request.Cohort.Provider = parser.AgentEvener
		binding.Versions.ParserBuild, err = bound.BuildIdentity()
		require.NoError(t, err)
		source := parityWorkerSource([]rawsync.CanonicalManifest{first, second})
		evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory([]rawsync.CanonicalManifest{first, second})}}
		worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence, Manifests: ManifestLoader{Store: store, Limits: rawsync.DefaultManifestLimits()}, Materializer: Materializer{Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}, Parser: bound, Binding: binding, AttemptTimeout: 20 * time.Second})
		require.NoError(t, err)
		_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
		require.NoError(t, err)
		require.Len(t, evidence.recorded, 1)
		assert.False(t, evidence.recorded[0].Complete)
		assert.Equal(t, "parse_failed", evidence.recorded[0].Code)
		assert.Empty(t, evidence.recorded[0].Members)
		assert.Empty(t, evidence.preparedGraphs)
	})
}

func TestParityWorkerRejectsHistoryAndMemberLimitsBeforePublication(t *testing.T) {
	manifests := parityWorkerManifests(t, 1)
	source := parityWorkerSource(manifests)
	tests := []struct {
		name    string
		history []ParityHistoryEntry
		results int
	}{
		{name: "1025 manifests", history: make([]ParityHistoryEntry, 1025)},
		{name: "4097 members", history: parityWorkerHistory(manifests), results: 4097},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.history {
				tc.history[i] = ParityHistoryEntry{Generation: int64(i + 1), ManifestID: manifests[0].ManifestID}
			}
			outcome := parser.ParseOutcome{ResultSetComplete: true}
			for i := 0; i < tc.results; i++ {
				outcome.Results = append(outcome.Results, parser.ParseResultOutcome{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{
					ID: strings.Repeat("m", 8) + string(rune(i+1)), Agent: parser.AgentClaude,
				}}})
			}
			evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: tc.history}}
			binding := parityWorkerBinding()
			worker, err := NewParityWorker(ParityWorkerConfig{
				Evidence: evidence,
				Manifests: exactManifestSourceFunc(func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error) {
					return manifests[0], nil
				}),
				Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
					return &Materialization{}, nil
				}),
				Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
					return ParsedManifest{Outcome: outcome}, nil
				}),
				Binding: binding, AttemptTimeout: time.Second,
			})
			require.NoError(t, err)
			_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
			require.NoError(t, err)
			require.Len(t, evidence.recorded, 1)
			assert.Equal(t, "limit_exceeded", evidence.recorded[0].Code)
			assert.False(t, evidence.recorded[0].Complete)
		})
	}
}

func TestParityWorkerCanonicalizesWrappedContextBoundaryErrors(t *testing.T) {
	privateCanceled := fmt.Errorf("private-path/transcript-sentinel: %w", context.Canceled)
	privateDeadline := fmt.Errorf("private-path/transcript-sentinel: %w", context.DeadlineExceeded)
	tests := []struct {
		name      string
		configure func(*parityEvidenceFixture)
		want      error
	}{
		{name: "next sources", configure: func(f *parityEvidenceFixture) { f.nextErr = privateCanceled }, want: context.Canceled},
		{name: "prepare graph", configure: func(f *parityEvidenceFixture) { f.prepareBoundaryErr = privateDeadline }, want: context.DeadlineExceeded},
		{name: "record", configure: func(f *parityEvidenceFixture) { f.recordErr = privateCanceled }, want: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifests := parityWorkerManifests(t, 1)
			source := parityWorkerSource(manifests)
			evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
			tc.configure(evidence)
			binding := parityWorkerBinding()
			worker, err := NewParityWorker(ParityWorkerConfig{
				Evidence: evidence,
				Manifests: exactManifestSourceFunc(func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error) {
					return manifests[0], nil
				}),
				Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
					return &Materialization{}, nil
				}),
				Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
					return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: "session-a", Agent: parser.AgentClaude}}}}}}, nil
				}),
				Binding: binding, AttemptTimeout: time.Second,
			})
			require.NoError(t, err)
			_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, tc.want.Error(), err.Error())
			assert.NotContains(t, err.Error(), "private")
		})
	}
}

func TestParityWorkerRejectsAggregateChildRowsBeforePublication(t *testing.T) {
	manifests := parityWorkerManifests(t, 2)
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
	binding := parityWorkerBinding()
	worker, err := NewParityWorker(ParityWorkerConfig{
		Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
			for _, manifest := range manifests {
				if manifest.ManifestID == id {
					return manifest, nil
				}
			}
			return rawsync.CanonicalManifest{}, rawsync.ErrMissingObject
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{}, nil
		}),
		Parser: sourceParserFunc(func(_ context.Context, manifest rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			id := "generation-one"
			if manifest.ManifestID == manifests[1].ManifestID {
				id = "generation-two"
			}
			messages := make([]parser.ParsedMessage, maxParityChildRows/2+1)
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: id, Agent: parser.AgentClaude}, Messages: messages}}}}}, nil
		}),
		Binding: binding, AttemptTimeout: time.Second,
	})
	require.NoError(t, err)
	_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	require.Len(t, evidence.recorded, 1)
	assert.Equal(t, "limit_exceeded", evidence.recorded[0].Code)
	assert.Empty(t, evidence.preparedGraphs)
}

func TestParityWorkerRejectsTransientAggregateCandidateBytes(t *testing.T) {
	manifests := parityWorkerManifests(t, 2)
	source := parityWorkerSource(manifests)
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory(manifests)}}
	binding := parityWorkerBinding()
	worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence,
		Manifests: exactManifestSourceFunc(func(_ context.Context, _ rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
			for _, manifest := range manifests {
				if manifest.ManifestID == id {
					return manifest, nil
				}
			}
			return rawsync.CanonicalManifest{}, rawsync.ErrMissingObject
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{}, nil
		}),
		Parser: sourceParserFunc(func(_ context.Context, manifest rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			id := "generation-one"
			if manifest.ManifestID == manifests[1].ManifestID {
				id = "generation-two"
			}
			calls := make([]parser.ParsedToolCall, 10000)
			for i := range calls {
				calls[i] = parser.ParsedToolCall{ToolName: "Read", Category: "read", InputJSON: `{"value":"` + strings.Repeat("x", 1980) + `"}`}
			}
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionCurrent, Result: parser.ParseResult{Session: parser.ParsedSession{ID: id, Agent: parser.AgentClaude}, Messages: []parser.ParsedMessage{{Role: parser.RoleAssistant, ToolCalls: calls}}}}}}}, nil
		}), Binding: binding, AttemptTimeout: time.Second})
	require.NoError(t, err)
	_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	require.Len(t, evidence.recorded, 1)
	assert.Equal(t, "limit_exceeded", evidence.recorded[0].Code)
	assert.Len(t, evidence.preparedGraphs, 0)
}

func TestParityWorkerRetainedClaudeCompanionSandboxReplay(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	sessionID := "session-hosted"
	transcriptPath := filepath.Join(projectDir, sessionID+".jsonl")
	sidecarPath := filepath.Join(projectDir, sessionID, "tool-results", "r1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(sidecarPath), 0o755))
	fullOutput := "retained companion output\n"
	require.NoError(t, os.WriteFile(sidecarPath, []byte(fullOutput), 0o644))
	persisted := "<persisted-output>\nFull output saved to: " + sidecarPath + "\n</persisted-output>"
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"local_command","timestamp":"2026-08-13T11:59:59Z","sessionId":"` + sessionID + `","content":"<command-name>/rename</command-name>\n<command-args>retained title</command-args>"}`,
		`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"` + sessionID + `","cwd":"/work/project","message":{"content":"retained title"}}`,
		`{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"` + sessionID + `","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make"}}],"usage":{"input_tokens":7,"output_tokens":3}}}`,
		`{"type":"user","timestamp":"2026-08-13T12:00:02Z","uuid":"u2","parentUuid":"a1","sessionId":"` + sessionID + `","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + strconv.Quote(persisted) + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + strconv.Quote(sidecarPath) + `,"persistedOutputSize":26}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}, Machine: "hosted-worker"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	local, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, local.Results, 1)
	first, firstObjects := manifestFromCapturePlan(t, parser.AgentClaude, provider, sources[0])
	require.NoError(t, os.WriteFile(sidecarPath, []byte(fullOutput+"generation two\n"), 0o644))
	transcript += strings.Join([]string{
		`{"type":"user","timestamp":"2026-08-13T12:00:03Z","uuid":"u3","parentUuid":"u2","sessionId":"` + sessionID + `","cwd":"/work/project","message":{"content":"generation two question"}}`,
		`{"type":"assistant","timestamp":"2026-08-13T12:00:04Z","uuid":"a2","parentUuid":"u3","sessionId":"` + sessionID + `","message":{"content":[{"type":"text","text":"generation two answer"}],"usage":{"input_tokens":5,"output_tokens":4}}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))
	local, err = provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, local.Results, 1)
	localResult := local.Results[0].Result
	assert.Equal(t, "retained title", localResult.Session.SessionName)
	require.Len(t, localResult.Messages, 5)
	require.Len(t, localResult.Messages[2].ToolResults, 1)
	assert.Contains(t, parser.DecodeContent(localResult.Messages[2].ToolResults[0].ContentRaw), "generation two")
	assert.Equal(t, 7, localResult.Messages[1].ContextTokens)
	assert.Equal(t, 3, localResult.Messages[1].OutputTokens)
	assert.Equal(t, 5, localResult.Messages[4].ContextTokens)
	assert.Equal(t, 4, localResult.Messages[4].OutputTokens)
	assert.Equal(t, 7, localResult.Session.TotalOutputTokens)
	assert.Equal(t, 7, localResult.Session.PeakContextTokens)
	second, secondObjects := manifestFromCapturePlan(t, parser.AgentClaude, provider, sources[0])
	store := &parityRetainedCustody{manifests: map[string]rawsync.CanonicalManifest{first.ManifestID: first, second.ManifestID: second}, objects: firstObjects}
	maps.Copy(store.objects, secondObjects)
	bound, err := NewBoundSubprocessParser(20 * time.Second)
	if err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("bound parser unavailable")
	}
	defer bound.Close()
	if err = bound.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("kernel isolation unavailable")
	}
	binding := parityWorkerBinding()
	binding.Request.Cohort.Provider = parser.AgentClaude
	binding.Versions.ParserBuild, err = bound.BuildIdentity()
	require.NoError(t, err)
	source := parityWorkerSource([]rawsync.CanonicalManifest{first, second})
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory([]rawsync.CanonicalManifest{first, second})}}
	worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence, Manifests: ManifestLoader{Store: store, Limits: rawsync.DefaultManifestLimits()}, Materializer: Materializer{Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}, Parser: bound, Binding: binding, AttemptTimeout: 20 * time.Second})
	require.NoError(t, err)
	_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	require.Len(t, evidence.recorded, 1)
	assert.True(t, evidence.recorded[0].Complete, evidence.recorded[0].Code)
	require.Len(t, evidence.preparedGraphs, 1)
	prepared := evidence.preparedGraphs[0].Prepared
	require.NotNil(t, prepared.Session.SessionName)
	assert.Equal(t, "retained title", *prepared.Session.SessionName)
	require.Len(t, prepared.Messages, 4)
	require.Len(t, prepared.Messages[1].ToolCalls, 1)
	assert.Contains(t, prepared.Messages[1].ToolCalls[0].ResultContent, "generation two")
	assert.Equal(t, 7, prepared.Messages[1].ContextTokens)
	assert.Equal(t, 3, prepared.Messages[1].OutputTokens)
	assert.Equal(t, "generation two answer", prepared.Messages[3].Content)
	assert.Equal(t, 5, prepared.Messages[3].ContextTokens)
	assert.Equal(t, 4, prepared.Messages[3].OutputTokens)
	assert.Equal(t, 7, prepared.Session.TotalOutputTokens)
	assert.Equal(t, 7, prepared.Session.PeakContextTokens)

	var firstTranscriptObject, secondTranscriptObject rawsync.ObjectRef
	for _, entry := range first.Manifest.Entries {
		if strings.HasSuffix(entry.Path, sessionID+".jsonl") {
			require.Len(t, entry.Objects, 1)
			firstTranscriptObject = entry.Objects[0]
		}
	}
	for _, entry := range second.Manifest.Entries {
		if strings.HasSuffix(entry.Path, sessionID+".jsonl") {
			require.Len(t, entry.Objects, 1)
			secondTranscriptObject = entry.Objects[0]
		}
	}
	require.NotEqual(t, rawsync.ObjectRef{}, firstTranscriptObject)
	require.NotEqual(t, rawsync.ObjectRef{}, secondTranscriptObject)
	require.NotEqual(t, firstTranscriptObject, secondTranscriptObject)
	delete(store.objects, firstTranscriptObject)
	missingEvidence := &parityEvidenceFixture{
		sources: []ParitySource{source},
		history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory([]rawsync.CanonicalManifest{first, second})},
	}
	missingWorker, err := NewParityWorker(ParityWorkerConfig{Evidence: missingEvidence, Manifests: ManifestLoader{Store: store, Limits: rawsync.DefaultManifestLimits()}, Materializer: Materializer{Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}, Parser: bound, Binding: binding, AttemptTimeout: 20 * time.Second})
	require.NoError(t, err)
	_, err = missingWorker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	require.Len(t, missingEvidence.recorded, 1)
	assert.Equal(t, "missing_object", missingEvidence.recorded[0].Code)
	assert.Len(t, missingEvidence.preparedGraphs, 0, "baseline-only evidence must never become candidate input")

	store.objects[firstTranscriptObject] = firstObjects[firstTranscriptObject]
	tombstone, err := rawsync.ValidateAndCanonicalize(first.Identity, rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentClaude, ConfiguredRootID: first.Manifest.ConfiguredRootID, SourceKey: first.Manifest.SourceKey, CaptureID: "capture-tombstone", CapturedAt: time.Date(2026, 9, 12, 12, 0, 2, 0, time.UTC), Kind: rawsync.ManifestTombstone}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	store.manifests[tombstone.ManifestID] = tombstone
	tombSource := parityWorkerSource([]rawsync.CanonicalManifest{first, tombstone})
	tombEvidence := &parityEvidenceFixture{sources: []ParitySource{tombSource}, history: map[string][]ParityHistoryEntry{tombSource.ID: parityWorkerHistory([]rawsync.CanonicalManifest{first, tombstone})}}
	tombWorker, err := NewParityWorker(ParityWorkerConfig{Evidence: tombEvidence, Manifests: ManifestLoader{Store: store, Limits: rawsync.DefaultManifestLimits()}, Materializer: Materializer{Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}, Parser: bound, Binding: binding, AttemptTimeout: 20 * time.Second})
	require.NoError(t, err)
	_, err = tombWorker.RunBatch(t.Context(), parityWorkerLease(binding, tombSource, 1))
	require.NoError(t, err)
	require.Len(t, tombEvidence.recorded, 1)
	assert.True(t, tombEvidence.recorded[0].Complete)
	assert.Empty(t, tombEvidence.recorded[0].Members, "authoritative tombstone is complete disappearance")
}

func TestParityWorkerRealClaudeExclusionSandboxReplay(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	orig := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-01T10:00:00Z","sessionId":"orig-1111","cwd":"/work/project","message":{"content":"first question"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T10:00:05Z","sessionId":"orig-1111","message":{"id":"msg_a1","content":[{"type":"text","text":"first answer"}]}}`,
	}, "\n") + "\n"
	fork := strings.ReplaceAll(strings.ReplaceAll(orig, "orig-1111", "fork-2222"), `"sessionId":"fork-2222",`, `"sessionId":"fork-2222","sessionKind":"bg",`)
	require.NoError(t, os.WriteFile(filepath.Join(project, "orig-1111.jsonl"), []byte(orig), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(project, "fork-2222.jsonl"), []byte(fork), 0o644))
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}, Machine: "hosted-worker"})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	var forkSource parser.SourceRef
	for _, candidate := range discovered {
		if strings.HasSuffix(candidate.Key, "fork-2222.jsonl") {
			forkSource = candidate
		}
	}
	require.NotEmpty(t, forkSource.Key)
	manifest, objects := manifestFromCapturePlan(t, parser.AgentClaude, provider, forkSource)
	store := &parityRetainedCustody{manifests: map[string]rawsync.CanonicalManifest{manifest.ManifestID: manifest}, objects: objects}
	bound, err := NewBoundSubprocessParser(20 * time.Second)
	if err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("bound parser unavailable")
	}
	defer bound.Close()
	if err = bound.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("kernel isolation unavailable")
	}
	binding := parityWorkerBinding()
	binding.Request.Cohort.Provider = parser.AgentClaude
	binding.Versions.ParserBuild, err = bound.BuildIdentity()
	require.NoError(t, err)
	source := parityWorkerSource([]rawsync.CanonicalManifest{manifest})
	evidence := &parityEvidenceFixture{sources: []ParitySource{source}, history: map[string][]ParityHistoryEntry{source.ID: parityWorkerHistory([]rawsync.CanonicalManifest{manifest})}, prepareErr: ErrParityExclusionProvenanceUnavailable}
	worker, err := NewParityWorker(ParityWorkerConfig{Evidence: evidence, Manifests: ManifestLoader{Store: store, Limits: rawsync.DefaultManifestLimits()}, Materializer: Materializer{Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}, Parser: bound, Binding: binding, AttemptTimeout: 20 * time.Second})
	require.NoError(t, err)
	_, err = worker.RunBatch(t.Context(), parityWorkerLease(binding, source, 1))
	require.NoError(t, err)
	require.Len(t, evidence.recorded, 1)
	assert.Equal(t, "exclusion_provenance_unavailable", evidence.recorded[0].Code)
	require.Len(t, evidence.recorded[0].Members, 1)
	assert.Equal(t, "fork-2222", evidence.recorded[0].Members[0].Key.LogicalKey)
	assert.Equal(t, ParityPartial, evidence.recorded[0].Members[0].Verdict)
}

type parityRetainedCustody struct {
	manifests map[string]rawsync.CanonicalManifest
	objects   map[rawsync.ObjectRef][]byte
}

func (s *parityRetainedCustody) OpenManifest(_ context.Context, identity rawsync.AuthIdentity, id string) (rawsync.ObjectInfo, rawsync.VerifiedObjectReader, error) {
	manifest, ok := s.manifests[id]
	if !ok || manifest.Identity != identity {
		return rawsync.ObjectInfo{}, nil, rawsync.ErrNotFound
	}
	ref, err := rawsync.NewObjectRef(id, int64(len(manifest.CanonicalJSON)))
	if err != nil {
		return rawsync.ObjectInfo{}, nil, err
	}
	return rawsync.ObjectInfo{Ref: ref}, &testVerifiedReader{Reader: bytes.NewReader(manifest.CanonicalJSON)}, nil
}

func (s *parityRetainedCustody) CopyObject(_ context.Context, _ string, ref rawsync.ObjectRef, dst io.Writer) (rawsync.ObjectInfo, error) {
	body, ok := s.objects[ref]
	if !ok {
		return rawsync.ObjectInfo{}, rawsync.ErrNotFound
	}
	if _, err := dst.Write(body); err != nil {
		return rawsync.ObjectInfo{}, err
	}
	return rawsync.ObjectInfo{Ref: ref}, nil
}

type exactManifestSourceFunc func(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error)

func (f exactManifestSourceFunc) LoadManifest(ctx context.Context, identity rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
	return f(ctx, identity, id)
}

type parityEvidenceFixture struct {
	sources            []ParitySource
	history            map[string][]ParityHistoryEntry
	recorded           []ParitySourceResult
	recordedSources    []ParitySource
	prepareErr         error
	preparedGraphs     []ParityGraph
	nextErr            error
	prepareBoundaryErr error
	recordErr          error
}

func (f *parityEvidenceFixture) NextParitySources(context.Context, ParityLease, string, int) ([]ParitySource, error) {
	return append([]ParitySource(nil), f.sources...), f.nextErr
}
func (f *parityEvidenceFixture) ReserveParitySource(context.Context, ParityLease, ParitySource) error {
	return nil
}
func (f *parityEvidenceFixture) NextParityHistory(_ context.Context, _ ParityLease, sourceID string, after int64, limit int) ([]ParityHistoryEntry, error) {
	var page []ParityHistoryEntry
	for _, entry := range f.history[sourceID] {
		if entry.Generation > after {
			page = append(page, entry)
			if len(page) == limit {
				break
			}
		}
	}
	return page, nil
}
func (f *parityEvidenceFixture) PrepareParityGraph(_ context.Context, _ ParityLease, _ ParitySource, graph ParityGraph) (ParityGraph, error) {
	f.preparedGraphs = append(f.preparedGraphs, graph)
	if f.prepareBoundaryErr != nil {
		return graph, f.prepareBoundaryErr
	}
	return graph, f.prepareErr
}
func (f *parityEvidenceFixture) RecordParitySource(_ context.Context, _ ParityLease, source ParitySource, result ParitySourceResult) error {
	f.recordedSources = append(f.recordedSources, source)
	f.recorded = append(f.recorded, result)
	return f.recordErr
}

func parityWorkerManifests(t *testing.T, count int) []rawsync.CanonicalManifest {
	t.Helper()
	return parityWorkerManifestsForSource(t, "source-a", count)
}

func parityWorkerManifestsForSource(t *testing.T, sourceKey string, count int) []rawsync.CanonicalManifest {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifests := make([]rawsync.CanonicalManifest, count)
	for i := range manifests {
		body := []byte{byte(i + 1)}
		object := objectRefForBytes(t, body)
		manifests[i], err = rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
			SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentClaude,
			ConfiguredRootID: "root-a", SourceKey: sourceKey, CaptureID: "capture-" + string(rune('a'+i)),
			CapturedAt: time.Date(2026, 9, 12, 12, 0, i, 0, time.UTC), Kind: rawsync.ManifestSnapshot,
			Entries: []rawsync.Entry{{Path: "source.jsonl", Type: "file", Length: 1, Objects: []rawsync.ObjectRef{object}}},
		}, rawsync.DefaultManifestLimits())
		require.NoError(t, err)
	}
	return manifests
}

func parityWorkerSource(manifests []rawsync.CanonicalManifest) ParitySource {
	return ParitySource{ID: SourceID(manifests[0]), Identity: manifests[0].Identity,
		HeadManifestID: manifests[len(manifests)-1].ManifestID, HeadGeneration: int64(len(manifests)),
		DependencyDigest: ParityDigest{1}, Required: true}
}
func parityWorkerHistory(manifests []rawsync.CanonicalManifest) []ParityHistoryEntry {
	entries := make([]ParityHistoryEntry, len(manifests))
	for i, manifest := range manifests {
		entries[i] = ParityHistoryEntry{ManifestID: manifest.ManifestID, Generation: int64(i + 1)}
	}
	return entries
}
func parityWorkerBinding() ParityBinding {
	binding := parityTestBinding()
	binding.Request.Cohort.Provider = parser.AgentClaude
	binding.Versions.Data = db.CurrentDataVersion()
	binding.Versions.Quality = db.CurrentQualitySignalVersion
	binding.Versions.SecretRules = secrets.DefiniteRulesVersion()
	return binding
}

func parityWorkerLease(binding ParityBinding, source ParitySource, batch int) ParityLease {
	digest, err := DigestParityBinding(binding)
	if err != nil {
		panic(err)
	}
	return ParityLease{RunID: binding.Request.RunID, Owner: "owner", Token: "00000000-0000-4000-8000-000000000077",
		Request: binding.Request, RequestGeneration: 1, InitEpoch: 1, BindingDigest: digest,
		ObservedAt: binding.ObservedAt, BatchSize: batch}
}

func parityWorkerBodyManifests(t *testing.T, agent parser.AgentType, sourceKey string, contents []string) ([]rawsync.CanonicalManifest, map[rawsync.ObjectRef][]byte) {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifests := make([]rawsync.CanonicalManifest, len(contents))
	objects := make(map[rawsync.ObjectRef][]byte, len(contents))
	for i, content := range contents {
		body := []byte(content)
		object := objectRefForBytes(t, body)
		objects[object] = body
		manifests[i], err = rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
			SchemaVersion: rawsync.ManifestSchemaVersion, Provider: agent, ConfiguredRootID: "root-a", SourceKey: sourceKey,
			CaptureID: "body-capture-" + string(rune('a'+i)), CapturedAt: time.Date(2026, 9, 12, 13, 0, i, 0, time.UTC), Kind: rawsync.ManifestSnapshot,
			Entries: []rawsync.Entry{{Path: "tasks/task-1/history_item.json", Type: "file", Length: int64(len(body)), Objects: []rawsync.ObjectRef{object}}},
		}, rawsync.DefaultManifestLimits())
		require.NoError(t, err)
	}
	return manifests, objects
}
