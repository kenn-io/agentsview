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
		Def: parser.AgentDef{
			Type:     parser.AgentCodex,
			IDPrefix: "codex:",
		},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			ActivityHints: parser.CapabilitySupported,
		}},
		hintPath: hintPath,
	}
}

type cancelingActivityHintDecoder struct {
	cancel  context.CancelFunc
	decoded int
}

func (d *cancelingActivityHintDecoder) ActivityHintSources(
	context.Context,
) ([]parser.ActivityHintSource, error) {
	return nil, nil
}

func (d *cancelingActivityHintDecoder) DecodeActivityHint(
	line []byte,
) (parser.ActivityHint, bool) {
	d.decoded++
	d.cancel()
	return literalActivityHintDecoder{}.DecodeActivityHint(line)
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
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	require.NoError(os.WriteFile(history, []byte(hintRecord("cold-id", now)), 0o644))
	require.NoError(os.WriteFile(rollout, []byte("seed\n"), 0o644))
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
	require.NoError(err)
	assert.Equal([]string{"codex:cold-id"}, lookupIDs)
	assert.Equal([][]string{{rollout}}, syncCalls)
	assert.Equal(1, stats.SessionLookups)
	assert.Equal(1, stats.SourceStats)
	assert.Equal(1, stats.SyncPaths)

	_, err = poller.PollOnce(t.Context(), now.Add(time.Second))
	require.NoError(err)
	assert.Len(syncCalls, 1)

	appendFile(t, rollout, "still-open growth\n")
	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Second))
	require.NoError(err)
	require.Len(syncCalls, 2)
	assert.Equal([]string{rollout}, syncCalls[1])

	appendFile(t, rollout, "retry growth\n")
	failSync = true
	_, err = poller.PollOnce(t.Context(), now.Add(3*time.Second))
	require.Error(err)
	failSync = false
	_, err = poller.PollOnce(t.Context(), now.Add(4*time.Second))
	require.NoError(err)
	assert.Len(syncCalls, 4, "failed observations must retry")
	assert.Zero(provider.findSourceCalls)
}

func TestLiveActivityBoundsRetriesAndExpiration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	var records strings.Builder
	for i := range liveActivityMaxEntries + 1 {
		records.WriteString(hintRecord(fmt.Sprintf("id-%05d", i), now))
	}
	require.NoError(os.WriteFile(history, []byte(records.String()), 0o644))
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
	require.NoError(err)
	assert.LessOrEqual(len(poller.hot)+len(poller.retries), liveActivityMaxEntries)
	assert.Equal(liveActivityMaxEntries, lookups)
	assert.Equal(liveActivityMaxEntries, stats.SessionLookups)

	_, err = poller.PollOnce(t.Context(), now.Add(liveActivityRetryTTL-time.Second))
	require.NoError(err)
	assert.Greater(lookups, liveActivityMaxEntries)
	beforeExpiry := lookups
	_, err = poller.PollOnce(t.Context(), now.Add(liveActivityRetryTTL+time.Second))
	require.NoError(err)
	assert.Equal(beforeExpiry, lookups)
	assert.Empty(poller.retries)
	assert.Zero(provider.findSourceCalls)
}

func TestLiveActivityHintBudgetIsGlobalAcrossSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	var firstRecords strings.Builder
	var secondRecords strings.Builder
	for i := range activityHintMaxIDsPerPoll - 1 {
		firstRecords.WriteString(hintRecord(fmt.Sprintf("first-%05d", i), now))
	}
	secondRecords.WriteString(hintRecord("second-00000", now))
	secondRecords.WriteString(hintRecord("second-00001", now))
	require.NoError(os.WriteFile(first, []byte(firstRecords.String()), 0o644))
	require.NoError(os.WriteFile(second, []byte(secondRecords.String()), 0o644))
	provider := newLiveActivityTestProvider(first)
	decoder := &countingActivityHintDecoder{}
	var lookupIDs []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    decoder,
		Sources: []parser.ActivityHintSource{
			{Path: first},
			{Path: second},
		},
	}}, func(_ context.Context, id string) (LiveActivitySource, bool, error) {
		lookupIDs = append(lookupIDs, id)
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)

	firstStats, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)
	assert.Equal(activityHintMaxIDsPerPoll, decoder.decoded)
	assert.Len(lookupIDs, activityHintMaxIDsPerPoll-1)
	assert.Equal(activityHintMaxIDsPerPoll-1, firstStats.SessionLookups)
	assert.NotContains(lookupIDs, "codex:second-00000")
	assert.NotContains(lookupIDs, "codex:second-00001")
	secondCursor := poller.cursors[liveActivityCursorKey{path: second}]
	require.NotNil(secondCursor)
	assert.False(secondCursor.initialized,
		"a partially read source must remain unread")

	_, err = poller.PollOnce(t.Context(), now.Add(time.Second))
	require.NoError(err)
	assert.Contains(lookupIDs, "codex:second-00000")
	assert.Contains(lookupIDs, "codex:second-00001")
	assert.True(secondCursor.initialized)
}

func TestLiveActivityHintByteBudgetIsGlobalAcrossSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	firstContent := []byte(strings.Repeat("x", activityHintMaxReadBytes-64))
	const firstSecondID = "second-byte-00000000000000000000"
	const secondSecondID = "second-byte-00000000000000000001"
	secondContent := hintRecord(firstSecondID, now) +
		hintRecord(secondSecondID, now)
	require.Greater(len(secondContent), 64)
	require.NoError(os.WriteFile(first, firstContent, 0o644))
	require.NoError(os.WriteFile(second, []byte(secondContent), 0o644))
	provider := newLiveActivityTestProvider(first)
	var lookupIDs []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources: []parser.ActivityHintSource{
			{Path: first},
			{Path: second},
		},
	}}, func(_ context.Context, id string) (LiveActivitySource, bool, error) {
		lookupIDs = append(lookupIDs, id)
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)

	stats, err := poller.PollOnce(t.Context(), now)

	require.NoError(err)
	assert.Equal(activityHintMaxReadBytes, stats.HintBytes)
	assert.Empty(lookupIDs)
	secondCursor := poller.cursors[liveActivityCursorKey{path: second}]
	require.NotNil(secondCursor)
	assert.False(secondCursor.initialized,
		"a partially read source must remain unread")

	_, err = poller.PollOnce(t.Context(), now.Add(time.Second))
	require.NoError(err)
	assert.Contains(lookupIDs, "codex:"+firstSecondID)
	assert.Contains(lookupIDs, "codex:"+secondSecondID)
	assert.True(secondCursor.initialized)
}

func TestLiveActivityNewHintRestartsExpiredLookupWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.WriteFile(
		history, []byte(hintRecord("waiting", now)), 0o644,
	))
	provider := newLiveActivityTestProvider(history)
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)
	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)
	require.Contains(poller.retries, "codex:waiting")
	assert.Equal(now, poller.retries["codex:waiting"].firstSeen)

	later := now.Add(liveActivityRetryTTL + time.Second)
	appendFile(t, history, hintRecord("waiting", later))
	_, err = poller.PollOnce(t.Context(), later)

	require.NoError(err)
	require.Contains(poller.retries, "codex:waiting",
		"a new prompt must receive its own bounded lookup window")
	assert.Equal(later, poller.retries["codex:waiting"].firstSeen)
}

func TestLiveActivityOlderHintDoesNotRegressRetryTimestamp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.WriteFile(
		history, []byte(hintRecord("waiting", now)), 0o644,
	))
	provider := newLiveActivityTestProvider(history)
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)
	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)

	replacement := history + ".new"
	require.NoError(os.WriteFile(
		replacement,
		[]byte(hintRecord("waiting", now.Add(-time.Hour))),
		0o644,
	))
	require.NoError(os.Rename(replacement, history))
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute))

	require.NoError(err)
	require.Contains(poller.retries, "codex:waiting")
	assert.Equal(now, poller.retries["codex:waiting"].firstSeen)
	assert.Equal(now, poller.retries["codex:waiting"].lastHint)
}

func TestLiveActivityRetriesHotSessionAfterLookupFailureAndMove(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	require.NoError(os.WriteFile(history, []byte(hintRecord("move", now)), 0o644))
	require.NoError(os.WriteFile(first, []byte("first\n"), 0o644))
	require.NoError(os.WriteFile(second, []byte("second\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	lookupPath := first
	lookupErr := error(nil)
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		if lookupErr != nil {
			return LiveActivitySource{}, false, lookupErr
		}
		return LiveActivitySource{Path: lookupPath}, true, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)
	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)
	require.Contains(poller.hot, "codex:move")

	appendFile(t, history, hintRecord("move", now.Add(time.Minute)))
	lookupErr = errors.New("temporary lookup failure")
	require.NoError(os.Remove(first))
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute))
	require.Error(err)
	assert.NotContains(poller.hot, "codex:move")
	require.Contains(poller.retries, "codex:move")

	lookupErr = nil
	lookupPath = second
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute+time.Second))
	require.NoError(err)
	require.Contains(poller.hot, "codex:move")
	assert.Equal(second, poller.hot["codex:move"].source.Path)
	assert.NotContains(poller.retries, "codex:move")
}

func TestLiveActivityRetriesCanonicalRefreshWhileOldPathExists(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	require.NoError(os.WriteFile(history, []byte(hintRecord("move", now)), 0o644))
	require.NoError(os.WriteFile(first, []byte("first\n"), 0o644))
	require.NoError(os.WriteFile(second, []byte("second\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	lookupPath := first
	lookupErr := error(nil)
	lookups := 0
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		lookups++
		if lookupErr != nil {
			return LiveActivitySource{}, false, lookupErr
		}
		return LiveActivitySource{Path: lookupPath}, true, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)
	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)

	appendFile(t, history, hintRecord("move", now.Add(time.Minute)))
	lookupErr = errors.New("temporary lookup failure")
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute))
	require.Error(err)
	require.Contains(poller.hot, "codex:move")
	assert.Equal(first, poller.hot["codex:move"].source.Path)
	require.NotNil(poller.hot["codex:move"].refreshRetry)

	lookupErr = nil
	lookupPath = second
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute+time.Second))
	require.NoError(err)
	require.Contains(poller.hot, "codex:move")
	assert.Equal(second, poller.hot["codex:move"].source.Path)
	assert.Nil(poller.hot["codex:move"].refreshRetry)
	assert.Equal(3, lookups)
}

func TestLiveActivityOlderHintDoesNotRegressHotRefreshRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	require.NoError(os.WriteFile(
		history, []byte(hintRecord("active", now)), 0o644,
	))
	require.NoError(os.WriteFile(rollout, []byte("stable\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	var lookupErr error
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		if lookupErr != nil {
			return LiveActivitySource{}, false, lookupErr
		}
		return LiveActivitySource{Path: rollout}, true, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)
	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)

	newerHint := now.Add(time.Minute)
	replacement := history + ".newer"
	require.NoError(os.WriteFile(
		replacement, []byte(hintRecord("active", newerHint)), 0o644,
	))
	require.NoError(os.Rename(replacement, history))
	lookupErr = errors.New("temporary lookup failure")
	_, err = poller.PollOnce(t.Context(), newerHint)
	require.Error(err)
	require.Contains(poller.hot, "codex:active")
	require.NotNil(poller.hot["codex:active"].refreshRetry)

	replacement = history + ".older"
	require.NoError(os.WriteFile(
		replacement,
		[]byte(hintRecord("active", now.Add(-time.Hour))),
		0o644,
	))
	require.NoError(os.Rename(replacement, history))
	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Minute))

	require.Error(err)
	require.NotNil(poller.hot["codex:active"].refreshRetry)
	assert.Equal(
		newerHint, poller.hot["codex:active"].refreshRetry.firstSeen,
	)
	assert.Equal(
		newerHint, poller.hot["codex:active"].refreshRetry.lastHint,
	)
}

func TestLiveActivityRetriesHintWhoseIndexedPathIsMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	missing := filepath.Join(dir, "missing.jsonl")
	canonical := filepath.Join(dir, "canonical.jsonl")
	require.NoError(os.WriteFile(
		history, []byte(hintRecord("move", now)), 0o644,
	))
	require.NoError(os.WriteFile(canonical, []byte("active\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	lookups := 0
	var synced []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(_ context.Context, id string) (LiveActivitySource, bool, error) {
		assert.Equal("codex:move", id)
		lookups++
		if lookups == 1 {
			return LiveActivitySource{Path: missing}, true, nil
		}
		return LiveActivitySource{Path: canonical}, true, nil
	}, func(_ context.Context, paths []string) error {
		synced = append(synced, paths...)
		return nil
	}, nil)

	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)
	assert.Equal(1, lookups)
	assert.NotContains(poller.hot, "codex:move")
	require.Contains(poller.retries, "codex:move")

	_, err = poller.PollOnce(t.Context(), now.Add(time.Second))
	require.NoError(err)
	assert.Equal(2, lookups)
	require.Contains(poller.hot, "codex:move")
	assert.Equal(canonical, poller.hot["codex:move"].source.Path)
	assert.NotContains(poller.retries, "codex:move")
	assert.Equal([]string{canonical}, synced)
}

func TestLiveActivityRetriesRepeatedMissingIndexedPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	firstMissing := filepath.Join(dir, "first-missing.jsonl")
	secondMissing := filepath.Join(dir, "second-missing.jsonl")
	canonical := filepath.Join(dir, "canonical.jsonl")
	require.NoError(os.WriteFile(
		history, []byte(hintRecord("move", now)), 0o644,
	))
	require.NoError(os.WriteFile(canonical, []byte("active\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	lookupPaths := []string{firstMissing, secondMissing, canonical}
	lookups := 0
	var synced []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(_ context.Context, id string) (LiveActivitySource, bool, error) {
		assert.Equal("codex:move", id)
		require.Less(lookups, len(lookupPaths))
		path := lookupPaths[lookups]
		lookups++
		return LiveActivitySource{Path: path}, true, nil
	}, func(_ context.Context, paths []string) error {
		synced = append(synced, paths...)
		return nil
	}, nil)

	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)
	require.Contains(poller.retries, "codex:move")

	_, err = poller.PollOnce(t.Context(), now.Add(time.Second))
	require.NoError(err)
	require.Contains(poller.retries, "codex:move",
		"a second stale indexed path must not consume the retry")

	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Second))
	require.NoError(err)
	assert.Equal(3, lookups)
	require.Contains(poller.hot, "codex:move")
	assert.Equal(canonical, poller.hot["codex:move"].source.Path)
	assert.NotContains(poller.retries, "codex:move")
	assert.Equal([]string{canonical}, synced)
}

func TestLiveActivityOlderReplayPreservesRetryAcrossRepeatedMissingPaths(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	firstMissing := filepath.Join(dir, "first-missing.jsonl")
	secondMissing := filepath.Join(dir, "second-missing.jsonl")
	canonical := filepath.Join(dir, "canonical.jsonl")
	require.NoError(os.WriteFile(
		history, []byte(hintRecord("move", now)), 0o644,
	))
	require.NoError(os.WriteFile(canonical, []byte("active\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	lookupPaths := []string{firstMissing, secondMissing, canonical}
	lookups := 0
	var synced []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(_ context.Context, id string) (LiveActivitySource, bool, error) {
		assert.Equal("codex:move", id)
		require.Less(lookups, len(lookupPaths))
		path := lookupPaths[lookups]
		lookups++
		return LiveActivitySource{Path: path}, true, nil
	}, func(_ context.Context, paths []string) error {
		synced = append(synced, paths...)
		return nil
	}, nil)

	_, err := poller.PollOnce(t.Context(), now)
	require.NoError(err)
	require.Contains(poller.retries, "codex:move")

	replacement := history + ".older"
	require.NoError(os.WriteFile(
		replacement,
		[]byte(hintRecord("move", now.Add(-time.Hour))),
		0o644,
	))
	require.NoError(os.Rename(replacement, history))
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute))
	require.NoError(err)
	require.Contains(poller.retries, "codex:move")
	assert.Equal(now, poller.retries["codex:move"].firstSeen)
	assert.Equal(now, poller.retries["codex:move"].lastHint)

	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute+time.Second))
	require.NoError(err)
	assert.Equal(3, lookups)
	require.Contains(poller.hot, "codex:move")
	assert.Equal(canonical, poller.hot["codex:move"].source.Path)
	assert.NotContains(poller.retries, "codex:move")
	assert.Equal([]string{canonical}, synced)
}

func TestLiveActivityPreservesRefreshRetryAcrossHotExpiration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	require.NoError(os.WriteFile(history, []byte(hintRecord("old", now)), 0o644))
	require.NoError(os.WriteFile(rollout, []byte("stable\n"), 0o644))
	provider := newLiveActivityTestProvider(history)
	lookupErr := errors.New("temporary lookup failure")
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		if lookupErr != nil {
			return LiveActivitySource{}, false, lookupErr
		}
		return LiveActivitySource{Path: rollout}, true, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)
	poller.hot["codex:old"] = &liveActivityHotEntry{
		source:       LiveActivitySource{Path: rollout},
		lastActivity: now.Add(-liveActivityHotTTL),
	}

	_, err := poller.PollOnce(t.Context(), now)
	require.Error(err)
	assert.NotContains(poller.hot, "codex:old")
	require.Contains(poller.retries, "codex:old")
	assert.Equal(now, poller.retries["codex:old"].firstSeen)

	lookupErr = nil
	_, err = poller.PollOnce(t.Context(), now.Add(time.Second))
	require.NoError(err)
	assert.Contains(poller.hot, "codex:old")
	assert.NotContains(poller.retries, "codex:old")
}

func TestLiveActivityRefreshesCanonicalPathAndDropsMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	require.NoError(os.WriteFile(history, []byte(hintRecord("move", now)), 0o644))
	require.NoError(os.WriteFile(first, []byte("first\n"), 0o644))
	require.NoError(os.WriteFile(second, []byte("second\n"), 0o644))
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
	require.NoError(err)
	require.Equal([]string{first}, synced)
	selected = second
	appendFile(t, history, hintRecord("move", now.Add(time.Minute)))
	_, err = poller.PollOnce(t.Context(), now.Add(time.Minute))
	require.NoError(err)
	assert.Equal([]string{first, second}, synced)

	require.NoError(os.Remove(second))
	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Minute))
	require.NoError(err)
	assert.Empty(poller.hot)
	assert.Zero(provider.findSourceCalls)
}

func TestLiveActivityDetectsEqualSizeMtimeSourceReplacement(t *testing.T) {
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	replacement := filepath.Join(dir, "replacement.jsonl")
	require.NoError(os.WriteFile(history, []byte(hintRecord("replace", now)), 0o644))
	require.NoError(os.WriteFile(rollout, []byte("first\n"), 0o644))
	require.NoError(os.Chtimes(rollout, now, now))
	oldInfo, err := os.Stat(rollout)
	require.NoError(err)
	oldInode, oldDevice := getFileIdentity(rollout, oldInfo)
	require.NoError(os.WriteFile(replacement, []byte("other\n"), 0o644))
	require.NoError(os.Chtimes(replacement, now, now))
	require.NoError(os.Rename(replacement, rollout))
	newInfo, err := os.Stat(rollout)
	require.NoError(err)
	newInode, newDevice := getFileIdentity(rollout, newInfo)
	require.NotEqual([2]int64{oldInode, oldDevice}, [2]int64{newInode, newDevice})
	require.Equal(oldInfo.Size(), newInfo.Size())
	require.Equal(oldInfo.ModTime(), newInfo.ModTime())
	provider := newLiveActivityTestProvider(history)
	var synced []string
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{
			Path:              rollout,
			StoredSize:        oldInfo.Size(),
			StoredMTimeNS:     oldInfo.ModTime().UnixNano(),
			StoredInode:       oldInode,
			StoredDevice:      oldDevice,
			HasStoredStat:     true,
			HasStoredIdentity: true,
		}, true, nil
	}, func(_ context.Context, paths []string) error {
		synced = append(synced, paths...)
		return nil
	}, nil)

	_, err = poller.PollOnce(t.Context(), now)

	require.NoError(err)
	assert.Equal(t, []string{rollout}, synced)
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

func TestLiveActivityBoundsCursorMemoryAcrossManySources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const (
		sourceCount            = 320
		expectedMaxCursors     = 256
		expectedMaxCursorBytes = 2 << 20
	)
	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	partial := []byte(strings.Repeat("x", 64<<10))
	sources := make([]parser.ActivityHintSource, 0, sourceCount)
	for i := range sourceCount {
		path := filepath.Join(dir, fmt.Sprintf("history-%03d.jsonl", i))
		require.NoError(os.WriteFile(path, partial, 0o644))
		sources = append(sources, parser.ActivityHintSource{Path: path})
	}
	provider := newLiveActivityTestProvider(sources[0].Path)
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  sources,
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		return nil
	}, nil)

	for i := range 5 {
		stats, err := poller.PollOnce(
			t.Context(), now.Add(time.Duration(i)*time.Second),
		)
		require.NoError(err)
		assert.LessOrEqual(stats.HintFiles, expectedMaxCursors)
		assert.LessOrEqual(stats.HintBytes, activityHintMaxReadBytes)
	}

	retainedBytes := 0
	for key := range poller.cursors {
		retainedBytes += len(key.path)
	}
	assert.LessOrEqual(len(poller.cursors), expectedMaxCursors)
	assert.LessOrEqual(retainedBytes, expectedMaxCursorBytes)
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
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	firstHistory := filepath.Join(dir, "first-history.jsonl")
	secondHistory := filepath.Join(dir, "second-history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	record := hintRecord("duplicate", now)
	require.NoError(os.WriteFile(firstHistory, []byte(record), 0o644))
	require.NoError(os.WriteFile(secondHistory, []byte(record+record), 0o644))
	require.NoError(os.WriteFile(rollout, []byte("changed"), 0o644))
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

	require.NoError(err)
	assert.Equal(1, lookups)
	assert.Equal(1, syncs)
	assert.Equal(2, stats.HintFiles)
	assert.Equal(1, stats.SessionLookups)
	assert.Equal(1, stats.SyncPaths)
}

func TestLiveActivityLookupErrorDoesNotDuplicateHotEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.WriteFile(history, []byte(hintRecord("active", now)), 0o644))
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
	require.NoError(os.WriteFile(rollout, []byte("stable"), 0o644))
	info, err := os.Stat(rollout)
	require.NoError(err)
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

	require.Error(err)
	assert.Contains(poller.hot, "codex:active")
	assert.NotContains(poller.retries, "codex:active")
	assert.Equal(1, len(poller.hot)+len(poller.retries))
}

func TestLiveActivityGrowthRefreshesHotExpiration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	require.NoError(os.WriteFile(history, []byte(
		hintRecord("growing", now.Add(-23*time.Hour)),
	), 0o644))
	require.NoError(os.WriteFile(rollout, []byte("growth"), 0o644))
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
	require.NoError(err)
	require.Contains(poller.hot, "codex:growing")
	assert.Equal(now, poller.hot["codex:growing"].lastActivity)

	_, err = poller.PollOnce(t.Context(), now.Add(2*time.Hour))
	require.NoError(err)
	assert.Contains(poller.hot, "codex:growing",
		"observed growth must refresh the 24-hour retention window")

	_, err = poller.PollOnce(t.Context(), now.Add(liveActivityHotTTL+time.Second))
	require.NoError(err)
	assert.Empty(poller.hot)
}

func TestLiveActivityOlderHintReplayPreservesObservedGrowth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	oldHint := hintRecord("growing", now.Add(-23*time.Hour))
	require.NoError(os.WriteFile(history, []byte(oldHint), 0o644))
	require.NoError(os.WriteFile(rollout, []byte("growth"), 0o644))
	info, err := os.Stat(rollout)
	require.NoError(err)
	inode, device := getFileIdentity(rollout, info)
	provider := newLiveActivityTestProvider(history)
	lookups := 0
	syncs := 0
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		lookups++
		source := LiveActivitySource{Path: rollout}
		if lookups > 1 {
			source.StoredSize = info.Size()
			source.StoredMTimeNS = info.ModTime().UnixNano()
			source.StoredInode = inode
			source.StoredDevice = device
			source.HasStoredStat = true
			source.HasStoredIdentity = true
		}
		return source, true, nil
	}, func(context.Context, []string) error {
		syncs++
		return nil
	}, nil)

	_, err = poller.PollOnce(t.Context(), now)
	require.NoError(err)
	require.Contains(poller.hot, "codex:growing")
	assert.Equal(now, poller.hot["codex:growing"].lastActivity)

	delete(poller.cursors, liveActivityCursorKey{path: history})
	_, err = poller.PollOnce(t.Context(), now.Add(time.Hour))

	require.NoError(err)
	require.Contains(poller.hot, "codex:growing")
	assert.Equal(now, poller.hot["codex:growing"].lastActivity)
	assert.Equal(2, lookups)
	assert.Equal(1, syncs)
}

func TestLiveActivityStopsAfterHintReadCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	var sources []parser.ActivityHintSource
	for i := range 3 {
		path := filepath.Join(dir, fmt.Sprintf("history-%d.jsonl", i))
		require.NoError(os.WriteFile(path, []byte(
			hintRecord(fmt.Sprintf("id-%d-a", i), now)+
				hintRecord(fmt.Sprintf("id-%d-b", i), now),
		), 0o644))
		sources = append(sources, parser.ActivityHintSource{Path: path})
	}
	ctx, cancel := context.WithCancel(t.Context())
	decoder := &cancelingActivityHintDecoder{cancel: cancel}
	provider := newLiveActivityTestProvider(sources[0].Path)
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    decoder,
		Sources:  sources,
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		t.Fatal("lookup after hint cancellation")
		return LiveActivitySource{}, false, nil
	}, func(context.Context, []string) error {
		t.Fatal("sync after hint cancellation")
		return nil
	}, nil)

	stats, err := poller.PollOnce(ctx, now)

	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, stats.HintFiles)
	assert.Equal(1, decoder.decoded)
	assert.Zero(stats.SessionLookups)
	assert.Zero(stats.SourceStats)
}

func TestLiveActivityStopsAfterLookupCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()
	history := filepath.Join(dir, "history.jsonl")
	rollout := filepath.Join(dir, "rollout.jsonl")
	require.NoError(os.WriteFile(
		history, []byte(hintRecord("active", now)), 0o644,
	))
	require.NoError(os.WriteFile(rollout, []byte("growth"), 0o644))
	ctx, cancel := context.WithCancel(t.Context())
	provider := newLiveActivityTestProvider(history)
	syncs := 0
	poller := NewLiveActivityPoller([]LiveActivityTarget{{
		Provider: provider,
		Hints:    provider,
		Sources:  []parser.ActivityHintSource{{Path: history}},
	}}, func(context.Context, string) (LiveActivitySource, bool, error) {
		cancel()
		return LiveActivitySource{Path: rollout}, true, nil
	}, func(context.Context, []string) error {
		syncs++
		return nil
	}, nil)

	stats, err := poller.PollOnce(ctx, now)

	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, stats.SessionLookups)
	assert.Zero(stats.SourceStats)
	assert.Zero(stats.SyncPaths)
	assert.Zero(syncs)
}

func TestLiveActivityThrottlesErrorsWithoutRecordContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	history := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.Mkdir(history, 0o755))
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
		require.Error(err)
	}

	require.Len(logs, 2)
	for _, logLine := range logs {
		assert.Contains(logLine, "1")
		assert.Contains(logLine, fmt.Sprintf("%q", history))
		assert.NotContains(logLine, "private-prompt-sentinel")
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
