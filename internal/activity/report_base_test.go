package activity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

type reportBackendFixture struct {
	t                *testing.T
	q                Query
	ids              []string
	pricing          *export.PricingBlock
	stage            string
	err              error
	calls            []string
	mu               sync.Mutex
	concurrent       bool
	beforeUsage      func(context.Context) error
	beforeCandidates func(context.Context) error
	release          func()
}

func (b *reportBackendFixture) record(stage string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, stage)
}

func (b *reportBackendFixture) ActivityReportSecondRead() (func(), bool) {
	return func() {
		if b.release != nil {
			b.release()
		}
	}, b.concurrent
}

func (b *reportBackendFixture) check(stage string, ids []string, q Query) error {
	b.t.Helper()
	assert.Equal(b.t, b.ids, ids)
	assert.Equal(b.t, b.q, q)
	b.record(stage)
	if b.stage == stage {
		return b.err
	}
	return nil
}

func (b *reportBackendFixture) ActivityReportUsage(ctx context.Context, ids []string, q Query) ([]UsageRow, *export.PricingBlock, error) {
	err := b.check("usage", ids, q)
	if err == nil && b.beforeUsage != nil {
		err = b.beforeUsage(ctx)
	}
	return []UsageRow{{
		SessionID: "s", Model: "model-a", Timestamp: "2026-06-16T10:01:00Z",
		OutputTokens: 20, Cost: money.Money{Microdollars: 100},
		Priced: true, Contributes: true,
	}}, b.pricing, err
}

func (b *reportBackendFixture) ActivityReportCandidateSource(ids []string, q Query) CandidateSource {
	assert.Equal(b.t, b.ids, ids)
	assert.Equal(b.t, b.q, q)
	var err error
	if b.stage == "candidates" {
		err = b.err
	}
	return func(ctx context.Context, yield func(IntervalCandidate) error) error {
		b.record("candidates")
		if b.beforeCandidates != nil {
			if err := b.beforeCandidates(ctx); err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		return yield(IntervalCandidate{
			SessionID: "s", StartOrdinal: 0, EndOrdinal: 1,
			Start:       mustStart(b.t, "2026-06-16T10:00:00Z"),
			End:         mustStart(b.t, "2026-06-16T10:01:00Z"),
			ClosingRole: "assistant", ClosingModel: "model-a",
		})
	}
}

func (b *reportBackendFixture) ActivityReportMessageCounts(_ context.Context, ids []string, q Query, artifacts *CandidateArtifacts) error {
	if err := b.check("counts", ids, q); err != nil {
		return err
	}
	counts := NewMessageAccumulator(q, artifacts)
	counts.Add("s", "assistant", mustStart(b.t, "2026-06-16T10:01:00Z"))
	return nil
}

func (b *reportBackendFixture) BuildProjectIdentityMap(_ context.Context, labels []string) (map[string]export.ProjectMapEntry, error) {
	assert.Equal(b.t, []string{"project-a"}, labels)
	b.record("projects")
	if b.stage == "projects" {
		return nil, b.err
	}
	return map[string]export.ProjectMapEntry{"project-a": {DisplayLabel: "project-a", ProjectKey: "project-key-a"}}, nil
}

func TestBuildReportArtifacts(t *testing.T) {
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC", BucketOverride: "1h"}, fixedNow(t))
	require.NoError(t, err)
	for _, concurrent := range []bool{false, true} {
		for _, withProgress := range []bool{true, false} {
			name := "nil callback"
			if withProgress {
				name = "progress callback"
			}
			t.Run(fmt.Sprintf("%s/concurrent=%t", name, concurrent), func(t *testing.T) {
				baseline := &reportBackendFixture{t: t, q: q, ids: []string{"s"}, pricing: &export.PricingBlock{}}
				want, _, err := BuildReportArtifacts(
					t.Context(), baseline,
					[]SessionMeta{{SessionID: "s", Project: "project-a"}}, baseline.ids, q,
					nil, "aggregating example activity report",
				)
				require.NoError(t, err)
				backend := &reportBackendFixture{t: t, q: q, ids: []string{"s"}, pricing: &export.PricingBlock{}, concurrent: concurrent}
				progress := []Progress{}
				var onProgress ProgressFunc
				if withProgress {
					onProgress = func(p Progress) { progress = append(progress, p) }
				}
				artifacts, done, err := BuildReportArtifacts(
					t.Context(), backend,
					[]SessionMeta{{SessionID: "s", Project: "project-a"}}, backend.ids, q,
					onProgress,
					"aggregating example activity report",
				)
				require.NoError(t, err)
				assert.Equal(t, want, artifacts)
				assert.Equal(t, Progress{Phase: ProgressDone, SessionsTotal: 1, SessionsProcessed: 1, RowsProcessed: 1}, done)
				if concurrent {
					assert.ElementsMatch(t, []string{"usage", "candidates"}, backend.calls[:2])
					assert.Equal(t, []string{"counts", "projects"}, backend.calls[2:])
				} else {
					assert.Equal(t, []string{"usage", "candidates", "counts", "projects"}, backend.calls)
				}
				if withProgress {
					assert.Equal(t, []Progress{
						{Phase: ProgressScanningActivity, SessionsTotal: 1},
						{Phase: ProgressScanningActivity, SessionsTotal: 1, RowsProcessed: 1},
						{Phase: ProgressFinalizing, SessionsTotal: 1, SessionsProcessed: 1, RowsProcessed: 1},
						{Phase: ProgressDone, SessionsTotal: 1, SessionsProcessed: 1, RowsProcessed: 1},
					}, progress)
				}
				assert.Equal(t, export.ActivityReportSchemaVersion, artifacts.Report.SchemaVersion)
				assert.Same(t, backend.pricing, artifacts.Report.Pricing)
				assert.Equal(t, 1, artifacts.Report.Buckets[10].AssistantMessages)
				require.Len(t, artifacts.Sessions, 1)
				assert.Equal(t, "project-key-a", artifacts.Sessions[0].ProjectKey)
				assert.Equal(t, []SessionRow{}, artifacts.Report.BySession)
			})
		}
	}
}

func TestBuildReportArtifactsErrors(t *testing.T) {
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, fixedNow(t))
	require.NoError(t, err)
	failure := errors.New("read failed")
	for _, concurrent := range []bool{false, true} {
		for _, stage := range []string{"usage", "candidates", "counts", "projects"} {
			t.Run(fmt.Sprintf("%s/concurrent=%t", stage, concurrent), func(t *testing.T) {
				backend := &reportBackendFixture{t: t, q: q, ids: []string{"s"}, stage: stage, err: failure, concurrent: concurrent}
				var done bool
				_, completion, err := BuildReportArtifacts(
					t.Context(), backend,
					[]SessionMeta{{SessionID: "s", Project: "project-a"}}, backend.ids, q,
					func(p Progress) { done = done || p.Phase == ProgressDone },
					"aggregating example activity report",
				)
				require.ErrorIs(t, err, failure)
				if stage == "candidates" {
					require.EqualError(t, err, "aggregating example activity report: read failed")
				} else {
					require.EqualError(t, err, "read failed")
				}
				assert.False(t, done)
				assert.Equal(t, Progress{}, completion)
				if stage != "projects" {
					assert.NotContains(t, backend.calls, "projects")
				}
			})
		}
	}
}

func TestBuildReportArtifactsOverlap(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, fixedNow(t))
	require.NoError(t, err)
	started := make(chan struct{})
	backend := &reportBackendFixture{
		t: t, q: q, ids: []string{"s"}, concurrent: true,
		beforeUsage: func(ctx context.Context) error {
			select {
			case <-started:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		beforeCandidates: func(context.Context) error { close(started); return nil },
	}
	artifacts, _, err := BuildReportArtifacts(ctx, backend,
		[]SessionMeta{{SessionID: "s", Project: "project-a"}}, backend.ids, q, nil, "aggregating")
	require.NoError(t, err)
	assert.Equal(t, 20, artifacts.Report.Totals.OutputTokens)
	assert.Equal(t, money.Money{Microdollars: 100}, artifacts.Report.Totals.Cost)
}

func TestBuildReportArtifactsConcurrentCancellation(t *testing.T) {
	for _, failingStage := range []string{"parent", "usage", "candidates"} {
		t.Run(failingStage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, fixedNow(t))
			require.NoError(t, err)
			failure := errors.New("stage failed")
			usageStarted, candidatesStarted := make(chan struct{}), make(chan struct{})
			usageReturned, candidatesReturned := false, false
			releases := 0
			backend := &reportBackendFixture{
				t: t, q: q, ids: []string{"s"}, concurrent: true,
				release: func() { releases++ },
				beforeUsage: func(ctx context.Context) error {
					defer func() { usageReturned = true }()
					close(usageStarted)
					select {
					case <-candidatesStarted:
					case <-ctx.Done():
						return ctx.Err()
					}
					if failingStage == "usage" {
						return failure
					}
					<-ctx.Done()
					return ctx.Err()
				},
				beforeCandidates: func(ctx context.Context) error {
					defer func() { candidatesReturned = true }()
					close(candidatesStarted)
					select {
					case <-usageStarted:
					case <-ctx.Done():
						return ctx.Err()
					}
					if failingStage == "candidates" {
						return failure
					}
					if failingStage == "parent" {
						cancel()
					}
					<-ctx.Done()
					return ctx.Err()
				},
			}
			_, _, err = BuildReportArtifacts(ctx, backend,
				[]SessionMeta{{SessionID: "s", Project: "project-a"}}, backend.ids, q, nil, "aggregating")
			switch failingStage {
			case "parent":
				require.ErrorIs(t, err, context.Canceled)
			case "usage":
				require.ErrorIs(t, err, failure)
				require.EqualError(t, err, "stage failed")
			case "candidates":
				require.ErrorIs(t, err, failure)
				require.EqualError(t, err, "aggregating: stage failed")
			}
			assert.True(t, usageReturned)
			assert.True(t, candidatesReturned)
			assert.Equal(t, 1, releases)
		})
	}
}
