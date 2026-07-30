package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

type liveActivityTestProvider struct {
	parser.ProviderBase
	hintPath        string
	findSourceCalls int
}

func newLiveActivityTestProvider(hintPath string) *liveActivityTestProvider {
	return &liveActivityTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type:     parser.AgentCodex,
				IDPrefix: "codex:",
			},
			Caps: parser.Capabilities{Source: parser.SourceCapabilities{
				ActivityHints: parser.CapabilitySupported,
			}},
		},
		hintPath: hintPath,
	}
}

func (p *liveActivityTestProvider) ActivityHintSources(
	context.Context,
) ([]parser.ActivityHintSource, error) {
	return []parser.ActivityHintSource{{Path: p.hintPath}}, nil
}

func (p *liveActivityTestProvider) DecodeActivityHint(
	line []byte,
) (parser.ActivityHint, bool) {
	return literalActivityHintDecoder{}.DecodeActivityHint(line)
}

func (p *liveActivityTestProvider) FindSource(
	context.Context,
	parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	p.findSourceCalls++
	return parser.SourceRef{}, false, errors.New("FindSource must not be called")
}

func (p *liveActivityTestProvider) Parse(
	context.Context,
	parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

func TestLiveActivityColdResumeAndOngoingAppend(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	require.NoError(t, os.WriteFile(history, []byte(hintRecord("cold-id", now)), 0o644))
	require.NoError(t, os.WriteFile(rollout, []byte("seed\n"), 0o644))
	provider := newLiveActivityTestProvider(history)

	var lookupIDs []string
	lookup := func(_ context.Context, id string) (LiveActivitySource, bool, error) {
		lookupIDs = append(lookupIDs, id)
		return LiveActivitySource{
			Path:          rollout,
			StoredSize:    0,
			StoredMTimeNS: 0,
			HasStoredStat: true,
		}, true, nil
	}
	var syncCalls [][]string
	failSync := false
	syncPaths := func(_ context.Context, paths []string) error {
		syncCalls = append(syncCalls, append([]string(nil), paths...))
		if failSync {
			return errors.New("temporary sync failure")
		}
		return nil
	}
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, lookup, syncPaths, nil)

	stats, err := poller.PollOnce(t.Context(), now)
	require.NoError(t, err)
	assert.Equal(t, []string{"codex:cold-id"}, lookupIDs)
	assert.Equal(t, [][]string{{rollout}}, syncCalls)
	assert.Equal(t, 1, stats.SessionLookups)
	assert.Equal(t, 1, stats.SourceStats)
	assert.Equal(t, 1, stats.SyncPaths)

	_, err = poller.PollOnce(t.Context(), now.Add(time.Second))
	require.NoError(t, err)
	assert.Len(t, syncCalls, 1)

	appendFile(t, rollout, "still-open growth\n")
	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Second))
	require.NoError(t, err)
	require.Len(t, syncCalls, 2)
	assert.Equal(t, []string{rollout}, syncCalls[1])

	appendFile(t, rollout, "retry growth\n")
	failSync = true
	_, err = poller.PollOnce(t.Context(), now.Add(3*time.Second))
	require.Error(t, err)
	failSync = false
	_, err = poller.PollOnce(t.Context(), now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Len(t, syncCalls, 4, "failed observations must retry")
	assert.Zero(t, provider.findSourceCalls)
}

func TestLiveActivityBoundsRetriesAndExpiration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	var records strings.Builder
	for i := range liveActivityMaxEntries + 1 {
		records.WriteString(hintRecord(fmt.Sprintf("id-%05d", i), now))
	}
	require.NoError(t, os.WriteFile(history, []byte(records.String()), 0o644))
	provider := newLiveActivityTestProvider(history)
	lookups := 0
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		lookups++
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)

	stats, err := poller.PollOnce(t.Context(), now)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(poller.hot)+len(poller.retries), liveActivityMaxEntries)
	assert.Equal(t, liveActivityMaxEntries, lookups)
	assert.Equal(t, liveActivityMaxEntries, stats.SessionLookups)

	_, err = poller.PollOnce(t.Context(), now.Add(liveActivityRetryTTL-time.Second))
	require.NoError(t, err)
	assert.Greater(t, lookups, liveActivityMaxEntries)
	beforeExpiry := lookups
	_, err = poller.PollOnce(t.Context(), now.Add(liveActivityRetryTTL+time.Second))
	require.NoError(t, err)
	assert.Equal(t, beforeExpiry, lookups)
	assert.Empty(t, poller.retries)
	assert.Zero(t, provider.findSourceCalls)
}

func TestLiveActivityRefreshesCanonicalPathAndDropsMissing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	require.NoError(t, os.WriteFile(history, []byte(hintRecord("move", now)), 0o644))
	require.NoError(t, os.WriteFile(first, []byte("first\n"), 0o644))
	require.NoError(t, os.WriteFile(second, []byte("second\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	selected := first
	var synced []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{Path: selected}, true, nil
	}, func(_ context.Context, paths []string) error {
		synced = append(synced, paths...)
		return nil
	}, nil)

	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, []string{first}, synced)
	selected = second
	appendFile(t, history, hintRecord("move", now.Add(time.Minute)))
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, []string{first, second}, synced)

	require.NoError(t, os.Remove(second))
	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Empty(t, poller.hot)
	assert.Zero(t, provider.findSourceCalls)
}

func TestLiveActivityArchiveCardinalityDoesNotChangeWork(t *testing.T) {
	small := runLiveActivityCardinalityCase(t, 10)
	large := runLiveActivityCardinalityCase(t, 20_000)

	assert.Equal(t, withoutHintBytes(small), withoutHintBytes(large))
	assert.Equal(t, LiveActivityPollStats{
		HintFiles:      1,
		SessionLookups: 1,
		SourceStats:    1,
		SyncPaths:      1,
	}, withoutHintBytes(small))
}

func TestLiveActivityRunStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	poller := NewLiveActivityPoller(nil,
		func(context.Context, string) (LiveActivitySource, bool, error) {
			t.Fatal("lookup after cancellation")
			return LiveActivitySource{}, false, nil
		},
		func(context.Context, []string) error {
			t.Fatal("sync after cancellation")
			return nil
		}, nil)
	poller.Run(ctx)
}

func TestLiveActivityBoundsPathBytesByOldestActivity(t *testing.T) {
	poller := NewLiveActivityPoller(nil, nil, nil, nil)
	now := time.Unix(1_800_000_000, 0).UTC()
	poller.hot["older"] = &liveActivityHotEntry{
		source: LiveActivitySource{
			Path: strings.Repeat("a", liveActivityMaxPathBytes/2+1),
		},
		lastActivity: now.Add(-time.Hour),
	}
	poller.hot["newer"] = &liveActivityHotEntry{
		source: LiveActivitySource{
			Path: strings.Repeat("b", liveActivityMaxPathBytes/2+1),
		},
		lastActivity: now,
	}

	poller.enforceBounds()

	assert.NotContains(t, poller.hot, "older")
	assert.Contains(t, poller.hot, "newer")
	assert.LessOrEqual(t, poller.hotPathBytes(), liveActivityMaxPathBytes)
}

func TestLiveActivityEvictsQuiescentBeforePending(t *testing.T) {
	poller := NewLiveActivityPoller(nil, nil, nil, nil)
	now := time.Unix(1_800_000_000, 0).UTC()
	poller.hot["older-pending"] = &liveActivityHotEntry{
		source: LiveActivitySource{
			Path: strings.Repeat("a", liveActivityMaxPathBytes/2+1),
		},
		lastActivity: now.Add(-time.Hour),
		pending:      true,
	}
	poller.hot["newer-quiescent"] = &liveActivityHotEntry{
		source: LiveActivitySource{
			Path: strings.Repeat("b", liveActivityMaxPathBytes/2+1),
		},
		lastActivity: now,
	}

	poller.enforceBounds()

	assert.Contains(t, poller.hot, "older-pending")
	assert.NotContains(t, poller.hot, "newer-quiescent")
}

func TestLiveActivityDeduplicatesOverlappingHintSources(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	firstHistory := filepath.Join(dir, "first-history.jsonl")
	secondHistory := filepath.Join(dir, "second-history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	record := hintRecord("duplicate", now)
	require.NoError(t, os.WriteFile(firstHistory, []byte(record), 0o644))
	require.NoError(t, os.WriteFile(secondHistory, []byte(record+record), 0o644))
	require.NoError(t, os.WriteFile(rollout, []byte("changed"), 0o644))
	provider := newLiveActivityTestProvider(firstHistory)
	lookups := 0
	syncs := 0
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources: []parser.ActivityHintSource{
			{Path: firstHistory},
			{Path: secondHistory},
		},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		lookups++
		return LiveActivitySource{Path: rollout}, true, nil
	}, func(context.Context, []string) error {
		syncs++
		return nil
	}, nil)

	stats, err := poller.PollOnce(t.Context(), now)

	require.NoError(t, err)
	assert.Equal(t, 1, lookups)
	assert.Equal(t, 1, syncs)
	assert.Equal(t, 2, stats.HintFiles)
	assert.Equal(t, 1, stats.SessionLookups)
	assert.Equal(t, 1, stats.SyncPaths)
}

func TestLiveActivityLookupErrorDoesNotDuplicateHotEntry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(t, os.WriteFile(history, []byte(hintRecord("active", now)), 0o644))
	provider := newLiveActivityTestProvider(history)
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{}, false, errors.New("temporary lookup error")
	}, func(context.Context, []string) error {
		return nil
	}, nil)
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(rollout, []byte("stable"), 0o644))
	info, err := os.Stat(rollout)
	require.NoError(t, err)
	poller.hot["codex:active"] = &liveActivityHotEntry{
		source: LiveActivitySource{
			Path:          rollout,
			StoredSize:    info.Size(),
			StoredMTimeNS: info.ModTime().UnixNano(),
			HasStoredStat: true,
		},
		lastActivity: now,
	}

	_, err = poller.PollOnce(t.Context(), now)

	require.Error(t, err)
	assert.Contains(t, poller.hot, "codex:active")
	assert.NotContains(t, poller.retries, "codex:active")
	assert.Equal(t, 1, len(poller.hot)+len(poller.retries))
}

func TestLiveActivityGrowthRefreshesHotExpiration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	require.NoError(t, os.WriteFile(history, []byte(
		hintRecord("growing", now.Add(-23*time.Hour)),
	), 0o644))
	require.NoError(t, os.WriteFile(rollout, []byte("growth"), 0o644))
	provider := newLiveActivityTestProvider(history)
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{Path: rollout}, true, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)

	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(t, err)
	require.Contains(t, poller.hot, "codex:growing")
	assert.Equal(t, now, poller.hot["codex:growing"].lastActivity)

	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Contains(t, poller.hot, "codex:growing",
		"observed growth must refresh the 24-hour retention window")

	_, err = poller.PollOnce(t.Context(), now.Add(liveActivityHotTTL+time.Second))
	require.NoError(t, err)
	assert.Empty(t, poller.hot)
}

func TestLiveActivityThrottlesErrorsWithoutRecordContent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(t, os.Mkdir(history, 0o755))
	provider := newLiveActivityTestProvider(history)
	var logs []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		return nil
	}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	for _, at := range []time.Time{
		now,
		now.Add(time.Minute),
		now.Add(liveActivityLogInterval),
	} {
		_, err := poller.PollOnce(t.Context(), at)
		require.Error(t, err)
	}

	require.Len(t, logs, 2)
	for _, logLine := range logs {
		assert.Contains(t, logLine, "1")
		assert.Contains(t, logLine, history)
		assert.NotContains(t, logLine, "private-prompt-sentinel")
	}
}

func runLiveActivityCardinalityCase(
	t *testing.T,
	unrelated int,
) LiveActivityPollStats {
	t.Helper()
	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	for i := range unrelated {
		path := filepath.Join(dir, fmt.Sprintf("unrelated-%05d.jsonl", i))
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
	}
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "selected.jsonl")
	require.NoError(t, os.WriteFile(history, []byte(hintRecord("selected", now)), 0o644))
	require.NoError(t, os.WriteFile(rollout, []byte("changed"), 0o644))
	provider := newLiveActivityTestProvider(history)
	var synced []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(_ context.Context, id string) (LiveActivitySource, bool, error) {
		assert.Equal(t, "codex:selected", id)
		return LiveActivitySource{Path: rollout, HasStoredStat: true}, true, nil
	}, func(_ context.Context, paths []string) error {
		synced = append(synced, paths...)
		return nil
	}, nil)
	stats, err := poller.PollOnce(t.Context(), now)
	require.NoError(t, err)
	assert.Equal(t, []string{rollout}, synced)
	assert.Zero(t, provider.findSourceCalls)
	return stats
}

func withoutHintBytes(stats LiveActivityPollStats) LiveActivityPollStats {
	stats.HintBytes = 0
	return stats
}

func appendFile(t *testing.T, path string, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
