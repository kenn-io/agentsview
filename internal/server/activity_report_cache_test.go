package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestActivityReportCacheExpiresWithoutAnotherCacheOperation(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil)
	srv.activityReports.idle = 50 * time.Millisecond
	require.True(t, srv.activityReports.put(
		"report", "digest", activity.CandidateArtifacts{
			Sessions: []activity.SessionRow{{SessionID: "retained"}},
		},
	))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
		require.ErrorIs(t, <-serveDone, http.ErrServerClosed)
	})

	require.Eventually(t, func() bool {
		srv.activityReports.mu.Lock()
		defer srv.activityReports.mu.Unlock()
		return len(srv.activityReports.entries) == 0 &&
			srv.activityReports.rows == 0 && srv.activityReports.bytes == 0
	}, time.Second, 10*time.Millisecond,
		"idle artifacts must be released without a later cache request")
}

func TestActivityReportCacheSlidesIdleExpiry(t *testing.T) {
	cache := newActivityReportCache()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	require.True(t, cache.put("report", "digest", activity.CandidateArtifacts{}))
	now = now.Add(14 * time.Minute)
	_, _, ok := cache.get("report")
	require.True(t, ok)
	now = now.Add(14 * time.Minute)
	_, _, ok = cache.get("report")
	require.True(t, ok, "successful access slides the idle deadline")
	now = now.Add(15 * time.Minute)
	_, _, ok = cache.get("report")
	assert.False(t, ok)
}

func TestActivityReportCacheEvictsLRUWithoutStub(t *testing.T) {
	cache := newActivityReportCache()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	for index := range activityReportCacheEntries + 1 {
		id := fmt.Sprintf("report-%d", index)
		require.True(t, cache.put(id, id, activity.CandidateArtifacts{}))
		now = now.Add(time.Second)
	}
	_, _, ok := cache.get("report-0")
	assert.False(t, ok)
	_, _, ok = cache.get("report-3")
	assert.True(t, ok)
}

func TestActivityReportCacheRejectsSingleOversizeArtifact(t *testing.T) {
	cache := newActivityReportCache()
	rows := make([]activity.SessionRow, activityReportCacheRows+1)
	assert.False(t, cache.put("oversize", "digest", activity.CandidateArtifacts{
		Sessions: rows,
	}))
	_, _, ok := cache.get("oversize")
	assert.False(t, ok)
}

func TestActivityReportCacheEnforcesCumulativeRowAndByteBounds(t *testing.T) {
	cache := newActivityReportCache()
	cache.maxRows = 3
	cache.maxBytes = 1 << 20
	report := func(id, title string) activity.CandidateArtifacts {
		return activity.CandidateArtifacts{Sessions: []activity.SessionRow{{
			SessionID: id, Title: title,
		}}}
	}
	require.True(t, cache.put("one", "one", report("one", "")))
	require.True(t, cache.put("two", "two", report("two", "")))
	require.True(t, cache.put("three", "three", report("three", "")))
	require.Equal(t, 3, cache.rows)
	require.True(t, cache.put("four", "four", report("four", "")))
	_, _, oldestPresent := cache.get("one")
	assert.False(t, oldestPresent, "cumulative row pressure evicts the LRU")

	cache = newActivityReportCache()
	small := report("small", "")
	replacement := report(
		"replacement", "a retained string that pushes the total over the bound",
	)
	cache.maxBytes = max(
		activity.EstimatedArtifactBytes(small),
		activity.EstimatedArtifactBytes(replacement),
	) + 1
	require.True(t, cache.put("small", "small", small))
	require.True(t, cache.put("replacement", "replacement", replacement))
	_, _, oldestPresent = cache.get("small")
	assert.False(t, oldestPresent, "cumulative byte pressure evicts the LRU")
	assert.LessOrEqual(t, cache.bytes, cache.maxBytes)
}

func TestActivityReportCacheAccountsAndReusesLazySortOrder(t *testing.T) {
	cache := newActivityReportCache()
	artifacts := activity.CandidateArtifacts{Sessions: []activity.SessionRow{
		{SessionID: "b", Project: "two"},
		{SessionID: "a", Project: "one"},
	}}
	require.True(t, cache.put("report", "digest", artifacts))
	baseBytes := cache.bytes
	options := activity.SessionPageOptions{
		Sort: activity.SessionSortProject, Direction: "asc",
	}
	first, ok, err := cache.page("report", options)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, first.Sessions, 2)
	assert.Equal(t, "a", first.Sessions[0].SessionID)
	assert.Equal(t, baseBytes+16, cache.bytes)

	_, ok, err = cache.page("report", options)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, baseBytes+16, cache.bytes, "same permutation is retained once")
}
