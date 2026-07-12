package service_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func seedPairwiseUsageFixture(t *testing.T, d *db.DB) {
	t.Helper()

	type usageSeed struct {
		id      string
		project string
		started string
		model   string
		input   int
		output  int
		create  int
		read    int
	}

	seeds := []usageSeed{
		{
			id:      "usage-alpha-sonnet",
			project: "alpha",
			started: "2024-06-01T09:00:00Z",
			model:   "claude-sonnet-4-20250514",
			input:   100,
			output:  50,
			create:  10,
			read:    20,
		},
		{
			id:      "usage-beta-gpt",
			project: "beta",
			started: "2024-06-01T10:00:00Z",
			model:   "gpt-4o",
			input:   30,
			output:  15,
			create:  0,
			read:    5,
		},
		{
			id:      "usage-beta-sonnet",
			project: "beta",
			started: "2024-06-01T11:00:00Z",
			model:   "claude-sonnet-4-20250514",
			input:   70,
			output:  35,
			create:  0,
			read:    0,
		},
	}

	for _, seed := range seeds {
		started := seed.started
		dbtest.SeedSessionWithMessages(
			t,
			d,
			seed.id,
			seed.project,
			[]db.Message{
				dbtest.UserMsg(seed.id, 0, "compare usage"),
				assistantUsageMsg(seed.id, 1, seed),
			},
			dbtest.WithMessageCounts(2, 1),
			func(s *db.Session) {
				s.Agent = "claude"
				s.StartedAt = &started
				s.EndedAt = &started
			},
		)
	}
}

func seedCommaProjectUsageFixture(t *testing.T, d *db.DB) {
	t.Helper()
	started := "2024-06-01T09:00:00Z"
	for _, seed := range []struct {
		id      string
		project string
		input   int
	}{
		{id: "usage-comma", project: "team,core", input: 100},
		{id: "usage-other", project: "other", input: 25},
	} {
		msg := dbtest.AsstMsg(seed.id, 1, "done")
		msg.Timestamp = started
		msg.Model = "test-model"
		msg.TokenUsage = fmt.Appendf(nil,
			`{"input_tokens":%d}`, seed.input)
		dbtest.SeedSessionWithMessages(
			t,
			d,
			seed.id,
			seed.project,
			[]db.Message{
				dbtest.UserMsg(seed.id, 0, "compare usage"),
				msg,
			},
			dbtest.WithMessageCounts(2, 1),
			func(s *db.Session) {
				s.Agent = "claude"
				s.StartedAt = &started
				s.EndedAt = &started
			},
		)
	}
}

func assistantUsageMsg(
	sessionID string, ordinal int, seed struct {
		id      string
		project string
		started string
		model   string
		input   int
		output  int
		create  int
		read    int
	},
) db.Message {
	msg := dbtest.AsstMsg(sessionID, ordinal, "done")
	msg.Timestamp = seed.started
	msg.Model = seed.model
	msg.TokenUsage = fmt.Appendf(nil,
		`{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}`,
		seed.input,
		seed.output,
		seed.create,
		seed.read,
	)
	return msg
}

func TestBuildUsageFilter_ValidMapping(t *testing.T) {
	t.Parallel()
	f, err := service.BuildUsageFilter(service.UsageRequest{
		From:    "2024-06-01",
		To:      "2024-06-15",
		Project: "proj",
		Agent:   "claude",
		// IncludeOneShot/IncludeAutomated default false -> exclude true.
	})
	require.NoError(t, err)
	assert.Equal(t, "2024-06-01", f.From)
	assert.Equal(t, "2024-06-15", f.To)
	assert.Equal(t, "proj", f.Project)
	assert.Equal(t, "UTC", f.Timezone, "empty timezone defaults to UTC")
	assert.True(t, f.ExcludeOneShot, "IncludeOneShot=false -> ExcludeOneShot=true")
	assert.True(t, f.ExcludeAutomated, "IncludeAutomated=false -> ExcludeAutomated=true")
	assert.True(t, f.Breakdowns, "summary needs per-day breakdowns")
}

func TestBuildUsageFilter_IncludeFlagsInvert(t *testing.T) {
	t.Parallel()
	f, err := service.BuildUsageFilter(service.UsageRequest{
		From:             "2024-06-01",
		To:               "2024-06-02",
		IncludeOneShot:   true,
		IncludeAutomated: true,
	})
	require.NoError(t, err)
	assert.False(t, f.ExcludeOneShot)
	assert.False(t, f.ExcludeAutomated)
}

func TestBuildUsageFilter_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  service.UsageRequest
	}{
		{"bad timezone", service.UsageRequest{Timezone: "Fake/Zone"}},
		{"bad from date", service.UsageRequest{From: "yesterday", To: "2024-06-02"}},
		{"from after to", service.UsageRequest{From: "2024-07-01", To: "2024-06-01"}},
		{"bad active_since", service.UsageRequest{
			From: "2024-06-01", To: "2024-06-02", ActiveSince: "not-a-ts",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := service.BuildUsageFilter(tc.req)
			require.Error(t, err)
			var ue *service.UsageInputError
			assert.True(t, errors.As(err, &ue),
				"want UsageInputError, got %T", err)
		})
	}
}

func TestDirectBackend_UsageSummary_InvalidInput(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil)

	_, err := be.UsageSummary(context.Background(), service.UsageRequest{
		Timezone: "Fake/Zone",
	})
	require.Error(t, err)
	var ue *service.UsageInputError
	assert.True(t, errors.As(err, &ue), "want UsageInputError, got %T", err)
}

func TestDirectBackend_UsageSummary_UnknownProjectKeyHasStableCode(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil)

	_, err := be.UsageSummary(context.Background(), service.UsageRequest{
		ExcludeProjectKey: "pl1:sha256:stale",
	})
	require.Error(t, err)
	var inputErr *service.UsageInputError
	require.ErrorAs(t, err, &inputErr)
	assert.Equal(t, service.UsageErrorCodeUnknownProjectKey, inputErr.Code)
}

func TestDirectBackend_UsageSummary_EmptyRange(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil)

	res, err := be.UsageSummary(context.Background(), service.UsageRequest{
		From: "2024-06-01", To: "2024-06-03",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "2024-06-01", res.From)
	assert.Equal(t, "2024-06-03", res.To)
	assert.NotNil(t, res.ProjectTotals, "folds should be non-nil slices")
}

func TestHTTPBackend_UsageSummary_Roundtrip(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	svc := env.Backend("", false)

	res, err := svc.UsageSummary(context.Background(), service.UsageRequest{
		From: "2024-06-01", To: "2024-06-03",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "2024-06-01", res.From)
	assert.Equal(t, "2024-06-03", res.To)
}

// The server defaults include_one_shot to true, so the HTTP backend must
// always send it explicitly to faithfully transmit a false value.
func TestHTTPBackend_UsageSummary_SendsExplicitIncludeOneShot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		includeOneShot bool
		want           string
	}{
		{"false", false, "false"},
		{"true", true, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					got = r.URL.Query().Get("include_one_shot")
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"from":"x","to":"y"}`))
				}))
			t.Cleanup(srv.Close)
			svc := service.NewHTTPBackend(srv.URL, "", false)

			_, err := svc.UsageSummary(context.Background(), service.UsageRequest{
				From: "2024-06-01", To: "2024-06-02",
				IncludeOneShot: tc.includeOneShot,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A read-only daemon (pg serve) returns 501 for usage; the HTTP backend
// maps that to the shared db.ErrReadOnly sentinel.
func TestHTTPBackend_UsageSummary_ReadOnly(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
		}))
	t.Cleanup(srv.Close)
	svc := service.NewHTTPBackend(srv.URL, "", true)

	_, err := svc.UsageSummary(context.Background(), service.UsageRequest{
		From: "2024-06-01", To: "2024-06-02",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrReadOnly),
		"501 should map to db.ErrReadOnly, got %v", err)
}

func TestBuildUsagePairwiseFilters_Validation(t *testing.T) {
	t.Parallel()

	_, _, _, _, err := service.BuildUsagePairwiseFilters(
		service.UsagePairwiseComparisonRequest{
			UsageRequest:   service.UsageRequest{From: "2024-06-01", To: "2024-06-02"},
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "machine",
			RightValue:     "host-a",
		},
	)
	require.Error(t, err)
	var ue *service.UsageInputError
	assert.True(t, errors.As(err, &ue))
	assert.Equal(t, "right_dimension must be model or project", ue.Msg)
}

func TestBuildUsagePairwiseFilters_PreservesNonComparedBaseFilters(t *testing.T) {
	t.Parallel()

	left, leftEmpty, right, rightEmpty, err := service.BuildUsagePairwiseFilters(
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:     "2024-06-01",
				To:       "2024-06-03",
				Timezone: "UTC",
				Model:    "model-a,model-b",
				Project:  "alpha,beta",
			},
			LeftDimension:  "model",
			LeftValue:      "model-b",
			RightDimension: "project",
			RightValue:     "beta",
		},
	)
	require.NoError(t, err)
	assert.False(t, leftEmpty)
	assert.False(t, rightEmpty)
	assert.Equal(t, "model-b", left.Model)
	assert.Equal(t, "alpha,beta", left.Project)
	assert.Equal(t, "model-a,model-b", right.Model)
	assert.Equal(t, "beta", right.Project)
}

func TestBuildUsagePairwiseFilters_ConflictingBaseFilterMarksEmptySide(t *testing.T) {
	t.Parallel()

	left, leftEmpty, right, rightEmpty, err := service.BuildUsagePairwiseFilters(
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:  "2024-06-01",
				To:    "2024-06-03",
				Model: "gpt-4o",
			},
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	assert.True(t, leftEmpty)
	assert.False(t, rightEmpty)
	assert.Empty(t, left.Model)
	assert.Equal(t, "gpt-4o", right.Model)
}

func TestDirectBackend_UsagePairwiseComparison_ModelVsModel(t *testing.T) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedPairwiseUsageFixture(t, d)
	be := service.NewDirectBackend(d, nil)

	res, err := be.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:           "2024-06-01",
				To:             "2024-06-01",
				Timezone:       "UTC",
				IncludeOneShot: true,
			},
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 2, res.Left.SessionCount)
	assert.Equal(t, 285, res.Left.TotalTokens)
	assert.Equal(t, 1, res.Right.SessionCount)
	assert.Equal(t, 50, res.Right.TotalTokens)
	assert.Equal(t, -235, res.Deltas.TotalTokensDelta)
	require.NotNil(t, res.Left.CostPerSession)
	require.NotNil(t, res.Right.CostPerSession)
}

func TestDirectBackend_UsagePairwiseComparison_ProjectVsModel(t *testing.T) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedPairwiseUsageFixture(t, d)
	be := service.NewDirectBackend(d, nil)
	summary, err := be.UsageSummary(context.Background(), service.UsageRequest{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		IncludeOneShot: true,
	})
	require.NoError(t, err)
	var betaKey string
	for _, project := range summary.ProjectTotals {
		if project.Project == "beta" {
			betaKey = project.ProjectKey
		}
	}
	require.NotEmpty(t, betaKey)

	res, err := be.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:           "2024-06-01",
				To:             "2024-06-01",
				Timezone:       "UTC",
				IncludeOneShot: true,
			},
			LeftDimension:  "project",
			LeftValue:      betaKey,
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 2, res.Left.SessionCount)
	assert.Equal(t, 155, res.Left.TotalTokens)
	assert.Equal(t, 1, res.Right.SessionCount)
	assert.Equal(t, 50, res.Right.TotalTokens)
}

func TestDirectBackend_UsagePairwiseComparison_PreservesCommaProjectLabel(
	t *testing.T,
) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedCommaProjectUsageFixture(t, d)
	be := service.NewDirectBackend(d, nil)
	base := service.UsageRequest{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		IncludeOneShot: true,
	}
	summary, err := be.UsageSummary(context.Background(), base)
	require.NoError(t, err)
	keys := make(map[string]string)
	for _, project := range summary.ProjectTotals {
		keys[project.Project] = project.ProjectKey
	}
	require.NotEmpty(t, keys["team,core"])
	require.NotEmpty(t, keys["other"])

	comparison, err := be.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest:   base,
			LeftDimension:  "project",
			LeftValue:      keys["team,core"],
			RightDimension: "project",
			RightValue:     keys["other"],
		},
	)
	require.NoError(t, err)
	require.NotNil(t, comparison)
	assert.Equal(t, 1, comparison.Left.SessionCount)
	assert.Equal(t, 100, comparison.Left.TotalTokens)
	assert.Equal(t, 1, comparison.Right.SessionCount)
	assert.Equal(t, 25, comparison.Right.TotalTokens)
}

func TestDirectBackend_UsageSummary_ExcludesOpaqueProjectKey(t *testing.T) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedPairwiseUsageFixture(t, d)
	be := service.NewDirectBackend(d, nil)
	base := service.UsageRequest{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		IncludeOneShot: true,
	}
	summary, err := be.UsageSummary(context.Background(), base)
	require.NoError(t, err)
	var betaKey string
	for _, project := range summary.ProjectTotals {
		if project.Project == "beta" {
			betaKey = project.ProjectKey
		}
	}
	require.NotEmpty(t, betaKey)

	base.ExcludeProjectKey = betaKey
	filtered, err := be.UsageSummary(context.Background(), base)
	require.NoError(t, err)
	require.Len(t, filtered.ProjectTotals, 1)
	assert.Equal(t, "alpha", filtered.ProjectTotals[0].Project)
}

func TestDirectBackend_UsageSummary_ExcludesSubagentOnlyProjectKey(t *testing.T) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	started := "2024-06-01T09:00:00Z"
	msg := dbtest.AsstMsg("usage-subagent", 1, "done")
	msg.Timestamp = started
	msg.Model = "test-model"
	msg.TokenUsage = []byte(`{"input_tokens":100}`)
	dbtest.SeedSessionWithMessages(
		t,
		d,
		"usage-subagent",
		"subagent-only",
		[]db.Message{
			dbtest.UserMsg("usage-subagent", 0, "compare usage"),
			msg,
		},
		dbtest.WithMessageCounts(2, 1),
		func(s *db.Session) {
			s.Agent = "claude"
			s.RelationshipType = "subagent"
			s.StartedAt = &started
			s.EndedAt = &started
		},
	)
	be := service.NewDirectBackend(d, nil)
	base := service.UsageRequest{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		IncludeOneShot: true,
	}

	summary, err := be.UsageSummary(context.Background(), base)
	require.NoError(t, err)
	require.Len(t, summary.ProjectTotals, 1)
	require.NotEmpty(t, summary.ProjectTotals[0].ProjectKey)
	assert.Equal(t, "subagent-only", summary.ProjectTotals[0].Project)

	base.ExcludeProjectKey = summary.ProjectTotals[0].ProjectKey
	filtered, err := be.UsageSummary(context.Background(), base)
	require.NoError(t, err)
	assert.Empty(t, filtered.ProjectTotals)
	assert.Zero(t, filtered.Totals.InputTokens)
}

func TestDirectBackend_UsageSummary_ExcludesCommaProjectByOpaqueKey(
	t *testing.T,
) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedCommaProjectUsageFixture(t, d)
	be := service.NewDirectBackend(d, nil)
	base := service.UsageRequest{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		IncludeOneShot: true,
	}
	summary, err := be.UsageSummary(context.Background(), base)
	require.NoError(t, err)
	var commaKey string
	for _, project := range summary.ProjectTotals {
		if project.Project == "team,core" {
			commaKey = project.ProjectKey
		}
	}
	require.NotEmpty(t, commaKey)

	base.ExcludeProjectKey = commaKey
	filtered, err := be.UsageSummary(context.Background(), base)
	require.NoError(t, err)
	require.Len(t, filtered.ProjectTotals, 1)
	assert.Equal(t, "other", filtered.ProjectTotals[0].Project)
	assert.Equal(t, 25, filtered.ProjectTotals[0].InputTokens)
}

func TestDirectBackend_UsagePairwiseComparison_ZeroDataSide(t *testing.T) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedPairwiseUsageFixture(t, d)
	be := service.NewDirectBackend(d, nil)

	res, err := be.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:           "2024-06-01",
				To:             "2024-06-01",
				Timezone:       "UTC",
				IncludeOneShot: true,
			},
			LeftDimension:  "project",
			LeftValue:      "missing-project",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Zero(t, res.Left.SessionCount)
	assert.Zero(t, res.Left.TotalTokens)
	assert.Nil(t, res.Left.CostPerSession)
	assert.Nil(t, res.Deltas.TotalCostDeltaRatio)
}

func TestDirectBackend_UsagePairwiseComparison_ConflictingBaseFilterReturnsZeroData(t *testing.T) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedPairwiseUsageFixture(t, d)
	be := service.NewDirectBackend(d, nil)

	res, err := be.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:           "2024-06-01",
				To:             "2024-06-01",
				Timezone:       "UTC",
				Model:          "gpt-4o",
				IncludeOneShot: true,
			},
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Zero(t, res.Left.SessionCount)
	assert.Zero(t, res.Left.TotalTokens)
	assert.Equal(t, 1, res.Right.SessionCount)
	assert.Equal(t, 50, res.Right.TotalTokens)
}

func TestDirectBackend_UsagePairwiseComparison_DoesNotUseSentinelFilterValues(t *testing.T) {
	t.Parallel()

	d := dbtest.OpenTestDB(t)
	seedPairwiseUsageFixture(t, d)
	started := "2024-06-01T12:00:00Z"
	dbtest.SeedSessionWithMessages(
		t,
		d,
		"usage-sentinel-model",
		"sentinel-project",
		[]db.Message{
			dbtest.UserMsg("usage-sentinel-model", 0, "compare usage"),
			assistantUsageMsg("usage-sentinel-model", 1, struct {
				id      string
				project string
				started string
				model   string
				input   int
				output  int
				create  int
				read    int
			}{
				id:      "usage-sentinel-model",
				project: "sentinel-project",
				started: started,
				model:   "__agentsview_no_match__",
				input:   999,
				output:  999,
				create:  0,
				read:    0,
			}),
		},
		dbtest.WithMessageCounts(2, 1),
		func(s *db.Session) {
			s.Agent = "claude"
			s.StartedAt = &started
			s.EndedAt = &started
		},
	)
	be := service.NewDirectBackend(d, nil)

	res, err := be.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:           "2024-06-01",
				To:             "2024-06-01",
				Timezone:       "UTC",
				Model:          "gpt-4o",
				IncludeOneShot: true,
			},
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Zero(t, res.Left.SessionCount)
	assert.Zero(t, res.Left.TotalTokens)
	assert.Equal(t, 1, res.Right.SessionCount)
	assert.Equal(t, 50, res.Right.TotalTokens)
}

func TestHTTPBackend_UsagePairwiseComparison_Roundtrip(t *testing.T) {
	t.Parallel()

	env := newHTTPBackendEnv(t)
	seedPairwiseUsageFixture(t, env.DB)
	svc := env.Backend("", false)

	res, err := svc.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				From:           "2024-06-01",
				To:             "2024-06-01",
				Timezone:       "UTC",
				IncludeOneShot: true,
			},
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 2, res.Left.SessionCount)
	assert.Equal(t, 1, res.Right.SessionCount)
}

func TestHTTPBackend_UsagePairwiseComparison_SerializesRequest(t *testing.T) {
	t.Parallel()

	var queryValues map[string]string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			queryValues = map[string]string{
				"left_dimension":      r.URL.Query().Get("left_dimension"),
				"left_value":          r.URL.Query().Get("left_value"),
				"right_dimension":     r.URL.Query().Get("right_dimension"),
				"right_value":         r.URL.Query().Get("right_value"),
				"git_branch":          r.URL.Query().Get("git_branch"),
				"exclude_project_key": r.URL.Query().Get("exclude_project_key"),
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"left":{"sessionCount":1,"totalTokens":12},"right":{"sessionCount":2,"totalTokens":34},"deltas":{"sessionCountDelta":1,"totalTokensDelta":22}}`))
		},
	))
	t.Cleanup(srv.Close)
	svc := service.NewHTTPBackend(srv.URL, "", false)

	res, err := svc.UsagePairwiseComparison(
		context.Background(),
		service.UsagePairwiseComparisonRequest{
			UsageRequest: service.UsageRequest{
				GitBranch:         "alpha/main",
				ExcludeProjectKey: "pl1:sha256:hidden",
			},
			LeftDimension:  "project",
			LeftValue:      "alpha",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "project", queryValues["left_dimension"])
	assert.Equal(t, "alpha", queryValues["left_value"])
	assert.Equal(t, "model", queryValues["right_dimension"])
	assert.Equal(t, "gpt-4o", queryValues["right_value"])
	assert.Equal(t, "alpha/main", queryValues["git_branch"])
	assert.Equal(t, "pl1:sha256:hidden", queryValues["exclude_project_key"])
	assert.Equal(t, 22, res.Deltas.TotalTokensDelta)
}
