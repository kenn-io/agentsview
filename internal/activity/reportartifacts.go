package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	DefaultSessionPageLimit = 200
	MaxSessionPageLimit     = 500
)

type ProgressPhase string

const (
	ProgressLoadingSessions  ProgressPhase = "loading_sessions"
	ProgressLoadingUsage     ProgressPhase = "loading_usage"
	ProgressScanningActivity ProgressPhase = "scanning_activity"
	ProgressFinalizing       ProgressPhase = "finalizing"
	ProgressDone             ProgressPhase = "done"
)

type Progress struct {
	Phase             ProgressPhase `json:"phase"`
	SessionsTotal     int           `json:"sessions_total"`
	SessionsProcessed int           `json:"sessions_processed"`
	RowsProcessed     int64         `json:"rows_processed"`
}

type ProgressFunc func(Progress)

// SourceProbe is a cheap monotonic summary used to reject stale report tokens
// before an expensive cache-miss rebuild. It is deliberately coarse: unrelated
// archive changes may force a harmless refresh, while the canonical artifact
// digest remains the final consistency check.
type SourceProbe struct {
	SessionCount              int64  `json:"session_count"`
	MaxSessionModified        string `json:"max_session_modified"`
	MaxDataVersion            int64  `json:"max_data_version"`
	MaxMessageID              int64  `json:"max_message_id"`
	MaxUsageID                int64  `json:"max_usage_id"`
	MaxPricingUpdated         string `json:"max_pricing_updated"`
	ProjectIdentityGeneration int64  `json:"project_identity_generation"`
}

type SessionSort string

const (
	SessionSortAgentMinutes SessionSort = "agent_minutes"
	SessionSortCost         SessionSort = "cost"
	SessionSortFirstActive  SessionSort = "first_active"
	SessionSortProject      SessionSort = "project"
	SessionSortAgent        SessionSort = "agent"
)

type SessionPageOptions struct {
	Limit     int
	Offset    int
	Sort      SessionSort
	Direction string
	Bucket    *int
}

// SessionPageOptionPresence records which cursor-bound options the caller
// explicitly supplied. Omitted options inherit their values from a
// continuation cursor; explicit values must match it.
type SessionPageOptionPresence struct {
	Sort      bool
	Direction bool
	Bucket    bool
}

type SessionPage struct {
	Sessions   []SessionRow `json:"sessions"`
	Next       int          `json:"-"`
	HasNext    bool         `json:"-"`
	NextCursor string       `json:"next_cursor,omitempty"`
	Total      int          `json:"total"`
}

func NormalizeSessionPageOptions(options SessionPageOptions) (SessionPageOptions, error) {
	if options.Limit <= 0 {
		options.Limit = DefaultSessionPageLimit
	}
	if options.Limit > MaxSessionPageLimit {
		options.Limit = MaxSessionPageLimit
	}
	if options.Offset < 0 {
		return SessionPageOptions{}, fmt.Errorf("session page offset must be non-negative")
	}
	if options.Sort == "" {
		options.Sort = SessionSortAgentMinutes
	}
	switch options.Sort {
	case SessionSortAgentMinutes, SessionSortCost, SessionSortFirstActive,
		SessionSortProject, SessionSortAgent:
	default:
		return SessionPageOptions{}, fmt.Errorf("invalid activity session sort %q", options.Sort)
	}
	if options.Direction == "" {
		options.Direction = "desc"
	}
	if options.Direction != "asc" && options.Direction != "desc" {
		return SessionPageOptions{}, fmt.Errorf(
			"invalid activity session direction %q", options.Direction,
		)
	}
	return options, nil
}

// ResolveSessionPageOptions applies cursor state before request defaults. This
// lets a cursor stand alone while still rejecting attempts to change its sort
// or bucket membership midway through a deterministic ordering.
func ResolveSessionPageOptions(
	requested SessionPageOptions,
	continuation *SessionPageOptions,
	presence SessionPageOptionPresence,
) (SessionPageOptions, error) {
	if continuation == nil {
		return NormalizeSessionPageOptions(requested)
	}
	normalizedContinuation, err := NormalizeSessionPageOptions(*continuation)
	if err != nil {
		return SessionPageOptions{}, err
	}
	if presence.Sort && requested.Sort != normalizedContinuation.Sort {
		return SessionPageOptions{}, fmt.Errorf("activity session sort does not match cursor")
	}
	if presence.Direction && requested.Direction != normalizedContinuation.Direction {
		return SessionPageOptions{}, fmt.Errorf("activity session direction does not match cursor")
	}
	if presence.Bucket && !sameSessionBucket(requested.Bucket, normalizedContinuation.Bucket) {
		return SessionPageOptions{}, fmt.Errorf("activity session bucket does not match cursor")
	}
	requested.Sort = normalizedContinuation.Sort
	requested.Direction = normalizedContinuation.Direction
	requested.Bucket = normalizedContinuation.Bucket
	return NormalizeSessionPageOptions(requested)
}

func sameSessionBucket(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// PageSessions applies backend-independent membership and deterministic total
// ordering. Session ID is always the final ascending tie-break.
func PageSessions(
	rows []SessionRow,
	membership map[string]BucketMembership,
	options SessionPageOptions,
) (SessionPage, error) {
	options, err := NormalizeSessionPageOptions(options)
	if err != nil {
		return SessionPage{}, err
	}
	order, err := OrderSessions(rows, membership, options)
	if err != nil {
		return SessionPage{}, err
	}
	return PageSessionsFromOrder(rows, order, options)
}

// OrderSessions returns a deterministic index permutation suitable for reuse
// by the bounded server cache.
func OrderSessions(
	rows []SessionRow,
	membership map[string]BucketMembership,
	options SessionPageOptions,
) ([]int, error) {
	options, err := NormalizeSessionPageOptions(options)
	if err != nil {
		return nil, err
	}
	order := make([]int, 0, len(rows))
	for index, row := range rows {
		if options.Bucket != nil && !membership[row.SessionID].Contains(*options.Bucket) {
			continue
		}
		order = append(order, index)
	}
	slices.SortStableFunc(order, func(leftIndex, rightIndex int) int {
		left, right := rows[leftIndex], rows[rightIndex]
		if comparison := compareTimingPresence(left, right, options.Sort); comparison != 0 {
			return comparison
		}
		comparison := compareSessionRows(left, right, options.Sort)
		if options.Direction == "desc" {
			comparison = -comparison
		}
		if comparison != 0 {
			return comparison
		}
		return strings.Compare(left.SessionID, right.SessionID)
	})
	return order, nil
}

// compareTimingPresence partitions untimed rows after timed rows before sort
// direction is applied. A nil agent-minutes/window value therefore stays at
// the bottom for both ascending and descending timing sorts.
func compareTimingPresence(left, right SessionRow, sortKey SessionSort) int {
	var leftNil, rightNil bool
	switch sortKey {
	case SessionSortAgentMinutes:
		leftNil, rightNil = left.AgentMinutes == nil, right.AgentMinutes == nil
	case SessionSortFirstActive:
		leftNil, rightNil = left.FirstActive == nil, right.FirstActive == nil
	default:
		return 0
	}
	if leftNil == rightNil {
		return 0
	}
	if leftNil {
		return 1
	}
	return -1
}

// PageSessionsFromOrder materializes one bounded page from a deterministic
// cached permutation. The order contains indexes into rows and is never exposed
// on the wire.
func PageSessionsFromOrder(
	rows []SessionRow, order []int, options SessionPageOptions,
) (SessionPage, error) {
	options, err := NormalizeSessionPageOptions(options)
	if err != nil {
		return SessionPage{}, err
	}
	page := SessionPage{Total: len(order)}
	if options.Offset >= len(order) {
		page.Sessions = []SessionRow{}
		return page, nil
	}
	end := min(options.Offset+options.Limit, len(order))
	page.Sessions = make([]SessionRow, 0, end-options.Offset)
	for _, index := range order[options.Offset:end] {
		if index < 0 || index >= len(rows) {
			return SessionPage{}, fmt.Errorf("activity session order index out of range")
		}
		page.Sessions = append(page.Sessions, rows[index])
	}
	if end < len(order) {
		page.Next, page.HasNext = end, true
	}
	return page, nil
}

func compareSessionRows(left, right SessionRow, sortKey SessionSort) int {
	switch sortKey {
	case SessionSortAgentMinutes:
		return compareOptionalFloat(left.AgentMinutes, right.AgentMinutes)
	case SessionSortCost:
		return compareInt64(left.Cost.Microdollars, right.Cost.Microdollars)
	case SessionSortFirstActive:
		return compareOptionalString(left.FirstActive, right.FirstActive)
	case SessionSortProject:
		return strings.Compare(left.Project, right.Project)
	case SessionSortAgent:
		return strings.Compare(left.Agent, right.Agent)
	default:
		return 0
	}
}

func compareOptionalFloat(left, right *float64) int {
	if left == nil || right == nil {
		switch {
		case left == nil && right == nil:
			return 0
		case left == nil:
			return -1
		default:
			return 1
		}
	}
	switch {
	case *left < *right:
		return -1
	case *left > *right:
		return 1
	default:
		return 0
	}
}

func compareOptionalString(left, right *string) int {
	if left == nil || right == nil {
		switch {
		case left == nil && right == nil:
			return 0
		case left == nil:
			return -1
		default:
			return 1
		}
	}
	return strings.Compare(*left, *right)
}

func compareInt64(left, right int64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// ArtifactDigest hashes only finalized compact artifacts. JSON sorts string
// map keys, making this stable across store row order and daemon restarts.
func ArtifactDigest(artifacts CandidateArtifacts) (string, error) {
	hash := sha256.New()
	if err := json.NewEncoder(hash).Encode(struct {
		Report     Report                      `json:"report"`
		Sessions   []SessionRow                `json:"sessions"`
		Membership map[string]BucketMembership `json:"membership"`
	}{artifacts.Report, artifacts.Sessions, artifacts.Membership}); err != nil {
		return "", fmt.Errorf("encoding activity report artifact digest: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// EstimatedArtifactBytes provides conservative cache accounting for the
// retained rows, strings, and actual-width membership bitmaps.
func EstimatedArtifactBytes(artifacts CandidateArtifacts) int64 {
	var size int64
	for _, row := range artifacts.Sessions {
		size += 192
		size += int64(len(row.SessionID) + len(row.ProjectKey) + len(row.Title) +
			len(row.Project) + len(row.Agent) + len(row.PrimaryModel) +
			len(row.TimingQuality))
		for _, model := range row.Models {
			size += int64(len(model) + 16)
		}
	}
	for sessionID, bitmap := range artifacts.Membership {
		size += int64(len(sessionID) + len(bitmap)*8 + 32)
	}
	encodedReport, err := json.Marshal(artifacts.Report)
	if err == nil {
		size += int64(len(encodedReport))
	}
	return size
}
