package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

type reportBackendFixture struct {
	t       *testing.T
	q       Query
	ids     []string
	pricing *export.PricingBlock
	stage   string
	err     error
	calls   []string
}

func (b *reportBackendFixture) check(stage string, ids []string, q Query) error {
	b.t.Helper()
	assert.Equal(b.t, b.ids, ids)
	assert.Equal(b.t, b.q, q)
	b.calls = append(b.calls, stage)
	if b.stage == stage {
		return b.err
	}
	return nil
}

func (b *reportBackendFixture) ActivityReportUsage(_ context.Context, ids []string, q Query) ([]UsageRow, *export.PricingBlock, error) {
	err := b.check("usage", ids, q)
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
	return func(_ context.Context, yield func(IntervalCandidate) error) error {
		b.calls = append(b.calls, "candidates")
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
	b.calls = append(b.calls, "projects")
	if b.stage == "projects" {
		return nil, b.err
	}
	return map[string]export.ProjectMapEntry{"project-a": {DisplayLabel: "project-a", ProjectKey: "project-key-a"}}, nil
}

func TestBuildReportArtifacts(t *testing.T) {
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC", BucketOverride: "1h"}, fixedNow(t))
	require.NoError(t, err)
	for _, withProgress := range []bool{true, false} {
		name := "nil callback"
		if withProgress {
			name = "progress callback"
		}
		t.Run(name, func(t *testing.T) {
			backend := &reportBackendFixture{t: t, q: q, ids: []string{"s"}, pricing: &export.PricingBlock{}}
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
			assert.Equal(t, Progress{Phase: ProgressDone, SessionsTotal: 1, SessionsProcessed: 1, RowsProcessed: 1}, done)
			assert.Equal(t, []string{"usage", "candidates", "counts", "projects"}, backend.calls)
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

func TestBuildReportArtifactsErrors(t *testing.T) {
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, fixedNow(t))
	require.NoError(t, err)
	failure := errors.New("read failed")
	for _, stage := range []string{"usage", "candidates", "counts", "projects"} {
		t.Run(stage, func(t *testing.T) {
			backend := &reportBackendFixture{t: t, q: q, ids: []string{"s"}, stage: stage, err: failure}
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
