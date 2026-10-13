package codexcloud

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
)

func usageLogin(accountID string) Login {
	return Login{Home: "/homes/a", Token: NewToken("usage-token-1"), AccountID: accountID, Key: passKey}
}

func TestQueryUsageRequestShape(t *testing.T) {
	asOf := time.Date(2027, 1, 15, 11, 30, 0, 0, time.UTC)
	usage := codexcloudtest.NewUsageHost(t, asOf)
	weekly, credits := 1.25, "0.5"
	usage.SetRow("root-a", codexcloudtest.Row("root-a", "available", &weekly, &credits))

	got, err := QueryUsage(t.Context(), nil, usage.URL(), usageLogin("acct-1"), "1.2.3", []UsageGroup{
		{ThreadID: "root-a", CreatedAt: time.Unix(1800000000, 0), Descendants: []string{"child-1", "child-2"}},
		{ThreadID: "root-b", CreatedAt: time.Unix(1800003600, 0)},
	})
	require.NoError(t, err)

	requests := usage.Requests()
	require.Len(t, requests, 1)
	h := requests[0].Header
	assert.Equal(t, "Bearer usage-token-1", h.Get("Authorization"))
	assert.Equal(t, "application/json", h.Get("Content-Type"))
	assert.Equal(t, "agentsview/1.2.3", h.Get("User-Agent"))
	assert.Equal(t, "agentsview", h.Get("originator"))
	assert.Equal(t, "acct-1", h.Get("ChatGPT-Account-Id"))
	assert.Equal(t, []codexcloudtest.UsageRequestGroup{
		{ThreadID: "root-a", CreatedAt: "2027-01-15T08:00:00Z", Descendants: []string{"child-1", "child-2"}},
		{ThreadID: "root-b", CreatedAt: "2027-01-15T09:00:00Z", Descendants: []string{}},
	}, requests[0].Threads)

	assert.Equal(t, asOf, got.DataAsOf)
	require.Len(t, got.Threads, 1, "a root the report leaves out has no row")
	row := got.Threads[0]
	assert.Equal(t, "root-a", row.ThreadID)
	assert.Equal(t, "available", row.DataStatus)
	assert.Equal(t, "included_plan", row.UsageSource)
	require.NotNil(t, row.WeeklyPercent)
	assert.InDelta(t, 1.25, *row.WeeklyPercent, 0)
	assert.Nil(t, row.FiveHourPercent)
	require.Len(t, row.Groups, 1)
	assert.Equal(t, "gpt-a", row.Groups[0].Model)
	assert.Equal(t, "high", row.Groups[0].ReasoningEffort)

	_, err = QueryUsage(t.Context(), nil, usage.URL(), usageLogin(""), "1.2.3", []UsageGroup{{ThreadID: "root-a"}})
	require.NoError(t, err)
	assert.NotContains(t, usage.Requests()[1].Header, "Chatgpt-Account-Id", "no account header without an account ID")
}

func TestQueryUsageNoRedirect(t *testing.T) {
	followed := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer other.Close()
	usage := codexcloudtest.NewUsageHost(t, fetchNow)
	usage.Refuse(http.StatusFound, http.Header{"Location": {other.URL}}, "")

	_, err := QueryUsage(t.Context(), nil, usage.URL(), usageLogin(""), "1", []UsageGroup{{ThreadID: "a"}})

	httpErr, ok := errors.AsType[*HTTPError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, http.StatusFound, httpErr.Status)
	assert.False(t, followed, "the bearer token never follows a redirect")
	assert.Equal(t, FailProtocol, FailureFor(err, usageLogin("")))
}

func TestQueryUsageCreditsExponent(t *testing.T) {
	usage := codexcloudtest.NewUsageHost(t, fetchNow)
	zero := "0E-10"
	usage.SetRow("a", codexcloudtest.Row("a", "available", nil, &zero))

	got, err := QueryUsage(t.Context(), nil, usage.URL(), usageLogin(""), "1", []UsageGroup{{ThreadID: "a"}})
	require.NoError(t, err)
	require.NotNil(t, got.Threads[0].Credits)
	assert.Equal(t, "0E-10", *got.Threads[0].Credits, "credits stay as sent")

	bad := "lots"
	usage.SetRow("a", codexcloudtest.Row("a", "available", nil, &bad))
	_, err = QueryUsage(t.Context(), nil, usage.URL(), usageLogin(""), "1", []UsageGroup{{ThreadID: "a"}})
	require.ErrorIs(t, err, ErrProtocolChange)
}

func TestQueryUsageCreditsMustBeDecimal(t *testing.T) {
	for _, credits := range []string{"NaN", "Inf", "-Infinity", "0x1p-2", "1e", "."} {
		t.Run(credits, func(t *testing.T) {
			usage := codexcloudtest.NewUsageHost(t, fetchNow)
			usage.SetRow("a", codexcloudtest.Row("a", "available", nil, &credits))

			_, err := QueryUsage(t.Context(), nil, usage.URL(), usageLogin(""), "1", []UsageGroup{{ThreadID: "a"}})

			require.ErrorIs(t, err, ErrProtocolChange)
		})
	}
}

func TestQueryUsageBadCreditsErrorIsBounded(t *testing.T) {
	credits := strings.Repeat("x", 10_000)
	usage := codexcloudtest.NewUsageHost(t, fetchNow)
	usage.SetRow("a", codexcloudtest.Row("a", "available", nil, &credits))

	_, err := QueryUsage(t.Context(), nil, usage.URL(), usageLogin(""), "1", []UsageGroup{{ThreadID: "a"}})

	require.ErrorIs(t, err, ErrProtocolChange)
	assert.Contains(t, err.Error(), "are not a decimal number")
	assert.Less(t, len(err.Error()), 200, "the log line quotes only the start of the value")
}

func TestQueryUsageErrorsHideToken(t *testing.T) {
	usage := codexcloudtest.NewUsageHost(t, fetchNow)
	usage.Refuse(http.StatusUnauthorized, http.Header{"X-Echo": {"usage-token-1"}}, "bad token usage-token-1")

	_, err := QueryUsage(t.Context(), nil, usage.URL(), usageLogin(""), "1", []UsageGroup{{ThreadID: "a"}})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "usage-token-1")
	assert.Equal(t, FailSignedOut, FailureFor(err, usageLogin("")))
}
