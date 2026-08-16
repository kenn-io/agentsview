package sync_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestLiveActivityPollerRefreshesOpenCodexActivityAndUsage(t *testing.T) {
	const (
		uuid      = "019f0000-0000-7000-8000-000000000003"
		sessionID = "codex:" + uuid
	)
	now := time.Date(2026, 7, 29, 15, 30, 0, 0, time.UTC)
	firstUser := now.Add(-35 * time.Minute)
	firstAssistant := now.Add(-34 * time.Minute)
	secondUser := now.Add(-12 * time.Minute)
	secondAssistant := now.Add(-11 * time.Minute)
	thirdUser := now.Add(-4 * time.Minute)
	thirdAssistant := now.Add(-3 * time.Minute)

	base := t.TempDir()
	sessions := filepath.Join(base, "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	env := setupSingleAgentTestEnvWithDirs(
		t, parser.AgentCodex, []string{sessions},
	)
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "user", firstUser.Format(time.RFC3339),
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", firstUser.Format(time.RFC3339),
		),
		testjsonl.CodexMsgJSON(
			"user", "first", firstUser.Format(time.RFC3339),
		),
		testjsonl.CodexMsgJSON(
			"assistant", "answer", firstAssistant.Format(time.RFC3339),
		),
		testjsonl.CodexTokenCountJSON(
			firstAssistant.Format(time.RFC3339), 1_000, 100, 400,
		),
	)
	rollout := env.writeCodexSession(
		t,
		filepath.Join("2026", "07", "29"),
		"rollout-2026-07-29T14-55-00-"+uuid+".jsonl",
		initial,
	)
	initialMTime := firstUser.Add(-time.Hour)
	require.NoError(t, os.Chtimes(rollout, initialMTime, initialMTime))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{rollout}))

	before, err := env.db.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.FileSize)
	require.NotNil(t, before.FileMtime)
	assert.Equal(t, 2, before.MessageCount)
	initialUsage := requireDailyOutputTokens(t, env.db, "2026-07-29")
	assert.Equal(t, 100, initialUsage)

	appendDescriptor, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, appendDescriptor.Close())
	})
	history := filepath.Join(base, "history.jsonl")
	require.NoError(t, os.WriteFile(history, fmt.Appendf(nil,
		`{"session_id":"%s","ts":%d,"text":"private prompt sentinel"}`+"\n",
		uuid, secondUser.Unix(),
	), 0o644))
	_, err = appendDescriptor.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", secondUser.Format(time.RFC3339),
		),
		testjsonl.CodexMsgJSON(
			"user", "second", secondUser.Format(time.RFC3339),
		),
		testjsonl.CodexMsgJSON(
			"assistant", "second answer", secondAssistant.Format(time.RFC3339),
		),
		testjsonl.CodexTokenCountJSON(
			secondAssistant.Format(time.RFC3339), 2_000, 250, 800,
		),
	))
	require.NoError(t, err)

	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
		Roots:   []string{sessions},
		Machine: "local",
	})
	require.True(t, ok)
	hints, supported, err := parser.ResolveActivityHintProvider(provider)
	require.NoError(t, err)
	require.True(t, supported)
	hintSources, err := hints.ActivityHintSources(t.Context())
	require.NoError(t, err)
	poller := agentsync.NewLiveActivityPoller(
		[]agentsync.LiveActivityTarget{{
			Provider: provider,
			Hints:    hints,
			Sources:  hintSources,
		}},
		func(
			ctx context.Context,
			id string,
		) (agentsync.LiveActivitySource, bool, error) {
			session, lookupErr := env.db.GetSessionFull(ctx, id)
			if lookupErr != nil || session == nil || session.FilePath == nil {
				return agentsync.LiveActivitySource{}, false, lookupErr
			}
			source := agentsync.LiveActivitySource{Path: *session.FilePath}
			if session.FileSize != nil && session.FileMtime != nil {
				source.StoredSize = *session.FileSize
				source.StoredMTimeNS = *session.FileMtime
				source.HasStoredStat = true
			}
			if session.FileInode != nil && session.FileDevice != nil {
				source.StoredInode = *session.FileInode
				source.StoredDevice = *session.FileDevice
				source.HasStoredIdentity = true
			}
			return source, true, nil
		},
		env.engine.SyncPathsContext,
		nil,
	)

	stats, err := poller.PollOnce(t.Context(), now)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionLookups)
	assert.Equal(t, 1, stats.SourceStats)
	assert.Equal(t, 1, stats.SyncPaths)

	afterSecond, err := env.db.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, afterSecond)
	require.NotNil(t, afterSecond.FileSize)
	require.NotNil(t, afterSecond.FileMtime)
	assert.Greater(t, *afterSecond.FileSize, *before.FileSize)
	assert.Greater(t, *afterSecond.FileMtime, *before.FileMtime)
	assert.Equal(t, 4, afterSecond.MessageCount)
	require.NotNil(t, afterSecond.EndedAt)
	assert.Equal(t, secondAssistant.Format(time.RFC3339), *afterSecond.EndedAt)
	secondUsage := requireDailyOutputTokens(t, env.db, "2026-07-29")
	assert.Equal(t, 350, secondUsage)
	requireActivityBucketMembership(
		t, env.db, now, sessionID,
		secondUser.Truncate(5*time.Minute),
		secondUser.Truncate(5*time.Minute).Add(5*time.Minute),
	)

	_, err = appendDescriptor.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"user", "third", thirdUser.Format(time.RFC3339),
		),
		testjsonl.CodexMsgJSON(
			"assistant", "third answer", thirdAssistant.Format(time.RFC3339),
		),
		testjsonl.CodexTokenCountJSON(
			thirdAssistant.Format(time.RFC3339), 500, 75, 200,
		),
	))
	require.NoError(t, err)
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute))
	require.NoError(t, err)
	afterThird, err := env.db.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, afterThird)
	require.NotNil(t, afterThird.FileSize)
	assert.Greater(t, *afterThird.FileSize, *afterSecond.FileSize)
	assert.Equal(t, 6, afterThird.MessageCount)
	require.NotNil(t, afterThird.EndedAt)
	assert.Equal(t, thirdAssistant.Format(time.RFC3339), *afterThird.EndedAt)
	thirdUsage := requireDailyOutputTokens(t, env.db, "2026-07-29")
	assert.Equal(t, 425, thirdUsage)
	assert.Greater(t, thirdUsage, secondUsage)
	requireActivityBucketMembership(
		t, env.db, now, sessionID,
		thirdUser.Truncate(5*time.Minute),
		thirdUser.Truncate(5*time.Minute).Add(5*time.Minute),
	)
}

func requireDailyOutputTokens(
	t *testing.T,
	database *db.DB,
	date string,
) int {
	t.Helper()
	daily, err := database.GetDailyUsage(t.Context(), db.UsageFilter{
		From: date, To: date, Timezone: "UTC",
	})
	require.NoError(t, err)
	return daily.Totals.OutputTokens
}

func requireActivityBucketMembership(
	t *testing.T,
	database *db.DB,
	now time.Time,
	sessionID string,
	bucketStart time.Time,
	bucketEnd time.Time,
) {
	t.Helper()
	query, err := activity.ResolveQuery(activity.QueryInput{
		Preset:         "day",
		Date:           now.Format(time.DateOnly),
		Timezone:       "UTC",
		BucketOverride: "5m",
	}, now)
	require.NoError(t, err)
	artifacts, err := database.BuildActivityReportArtifacts(
		t.Context(), db.AnalyticsFilter{Timezone: "UTC"}, query,
		nil,
	)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(artifacts.Sessions, func(row activity.SessionRow) bool {
		return row.SessionID == sessionID
	}), "activity report should include the refreshed session")
	bucket := slices.IndexFunc(artifacts.Report.Buckets, func(slot activity.Bucket) bool {
		start, startErr := time.Parse(time.RFC3339, slot.Start)
		end, endErr := time.Parse(time.RFC3339, slot.End)
		return startErr == nil && endErr == nil &&
			start.Equal(bucketStart) && end.Equal(bucketEnd)
	})
	require.NotEqual(t, -1, bucket, "activity report should contain the target bucket")
	page, err := activity.PageSessions(
		artifacts.Sessions, artifacts.Membership,
		activity.SessionPageOptions{Bucket: &bucket},
	)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(page.Sessions, func(row activity.SessionRow) bool {
		return row.SessionID == sessionID
	}), "activity bucket page should include the refreshed session")
}
