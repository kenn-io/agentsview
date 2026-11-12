package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// codexRateLimitWindow is a Codex RateLimitWindow as rollouts record it.
type codexRateLimitWindow struct {
	used    float64
	minutes int64
	resets  int64
}

// codexRateLimitLine builds a token_count event carrying a rate_limits
// snapshot shaped like the ones Codex writes: limit_id, plan_type, primary
// and secondary windows (nil for null), and the credits block.
func codexRateLimitLine(t *testing.T, ts, limitID, plan string, primary, secondary *codexRateLimitWindow) string {
	t.Helper()
	window := func(w *codexRateLimitWindow) any {
		if w == nil {
			return nil
		}
		return map[string]any{"used_percent": w.used, "window_minutes": w.minutes, "resets_at": w.resets}
	}
	return mustCodexLine(t, map[string]any{"type": "event_msg", "timestamp": ts, "payload": map[string]any{
		"type": "token_count",
		"info": nil,
		"rate_limits": map[string]any{
			"limit_id": limitID, "limit_name": nil,
			"primary": window(primary), "secondary": window(secondary),
			"credits":          map[string]any{"has_credits": true, "unlimited": false, "balance": "10"},
			"plan_type":        plan,
			"individual_limit": nil, "spend_control_reached": nil, "rate_limit_reached_type": nil,
		},
	}})
}

func codexAccountMetaLine(id, account, ts string) string {
	return testjsonl.CodexSessionMetaWithFieldsJSON(id, "/tmp", "codex_cli_rs", ts, map[string]any{
		"creator_account_id": account,
	})
}

func TestCodexRateLimitsRecordChangesOnly(t *testing.T) {
	weekly := &codexRateLimitWindow{used: 12, minutes: 10080, resets: 1794890601}
	weeklyLater := &codexRateLimitWindow{used: 13, minutes: 10080, resets: 1794890601}
	fiveHour := &codexRateLimitWindow{used: 40, minutes: 300, resets: 1794800000}
	content := testjsonl.JoinJSONL(
		codexAccountMetaLine("rl-full", "account-a", tsEarly),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "go", tsEarlyS1),
		codexRateLimitLine(t, "2024-01-01T10:00:05Z", "codex", "pro", fiveHour, weekly),
		codexRateLimitLine(t, "2024-01-01T10:00:06Z", "codex", "pro", fiveHour, weekly),
		// A report without any window carries nothing to record.
		codexRateLimitLine(t, "2024-01-01T10:00:07Z", "premium", "", nil, nil),
		codexRateLimitLine(t, "2024-01-01T10:00:08Z", "codex", "pro", fiveHour, weeklyLater),
	)
	sess, _ := runCodexParserTest(t, "test.jsonl", content, false)

	require.Len(t, sess.RateLimits, 2, "the repeat and the windowless report add nothing")
	first, second := sess.RateLimits[0], sess.RateLimits[1]
	assert.Equal(t, time.Date(2024, 1, 1, 10, 0, 5, 0, time.UTC), first.ObservedAt)
	assert.Equal(t, rateLimitAccountKey("account-a"), first.AccountKey)
	assert.NotContains(t, first.AccountKey, "account-a", "the raw account ID is not kept")
	assert.Equal(t, "codex", first.LimitID)
	assert.Equal(t, "pro", first.PlanType)
	require.NotNil(t, first.Primary)
	assert.InDelta(t, 40.0, first.Primary.UsedPercent, 0)
	require.NotNil(t, first.Primary.WindowMinutes)
	assert.Equal(t, int64(300), *first.Primary.WindowMinutes)
	require.NotNil(t, first.Primary.ResetsAt)
	assert.Equal(t, time.Unix(1794800000, 0).UTC(), *first.Primary.ResetsAt)
	require.NotNil(t, first.Secondary)
	assert.Equal(t, int64(10080), *first.Secondary.WindowMinutes)

	assert.Equal(t, time.Date(2024, 1, 1, 10, 0, 8, 0, time.UTC), second.ObservedAt)
	require.NotNil(t, second.Secondary)
	assert.InDelta(t, 13.0, second.Secondary.UsedPercent, 0)
}

func TestCodexRateLimitsKeepAbsentFields(t *testing.T) {
	line := mustCodexLine(t, map[string]any{"type": "event_msg", "timestamp": "2024-01-01T10:00:05Z", "payload": map[string]any{
		"type": "token_count",
		"rate_limits": map[string]any{
			"limit_id": "codex",
			"primary":  map[string]any{"used_percent": 7.5, "window_minutes": nil, "resets_at": nil},
		},
	}})
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("rl-sparse", "/tmp", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "go", tsEarlyS1),
		line,
	)
	sess, _ := runCodexParserTest(t, "test.jsonl", content, false)

	require.Len(t, sess.RateLimits, 1)
	snap := sess.RateLimits[0]
	assert.Empty(t, snap.AccountKey, "a session without an account keeps an empty key")
	assert.Empty(t, snap.PlanType)
	assert.Nil(t, snap.Secondary)
	require.NotNil(t, snap.Primary)
	assert.InDelta(t, 7.5, snap.Primary.UsedPercent, 0)
	assert.Nil(t, snap.Primary.WindowMinutes)
	assert.Nil(t, snap.Primary.ResetsAt)
}

func TestCodexRateLimitsOnlyForCodex(t *testing.T) {
	b := newCodexSessionBuilder(AgentTraeX, false, nil, NewCodexCollectingSink(0))
	_, err := b.processLine(t.Context(), codexRateLimitLine(t, tsEarlyS5, "codex", "pro",
		&codexRateLimitWindow{used: 1, minutes: 300, resets: 1794800000}, nil))
	require.NoError(t, err)
	assert.Empty(t, b.rateLimits, "forks sharing the parser are not checked against their producers")
}

func TestCodexRateLimitsIncrementalMatchesFullParse(t *testing.T) {
	weekly := &codexRateLimitWindow{used: 12, minutes: 10080, resets: 1794890601}
	weeklyLater := &codexRateLimitWindow{used: 14, minutes: 10080, resets: 1794890601}
	for _, tc := range []struct {
		name     string
		withSeed bool
	}{
		{name: "persisted cursor", withSeed: true},
		{name: "prefix rescan", withSeed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
			initial := testjsonl.JoinJSONL(
				codexAccountMetaLine(uuid, "account-b", tsEarly),
				testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
				testjsonl.CodexMsgJSON("user", "first", "2024-01-01T10:00:02Z"),
				codexRateLimitLine(t, "2024-01-01T10:00:03Z", "codex", "pro", weekly, nil),
				codexEventMsgJSON("task_complete", "2024-01-01T10:00:04Z"),
			)
			path := writeCodexProviderSessionContent(t, root, uuid, initial)
			provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source := requireCodexProviderSource(t, provider, uuid)
			initialFingerprint, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			full, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: initialFingerprint})
			require.NoError(t, err)
			require.Len(t, full.Results, 1)
			require.Len(t, full.Results[0].Result.Session.RateLimits, 1)

			appendCodexProviderContent(t, path, testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("user", "second", "2024-01-01T10:00:05Z"),
				// Repeats the report the committed prefix already recorded.
				codexRateLimitLine(t, "2024-01-01T10:00:06Z", "codex", "pro", weekly, nil),
				codexRateLimitLine(t, "2024-01-01T10:00:07Z", "codex", "pro", weeklyLater, nil),
			))
			currentFingerprint, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			req := IncrementalRequest{
				Source: source, Fingerprint: currentFingerprint,
				SessionID: "codex:" + uuid, Offset: initialFingerprint.Size, StartOrdinal: 1,
			}
			if tc.withSeed {
				req.Seed = full.Results[0].Result.Checkpoint
				require.NotEmpty(t, req.Seed)
			}
			// A fresh provider has no warm cursor cache, so the tail parse
			// resumes only from the seed or a prefix rescan.
			fresh, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			outcome, status, err := fresh.ParseIncremental(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, IncrementalApplied, status)

			reparsed, err := fresh.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: currentFingerprint})
			require.NoError(t, err)
			require.Len(t, reparsed.Results, 1)
			assert.Equal(t, reparsed.Results[0].Result.Session.RateLimits[1:], outcome.RateLimits,
				"the tail yields exactly the reports a full parse adds after the prefix")
			require.Len(t, outcome.RateLimits, 1)
			assert.Equal(t, rateLimitAccountKey("account-b"), outcome.RateLimits[0].AccountKey)
		})
	}
}

// A fork replays its parent's lines, restamped at fork creation. With the
// parent readable, the replay is suppressed and only the fork's own reports
// count; with the parent missing, the replay cannot be told apart, so every
// report waits for the reparse that follows once the parent appears.
func TestCodexRateLimitsForkReplay(t *testing.T) {
	forkCreatedMs := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	forkID := testUUIDv7(forkCreatedMs, 1)
	parentID := testUUIDv7(forkCreatedMs-7200_000, 0)
	parentTurnID := testUUIDv7(forkCreatedMs-3600_000, 2)
	genuineTurnID := testUUIDv7(forkCreatedMs+1000, 3)
	stale := &codexRateLimitWindow{used: 90, minutes: 300, resets: 1704100000}
	fresh := &codexRateLimitWindow{used: 5, minutes: 300, resets: 1704110000}
	fork := testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(forkID, parentID, "/tmp", "user", tsEarly),
		testjsonl.CodexSessionMetaJSON(parentID, "/tmp", "user", tsEarly),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", parentTurnID, tsEarly),
		testjsonl.CodexMsgJSON("user", "replayed question", tsEarly),
		codexRateLimitLine(t, tsEarly, "codex", "pro", stale, nil),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.5", genuineTurnID, tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "genuine question", tsEarlyS1),
		codexRateLimitLine(t, tsEarlyS5, "codex", "pro", fresh, nil),
	)
	parse := func(t *testing.T, root string) *ParsedSession {
		t.Helper()
		path := filepath.Join(root, "rollout-2024-01-01T10-00-00-"+forkID+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(fork), 0o600))
		sess, _, err := newCodexTestProvider(t, root).parseSession(path, "local", false)
		require.NoError(t, err)
		require.NotNil(t, sess)
		return sess
	}

	t.Run("parent readable", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(root, "rollout-2024-01-01T08-00-00-"+parentID+".jsonl"),
			[]byte(testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parentID, "/tmp", "user", tsEarly),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", parentTurnID, tsEarly),
			)), 0o600,
		))
		sess := parse(t, root)
		require.Len(t, sess.RateLimits, 1)
		require.NotNil(t, sess.RateLimits[0].Primary)
		assert.InDelta(t, 5.0, sess.RateLimits[0].Primary.UsedPercent, 0)
	})
	t.Run("parent missing", func(t *testing.T) {
		sess := parse(t, t.TempDir())
		assert.Empty(t, sess.RateLimits)
	})
}
