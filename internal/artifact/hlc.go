package artifact

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	metadataHLCStateKey        = "artifact_metadata_hlc"
	defaultMetadataHLCMaxDrift = 5 * time.Minute
	// hlcWallLayout deliberately omits the ":" separators of RFC3339 so the
	// rendered timestamp is safe to embed directly in artifact filenames.
	// Windows forbids ":" in path components, and metadata event files are
	// named after the HLC. Fixed-width fields keep the result lexicographically
	// sortable and round-trippable through ParseHLCTimestamp.
	hlcWallLayout   = "2006-01-02T150405.000000000Z"
	hlcLogicalWidth = 20
)

// ErrHLCDrift identifies a valid remote or persisted timestamp that cannot be
// observed yet because it exceeds the configured wall-clock drift bound.
var ErrHLCDrift = errors.New("metadata HLC exceeds drift bound")

type hlcStateStore interface {
	GetSyncState(key string) (string, error)
	SetSyncState(key, value string) error
}

// HLCTimestamp is a hybrid logical clock value for metadata events.
type HLCTimestamp struct {
	WallTime time.Time
	Logical  uint64
}

// String formats the timestamp in a lexicographically sortable form.
func (t HLCTimestamp) String() string {
	return fmt.Sprintf(
		"%s-%0*d",
		normalizeHLCWallTime(t.WallTime).Format(hlcWallLayout),
		hlcLogicalWidth,
		t.Logical,
	)
}

// ParseHLCTimestamp parses a timestamp produced by HLCTimestamp.String.
func ParseHLCTimestamp(s string) (HLCTimestamp, error) {
	idx := strings.LastIndex(s, "-")
	if idx < 0 {
		return HLCTimestamp{}, fmt.Errorf("invalid HLC timestamp %q: missing logical counter", s)
	}
	wallPart := s[:idx]
	logicalPart := s[idx+1:]
	if len(logicalPart) != hlcLogicalWidth || !isDecimal(logicalPart) {
		return HLCTimestamp{}, fmt.Errorf("invalid HLC timestamp %q: logical counter must be %d digits", s, hlcLogicalWidth)
	}
	wall, err := time.Parse(hlcWallLayout, wallPart)
	if err != nil {
		return HLCTimestamp{}, fmt.Errorf("invalid HLC timestamp %q: %w", s, err)
	}
	logical, err := strconv.ParseUint(logicalPart, 10, 64)
	if err != nil {
		return HLCTimestamp{}, fmt.Errorf("invalid HLC timestamp %q: %w", s, err)
	}
	return HLCTimestamp{
		WallTime: normalizeHLCWallTime(wall),
		Logical:  logical,
	}, nil
}

// Compare returns -1, 0, or 1 when t is ordered before, equal to, or after other.
func (t HLCTimestamp) Compare(other HLCTimestamp) int {
	wall := normalizeHLCWallTime(t.WallTime)
	otherWall := normalizeHLCWallTime(other.WallTime)
	switch {
	case wall.Before(otherWall):
		return -1
	case wall.After(otherWall):
		return 1
	case t.Logical < other.Logical:
		return -1
	case t.Logical > other.Logical:
		return 1
	default:
		return 0
	}
}

// OrderingKey appends a deterministic tie-breaker, usually the artifact hash.
func (t HLCTimestamp) OrderingKey(tieBreaker string) string {
	return t.String() + "-" + tieBreaker
}

// HLCClockOptions configures a persisted metadata HLC clock.
type HLCClockOptions struct {
	StateKey string
	Now      func() time.Time
	MaxDrift time.Duration
}

// HLCClock persists a monotonic hybrid logical clock in the sync-state store.
type HLCClock struct {
	mu       sync.Mutex
	store    hlcStateStore
	stateKey string
	now      func() time.Time
	maxDrift time.Duration
}

// NewHLCClock returns a persisted metadata HLC clock.
func NewHLCClock(store hlcStateStore, opts HLCClockOptions) *HLCClock {
	stateKey := opts.StateKey
	if stateKey == "" {
		stateKey = metadataHLCStateKey
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxDrift := opts.MaxDrift
	if maxDrift <= 0 {
		maxDrift = defaultMetadataHLCMaxDrift
	}
	return &HLCClock{
		store:    store,
		stateKey: stateKey,
		now:      now,
		maxDrift: maxDrift,
	}
}

// Next returns and persists the next local metadata-event timestamp.
func (c *HLCClock) Next() (HLCTimestamp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	last, ok, err := c.load()
	if err != nil {
		return HLCTimestamp{}, err
	}
	now := c.currentWallTime()
	if ok {
		if err := c.checkPersistedDrift(last, now); err != nil {
			return HLCTimestamp{}, err
		}
		if !now.After(last.WallTime) {
			next := HLCTimestamp{WallTime: last.WallTime, Logical: last.Logical + 1}
			return next, c.persist(next)
		}
	}
	next := HLCTimestamp{WallTime: now}
	return next, c.persist(next)
}

// Observe returns and persists a timestamp that is after the local and remote HLCs.
func (c *HLCClock) Observe(remote HLCTimestamp) (HLCTimestamp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	last, ok, err := c.load()
	if err != nil {
		return HLCTimestamp{}, err
	}
	now := c.currentWallTime()
	remote = HLCTimestamp{
		WallTime: normalizeHLCWallTime(remote.WallTime),
		Logical:  remote.Logical,
	}
	if ok {
		if err := c.checkPersistedDrift(last, now); err != nil {
			return HLCTimestamp{}, err
		}
	}
	if remote.WallTime.After(now.Add(c.maxDrift)) {
		return HLCTimestamp{}, fmt.Errorf(
			"%w: remote HLC wall time %s is more than %s ahead of local time %s",
			ErrHLCDrift,
			remote.WallTime.Format(hlcWallLayout),
			c.maxDrift,
			now.Format(hlcWallLayout),
		)
	}

	next := mergeHLC(HLCTimestamp{WallTime: now}, last, ok, remote)
	return next, c.persist(next)
}

func (c *HLCClock) load() (HLCTimestamp, bool, error) {
	if c.store == nil {
		return HLCTimestamp{}, false, errors.New("HLC state store is required")
	}
	raw, err := c.store.GetSyncState(c.stateKey)
	if err != nil {
		return HLCTimestamp{}, false, fmt.Errorf("reading HLC state: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return HLCTimestamp{}, false, nil
	}
	stamp, err := ParseHLCTimestamp(raw)
	if err != nil {
		return HLCTimestamp{}, false, fmt.Errorf("reading HLC state: %w", err)
	}
	return stamp, true, nil
}

func (c *HLCClock) persist(stamp HLCTimestamp) error {
	if err := c.store.SetSyncState(c.stateKey, stamp.String()); err != nil {
		return fmt.Errorf("persisting HLC state: %w", err)
	}
	return nil
}

func (c *HLCClock) currentWallTime() time.Time {
	return normalizeHLCWallTime(c.now())
}

func (c *HLCClock) checkPersistedDrift(last HLCTimestamp, now time.Time) error {
	if last.WallTime.After(now.Add(c.maxDrift)) {
		return fmt.Errorf(
			"%w: persisted HLC wall time %s is more than %s ahead of local time %s",
			ErrHLCDrift,
			last.WallTime.Format(hlcWallLayout),
			c.maxDrift,
			now.Format(hlcWallLayout),
		)
	}
	return nil
}

func mergeHLC(now, last HLCTimestamp, hasLast bool, remote HLCTimestamp) HLCTimestamp {
	maxWall := now.WallTime
	if hasLast && last.WallTime.After(maxWall) {
		maxWall = last.WallTime
	}
	if remote.WallTime.After(maxWall) {
		maxWall = remote.WallTime
	}

	next := HLCTimestamp{WallTime: maxWall}
	if hasLast && last.WallTime.Equal(maxWall) {
		next.Logical = last.Logical
	}
	if remote.WallTime.Equal(maxWall) && remote.Logical > next.Logical {
		next.Logical = remote.Logical
	}
	if now.WallTime.Equal(maxWall) && next.Logical == 0 {
		return next
	}
	next.Logical++
	return next
}

func normalizeHLCWallTime(t time.Time) time.Time {
	return t.UTC().Round(0)
}

func isDecimal(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
