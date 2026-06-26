package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

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
