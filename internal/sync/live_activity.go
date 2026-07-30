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
	liveActivityPollInterval   = 30 * time.Second
	liveActivityHotTTL         = 24 * time.Hour
	liveActivityRetryTTL       = 2 * time.Minute
	liveActivityLogInterval    = 5 * time.Minute
	liveActivityMaxEntries     = 8192
	liveActivityMaxPathBytes   = 2 << 20
	liveActivityMaxCursors     = 256
	liveActivityMaxCursorBytes = liveActivityMaxPathBytes
)

type LiveActivitySource struct {
	Path              string
	StoredSize        int64
	StoredMTimeNS     int64
	StoredInode       int64
	StoredDevice      int64
	HasStoredStat     bool
	HasStoredIdentity bool
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
	target           int
	source           LiveActivitySource
	lastActivity     time.Time
	pending          bool
	refreshRetry     *liveActivityRetryEntry
	retryPendingStat bool
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

	nextHintSource int
	cursorSequence uint64
}

func cloneActivityHintCursor(
	cursor *activityHintCursor,
) activityHintCursor {
	return *cursor
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
	pollErrors := make([]error, 0)
	hinted := make(map[string]liveActivityRetryEntry)
	bytesRemaining := activityHintMaxReadBytes
	recordsRemaining := activityHintMaxIDsPerPoll
	type pollSource struct {
		targetIndex int
		target      LiveActivityTarget
		source      parser.ActivityHintSource
	}
	sources := make([]pollSource, 0)
	for targetIndex, target := range p.targets {
		for _, source := range target.Sources {
			sources = append(sources, pollSource{
				targetIndex: targetIndex,
				target:      target,
				source:      source,
			})
		}
	}
	start := 0
	if len(sources) > 0 {
		start = p.nextHintSource % len(sources)
	}
	processedSources := 0
	deferredSource := -1
	for offset := range min(len(sources), liveActivityMaxCursors) {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if bytesRemaining == 0 || recordsRemaining == 0 {
			break
		}
		sourceIndex := (start + offset) % len(sources)
		current := sources[sourceIndex]
		processedSources++
		stats.HintFiles++
		key := liveActivityCursorKey{
			target: current.targetIndex,
			path:   current.source.Path,
		}
		cursor := p.cursors[key]
		if cursor == nil {
			cursor = &activityHintCursor{}
			p.cursors[key] = cursor
		}
		cursorBefore := cloneActivityHintCursor(cursor)
		byteBudget := bytesRemaining
		recordBudget := recordsRemaining
		result, err := readActivityHints(
			ctx, current.source, current.target.Hints, cursor, now,
			byteBudget, recordBudget,
		)
		stats.HintBytes += result.BytesRead
		if err != nil {
			*cursor = cursorBefore
			p.markCursorUsed(cursor)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return stats, ctxErr
			}
			pollErrors = append(pollErrors, err)
			continue
		}
		if err := ctx.Err(); err != nil {
			*cursor = cursorBefore
			p.markCursorUsed(cursor)
			return stats, err
		}
		bytesRemaining -= result.BytesRead
		recordsRemaining -= result.RecordsDecoded
		if result.Overflow {
			p.logThrottled("hint-overflow", now,
				"live activity hint input exceeded a bounded poll: path=%q bytes=%d records=%d ids=%d",
				current.source.Path, result.BytesRead,
				result.RecordsDecoded, len(result.Hints))
		}
		if byteBudget < activityHintMaxReadBytes && result.ByteOverflow ||
			recordBudget < activityHintMaxIDsPerPoll &&
				result.RecordOverflow {
			*cursor = cursorBefore
			p.markCursorUsed(cursor)
			deferredSource = sourceIndex
			break
		}
		p.markCursorUsed(cursor)
		for _, hint := range result.Hints {
			fullID := current.target.Provider.Definition().IDPrefix +
				hint.RawSessionID
			previous, exists := hinted[fullID]
			if !exists || hint.Timestamp.After(previous.lastHint) {
				hinted[fullID] = liveActivityRetryEntry{
					target:   current.targetIndex,
					lastHint: hint.Timestamp,
				}
			}
		}
	}
	if len(sources) > 0 {
		switch {
		case deferredSource >= 0:
			p.nextHintSource = deferredSource
		case processedSources > 0:
			p.nextHintSource = (start + processedSources) % len(sources)
		}
	}
	if evicted := p.enforceCursorBounds(); evicted > 0 {
		p.logThrottled("cursor-overflow", now,
			"live activity hint cursors exceeded bounded capacity: evicted=%d entries=%d bytes=%d",
			evicted, len(p.cursors), p.cursorBytes())
	}

	attempted := make(map[string]struct{}, len(hinted))
	for fullID, hint := range hinted {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		attempted[fullID] = struct{}{}
		stats.SessionLookups++
		source, found, err := p.lookup(ctx, fullID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, ctxErr
		}
		if err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("lookup live activity session %q: %w", fullID, err))
			if _, hot := p.hot[fullID]; hot {
				p.addHotRefreshRetry(
					fullID, hint.target, now, hint.lastHint,
				)
			} else {
				p.addRetry(fullID, hint.target, now, hint.lastHint)
			}
			continue
		}
		if !found || source.Path == "" {
			if _, hot := p.hot[fullID]; hot {
				p.addHotRefreshRetry(
					fullID, hint.target, now, hint.lastHint,
				)
			} else {
				p.addRetry(fullID, hint.target, now, hint.lastHint)
			}
			continue
		}
		p.setHot(fullID, hint.target, source, hint.lastHint)
	}

	for fullID, retry := range p.retries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if now.Sub(retry.firstSeen) >= liveActivityRetryTTL {
			delete(p.retries, fullID)
			continue
		}
		if _, ok := attempted[fullID]; ok {
			continue
		}
		attempted[fullID] = struct{}{}
		stats.SessionLookups++
		source, found, err := p.lookup(ctx, fullID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, ctxErr
		}
		if err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("retry live activity session %q: %w", fullID, err))
			continue
		}
		if found && source.Path != "" {
			p.setHot(fullID, retry.target, source, retry.lastHint)
		}
	}

	for fullID, entry := range p.hot {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		retry := entry.refreshRetry
		if retry == nil {
			continue
		}
		if now.Sub(retry.firstSeen) >= liveActivityRetryTTL {
			entry.refreshRetry = nil
			continue
		}
		if _, ok := attempted[fullID]; ok {
			continue
		}
		attempted[fullID] = struct{}{}
		stats.SessionLookups++
		source, found, err := p.lookup(ctx, fullID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, ctxErr
		}
		if err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("refresh live activity session %q: %w", fullID, err))
			continue
		}
		if found && source.Path != "" {
			p.setHot(fullID, retry.target, source, retry.lastHint)
		}
	}

	if err := ctx.Err(); err != nil {
		return stats, err
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
		inode   int64
		device  int64
	}
	observed := make(map[string]observedSource)
	changedPaths := make(map[string]struct{})
	for fullID, entry := range p.hot {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.SourceStats++
		info, err := os.Stat(entry.source.Path)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, ctxErr
		}
		if errors.Is(err, os.ErrNotExist) {
			delete(p.hot, fullID)
			if entry.refreshRetry != nil {
				p.retries[fullID] = entry.refreshRetry
			}
			if hint, ok := hinted[fullID]; ok {
				p.addRetry(
					fullID, hint.target, now, hint.lastHint,
				)
			}
			continue
		}
		if err != nil {
			pollErrors = append(pollErrors,
				fmt.Errorf("stat live activity source %q: %w", entry.source.Path, err))
			continue
		}
		if entry.retryPendingStat {
			entry.refreshRetry = nil
			entry.retryPendingStat = false
		}
		inode, device := getFileIdentity(entry.source.Path, info)
		if entry.source.HasStoredStat &&
			entry.source.StoredSize == info.Size() &&
			entry.source.StoredMTimeNS == info.ModTime().UnixNano() &&
			(!entry.source.HasStoredIdentity ||
				entry.source.StoredInode == inode &&
					entry.source.StoredDevice == device) {
			continue
		}
		entry.lastActivity = now
		entry.pending = true
		path := filepath.Clean(entry.source.Path)
		changedPaths[path] = struct{}{}
		observed[path] = observedSource{
			size:    info.Size(),
			mtimeNS: info.ModTime().UnixNano(),
			inode:   inode,
			device:  device,
		}
	}

	if err := ctx.Err(); err != nil {
		return stats, err
	}
	paths := sortedLiveActivityKeys(changedPaths)
	stats.SyncPaths = len(paths)
	if len(paths) > 0 {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := p.syncPaths(ctx, paths); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return stats, ctxErr
			}
			pollErrors = append(pollErrors,
				fmt.Errorf("sync live activity sources: %w", err))
		} else {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			for _, entry := range p.hot {
				info, ok := observed[filepath.Clean(entry.source.Path)]
				if !ok {
					continue
				}
				entry.source.StoredSize = info.size
				entry.source.StoredMTimeNS = info.mtimeNS
				entry.source.StoredInode = info.inode
				entry.source.StoredDevice = info.device
				entry.source.HasStoredStat = true
				entry.source.HasStoredIdentity = true
				entry.pending = false
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return stats, err
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
			lastHint:  lastHint,
		}
		p.retries[fullID] = retry
	} else if lastHint.After(retry.lastHint) {
		retry.firstSeen = now
		retry.target = target
		retry.lastHint = lastHint
	}
}

func (p *LiveActivityPoller) addHotRefreshRetry(
	fullID string,
	target int,
	now time.Time,
	lastHint time.Time,
) {
	entry := p.hot[fullID]
	if entry == nil {
		return
	}
	retry := entry.refreshRetry
	if retry == nil {
		retry = &liveActivityRetryEntry{
			target:    target,
			firstSeen: now,
			lastHint:  lastHint,
		}
		entry.refreshRetry = retry
	} else if lastHint.After(retry.lastHint) {
		retry.firstSeen = now
		retry.target = target
		retry.lastHint = lastHint
	}
	entry.retryPendingStat = false
}

func (p *LiveActivityPoller) setHot(
	fullID string,
	target int,
	source LiveActivitySource,
	lastActivity time.Time,
) {
	source.Path = filepath.Clean(source.Path)
	var refreshRetry *liveActivityRetryEntry
	if entry := p.hot[fullID]; entry != nil {
		if entry.lastActivity.After(lastActivity) {
			lastActivity = entry.lastActivity
		}
		if retry := entry.refreshRetry; retry != nil &&
			retry.lastHint.After(lastActivity) {
			lastActivity = retry.lastHint
		}
		refreshRetry = entry.refreshRetry
	}
	if retry := p.retries[fullID]; retry != nil {
		if retry.lastHint.After(lastActivity) {
			lastActivity = retry.lastHint
		}
		refreshRetry = retry
	}
	p.hot[fullID] = &liveActivityHotEntry{
		target:           target,
		source:           source,
		lastActivity:     lastActivity,
		refreshRetry:     refreshRetry,
		retryPendingStat: refreshRetry != nil,
	}
	delete(p.retries, fullID)
}

func (p *LiveActivityPoller) expireHot(now time.Time) {
	for fullID, entry := range p.hot {
		if now.Sub(entry.lastActivity) >= liveActivityHotTTL {
			if retry := entry.refreshRetry; retry != nil &&
				now.Sub(retry.firstSeen) < liveActivityRetryTTL {
				p.retries[fullID] = retry
			}
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

func (p *LiveActivityPoller) markCursorUsed(
	cursor *activityHintCursor,
) {
	p.cursorSequence++
	cursor.lastUsed = p.cursorSequence
}

func (p *LiveActivityPoller) enforceCursorBounds() int {
	retainedBytes := p.cursorBytes()
	if len(p.cursors) <= liveActivityMaxCursors &&
		retainedBytes <= liveActivityMaxCursorBytes {
		return 0
	}
	type candidate struct {
		key      liveActivityCursorKey
		lastUsed uint64
		bytes    int
	}
	candidates := make([]candidate, 0, len(p.cursors))
	for key, cursor := range p.cursors {
		candidates = append(candidates, candidate{
			key:      key,
			lastUsed: cursor.lastUsed,
			bytes:    len(key.path),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].lastUsed == candidates[j].lastUsed {
			if candidates[i].key.target == candidates[j].key.target {
				return candidates[i].key.path < candidates[j].key.path
			}
			return candidates[i].key.target < candidates[j].key.target
		}
		return candidates[i].lastUsed < candidates[j].lastUsed
	})

	evicted := 0
	for _, oldest := range candidates {
		if len(p.cursors) <= liveActivityMaxCursors &&
			retainedBytes <= liveActivityMaxCursorBytes {
			break
		}
		delete(p.cursors, oldest.key)
		retainedBytes -= oldest.bytes
		evicted++
	}
	return evicted
}

func (p *LiveActivityPoller) cursorBytes() int {
	total := 0
	for key := range p.cursors {
		total += len(key.path)
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
