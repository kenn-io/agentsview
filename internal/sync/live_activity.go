package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	liveActivityPollInterval = 30 * time.Second
	liveActivityHotTTL       = 24 * time.Hour
	liveActivityRetryTTL     = 2 * time.Minute
	liveActivityLogInterval  = 5 * time.Minute
	liveActivityMaxEntries   = 8192
	liveActivityMaxPathBytes = 2 << 20
)

type LiveActivitySource struct {
	Path          string
	StoredSize    int64
	StoredMTimeNS int64
	HasStoredStat bool
}

type LiveActivityLookup func(
	context.Context,
	string,
) (LiveActivitySource, bool, error)

type LiveActivitySync func(context.Context, []string) error

type LiveActivityTarget struct {
	Provider parser.Provider
	Hints    parser.ActivityHintProvider
	Sources  []parser.ActivityHintSource
}

type LiveActivityPollStats struct {
	HintFiles      int
	HintBytes      int
	SessionLookups int
	SourceStats    int
	SyncPaths      int
}

type liveActivityHotEntry struct {
	target       int
	source       LiveActivitySource
	lastActivity time.Time
	pending      bool
}

type liveActivityRetryEntry struct {
	target    int
	firstSeen time.Time
	lastHint  time.Time
}

type liveActivityCursorKey struct {
	target int
	path   string
}

type LiveActivityPoller struct {
	targets   []LiveActivityTarget
	lookup    LiveActivityLookup
	syncPaths LiveActivitySync
	logf      func(string, ...any)

	cursors map[liveActivityCursorKey]*activityHintCursor
	hot     map[string]*liveActivityHotEntry
	retries map[string]*liveActivityRetryEntry
	logged  map[string]time.Time
}

func NewLiveActivityPoller(
	targets []LiveActivityTarget,
	lookup LiveActivityLookup,
	syncPaths LiveActivitySync,
	logf func(string, ...any),
) *LiveActivityPoller {
	return &LiveActivityPoller{
		targets:   append([]LiveActivityTarget(nil), targets...),
		lookup:    lookup,
		syncPaths: syncPaths,
		logf:      logf,
		cursors:   make(map[liveActivityCursorKey]*activityHintCursor),
		hot:       make(map[string]*liveActivityHotEntry),
		retries:   make(map[string]*liveActivityRetryEntry),
		logged:    make(map[string]time.Time),
	}
}

func (p *LiveActivityPoller) PollOnce(
	ctx context.Context,
	now time.Time,
) (LiveActivityPollStats, error) {
	if err := ctx.Err(); err != nil {
		return LiveActivityPollStats{}, err
	}

	stats := LiveActivityPollStats{}
	var pollErrors []error
	hinted := make(map[string]liveActivityRetryEntry)
	for targetIndex, target := range p.targets {
		for _, source := range target.Sources {
			stats.HintFiles++
			key := liveActivityCursorKey{target: targetIndex, path: source.Path}
			cursor := p.cursors[key]
			if cursor == nil {
				cursor = &activityHintCursor{}
				p.cursors[key] = cursor
			}
			result, err := readActivityHints(
				ctx, source, target.Hints, cursor, now,
			)
			stats.HintBytes += result.BytesRead
			if err != nil {
				pollErrors = append(pollErrors, err)
				continue
			}
			if result.Overflow {
				p.logThrottled("hint-overflow", now,
					"live activity hint input exceeded a bounded poll: path=%q bytes=%d ids=%d",
					source.Path, result.BytesRead, len(result.Hints))
			}
			for _, hint := range result.Hints {
				fullID := target.Provider.Definition().IDPrefix + hint.RawSessionID
				previous, exists := hinted[fullID]
				if !exists || hint.Timestamp.After(previous.lastHint) {
					hinted[fullID] = liveActivityRetryEntry{
						target:   targetIndex,
						lastHint: hint.Timestamp,
					}
				}
			}
		}
	}

	attempted := make(map[string]struct{}, len(hinted))
	for _, fullID := range sortedLiveActivityKeys(hinted) {
		hint := hinted[fullID]
		attempted[fullID] = struct{}{}
		stats.SessionLookups++
		source, found, err := p.lookup(ctx, fullID)
		if err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("lookup live activity session %q: %w", fullID, err))
			if _, hot := p.hot[fullID]; !hot {
				p.addRetry(fullID, hint.target, now, hint.lastHint)
			}
			continue
		}
		if !found || source.Path == "" {
			if _, hot := p.hot[fullID]; !hot {
				p.addRetry(fullID, hint.target, now, hint.lastHint)
			}
			continue
		}
		p.setHot(fullID, hint.target, source, hint.lastHint)
	}

	for _, fullID := range sortedLiveActivityKeys(p.retries) {
		retry := p.retries[fullID]
		if now.Sub(retry.firstSeen) >= liveActivityRetryTTL {
			delete(p.retries, fullID)
			continue
		}
		if _, ok := attempted[fullID]; ok {
			continue
		}
		stats.SessionLookups++
		source, found, err := p.lookup(ctx, fullID)
		if err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("retry live activity session %q: %w", fullID, err))
			continue
		}
		if found && source.Path != "" {
			p.setHot(fullID, retry.target, source, retry.lastHint)
		}
	}

	p.expireHot(now)
	if evicted := p.enforceBounds(); evicted > 0 {
		p.logThrottled("state-overflow", now,
			"live activity state exceeded bounded capacity: evicted=%d entries=%d path_bytes=%d",
			evicted, len(p.hot)+len(p.retries), p.hotPathBytes())
	}

	type observedSource struct {
		size    int64
		mtimeNS int64
	}
	observed := make(map[string]observedSource)
	changedPaths := make(map[string]struct{})
	for _, fullID := range sortedLiveActivityKeys(p.hot) {
		entry := p.hot[fullID]
		stats.SourceStats++
		info, err := os.Stat(entry.source.Path)
		if errors.Is(err, os.ErrNotExist) {
			delete(p.hot, fullID)
			continue
		}
		if err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("stat live activity source %q: %w", entry.source.Path, err))
			continue
		}
		if entry.source.HasStoredStat &&
			entry.source.StoredSize == info.Size() &&
			entry.source.StoredMTimeNS == info.ModTime().UnixNano() {
			continue
		}
		entry.lastActivity = now
		entry.pending = true
		path := filepath.Clean(entry.source.Path)
		changedPaths[path] = struct{}{}
		observed[path] = observedSource{
			size:    info.Size(),
			mtimeNS: info.ModTime().UnixNano(),
		}
	}

	paths := sortedLiveActivityKeys(changedPaths)
	stats.SyncPaths = len(paths)
	if len(paths) > 0 {
		if err := p.syncPaths(ctx, paths); err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("sync live activity sources: %w", err))
		} else {
			for _, entry := range p.hot {
				info, ok := observed[filepath.Clean(entry.source.Path)]
				if !ok {
					continue
				}
				entry.source.StoredSize = info.size
				entry.source.StoredMTimeNS = info.mtimeNS
				entry.source.HasStoredStat = true
				entry.pending = false
			}
		}
	}

	err := errors.Join(pollErrors...)
	if err != nil {
		p.logThrottled("poll-error", now,
			"live activity poll encountered %d bounded errors: first=%v",
			len(pollErrors), pollErrors[0])
	}
	return stats, err
}

func (p *LiveActivityPoller) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	_, _ = p.PollOnce(ctx, time.Now())
	ticker := time.NewTicker(liveActivityPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_, _ = p.PollOnce(ctx, now)
		}
	}
}

func (p *LiveActivityPoller) addRetry(
	fullID string,
	target int,
	now time.Time,
	lastHint time.Time,
) {
	retry := p.retries[fullID]
	if retry == nil {
		retry = &liveActivityRetryEntry{
			target:    target,
			firstSeen: now,
		}
		p.retries[fullID] = retry
	}
	retry.target = target
	retry.lastHint = lastHint
}

func (p *LiveActivityPoller) setHot(
	fullID string,
	target int,
	source LiveActivitySource,
	lastActivity time.Time,
) {
	source.Path = filepath.Clean(source.Path)
	p.hot[fullID] = &liveActivityHotEntry{
		target:       target,
		source:       source,
		lastActivity: lastActivity,
	}
	delete(p.retries, fullID)
}

func (p *LiveActivityPoller) expireHot(now time.Time) {
	for fullID, entry := range p.hot {
		if now.Sub(entry.lastActivity) >= liveActivityHotTTL {
			delete(p.hot, fullID)
		}
	}
}

func (p *LiveActivityPoller) enforceBounds() int {
	pathBytes := p.hotPathBytes()
	if len(p.hot)+len(p.retries) <= liveActivityMaxEntries &&
		pathBytes <= liveActivityMaxPathBytes {
		return 0
	}
	type candidate struct {
		id       string
		activity time.Time
		hot      bool
		pending  bool
		pathSize int
	}
	candidates := make([]candidate, 0, len(p.hot)+len(p.retries))
	for id, entry := range p.hot {
		candidates = append(candidates, candidate{
			id:       id,
			activity: entry.lastActivity,
			hot:      true,
			pending:  entry.pending,
			pathSize: len(entry.source.Path),
		})
	}
	for id, entry := range p.retries {
		candidates = append(candidates, candidate{
			id: id, activity: entry.lastHint,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].pending != candidates[j].pending {
			return !candidates[i].pending
		}
		if candidates[i].activity.Equal(candidates[j].activity) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].activity.Before(candidates[j].activity)
	})

	evicted := 0
	for _, oldest := range candidates {
		if len(p.hot)+len(p.retries) <= liveActivityMaxEntries &&
			pathBytes <= liveActivityMaxPathBytes {
			break
		}
		if oldest.hot {
			delete(p.hot, oldest.id)
			pathBytes -= oldest.pathSize
		} else {
			delete(p.retries, oldest.id)
		}
		evicted++
	}
	return evicted
}

func (p *LiveActivityPoller) hotPathBytes() int {
	total := 0
	for _, entry := range p.hot {
		total += len(entry.source.Path)
	}
	return total
}

func (p *LiveActivityPoller) logThrottled(
	key string,
	now time.Time,
	format string,
	args ...any,
) {
	if p.logf == nil {
		return
	}
	if last := p.logged[key]; !last.IsZero() &&
		now.Sub(last) < liveActivityLogInterval {
		return
	}
	p.logged[key] = now
	p.logf(format, args...)
}

func sortedLiveActivityKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
