// ABOUTME: Rate-limit request/response types and the thin translation
// ABOUTME: between db.RateLimitSnapshot rows and the API shape.
package service

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// RateLimitFilterRequest is the transport-neutral filter shared by the
// current-snapshot and history endpoints.
type RateLimitFilterRequest struct {
	// Vendor narrows to one vendor ("codex" today); empty matches every
	// vendor the table holds.
	Vendor string `json:"vendor,omitempty"`
	// AccountID narrows to one account. Reserved for a future
	// account-keyed vendor; ignored for Codex rows, which carry no
	// account identity.
	AccountID string `json:"accountId,omitempty"`
	Machine   string `json:"machine,omitempty"`
	// Agent is the same comma-separated agent selection the shared
	// session filters use (see db.RateLimitAgentMatchesVendor). It is
	// accepted for backward compatibility with the original Codex-only
	// filter name; it behaves like Vendor when Vendor is unset.
	Agent string `json:"agent,omitempty"`
}

// RateLimitHistoryRequest narrows RateLimitFilterRequest to a time range
// and optionally one limit_id/window_kind, for a used_percent time series.
// Since and Until are RFC3339 (or RFC3339Nano) timestamps, parsed and
// normalized to UTC by db.RateLimitSnapshotHistory: Since is an inclusive
// lower bound and Until is an EXCLUSIVE upper bound, so a caller wanting
// a whole local day sends the next day's local midnight as Until rather
// than that day's 23:59:59.
type RateLimitHistoryRequest struct {
	RateLimitFilterRequest
	LimitID    string `json:"limit_id,omitempty"`
	WindowKind string `json:"window_kind,omitempty"`
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	// MaxPoints bounds the number of points returned; a range with more
	// matching observations than this is downsampled to one per time
	// bucket (see db.RateLimitHistoryFilter.MaxPoints). <= 0 uses the
	// database layer's default.
	MaxPoints int `json:"max_points,omitempty"`
}

// RateLimitWindow is the API shape for one rate-limit window snapshot:
// which vendor reported it, how much of the window is used, when it
// resets, and (Codex only) the plan type and credit balance reported
// alongside it.
type RateLimitWindow struct {
	Vendor       string `json:"vendor"`
	AccountID    string `json:"accountId,omitempty"`
	AccountLabel string `json:"accountLabel,omitempty"`
	Machine      string `json:"machine"`
	LimitID      string `json:"limitId"`
	LimitName    string `json:"limitName,omitempty"`
	PlanType     string `json:"planType,omitempty"`
	WindowKind   string `json:"windowKind"`

	UsedPercent float64 `json:"usedPercent"`
	// WindowMinutes is omitted when Codex did not report this window's
	// duration on any observation on record (db.RateLimitSnapshot.
	// WindowMinutes == 0, which never occurs for a genuine duration): the
	// frontend card falls back to a window-kind label instead of showing
	// a placeholder duration.
	WindowMinutes *int `json:"windowMinutes,omitempty"`
	// ResetsAt is unix seconds, omitted when the vendor did not report a
	// reset time for this window (kept nullable rather than flattened
	// to 0, so an unknown reset time is never displayed as if the
	// window resets at the unix epoch).
	ResetsAt *int64 `json:"resetsAt,omitempty"`

	CreditsHas       bool   `json:"creditsHas"`
	CreditsUnlimited bool   `json:"creditsUnlimited"`
	CreditsBalance   string `json:"creditsBalance,omitempty"`

	// ScopeLabel and Details are reserved for a future vendor whose
	// rate-limit source reports a scoped or free-form extra shape; both
	// are always empty for Codex.
	ScopeLabel string `json:"scopeLabel,omitempty"`
	Details    string `json:"details,omitempty"`

	RateLimitReachedType string `json:"rateLimitReachedType,omitempty"`
	ObservedAt           string `json:"observedAt"`
	SessionID            string `json:"sessionId,omitempty"`
}

func rateLimitWindowFromRow(row db.RateLimitSnapshot) RateLimitWindow {
	vendor := row.Vendor
	if vendor == "" {
		vendor = "codex"
	}
	var windowMinutes *int
	if row.WindowMinutes != 0 {
		windowMinutes = &row.WindowMinutes
	}
	return RateLimitWindow{
		Vendor:               vendor,
		AccountID:            row.AccountID,
		AccountLabel:         row.AccountLabel,
		Machine:              row.Machine,
		LimitID:              row.LimitID,
		LimitName:            row.LimitName,
		PlanType:             row.PlanType,
		WindowKind:           row.WindowKind,
		UsedPercent:          row.UsedPercent,
		WindowMinutes:        windowMinutes,
		ResetsAt:             row.ResetsAt,
		CreditsHas:           row.CreditsHas,
		CreditsUnlimited:     row.CreditsUnlimited,
		CreditsBalance:       row.CreditsBalance,
		ScopeLabel:           row.ScopeLabel,
		Details:              row.Details,
		RateLimitReachedType: row.RateLimitReachedType,
		ObservedAt:           row.ObservedAt,
		SessionID:            row.SessionID,
	}
}

// RateLimitCurrent returns the latest snapshot per (vendor, machine,
// account, limit_id, window_kind) group.
func RateLimitCurrent(
	ctx context.Context, store db.Store, req RateLimitFilterRequest,
) ([]RateLimitWindow, error) {
	rows, err := store.LatestRateLimitSnapshots(ctx, db.RateLimitFilter{
		Vendor:    strings.TrimSpace(req.Vendor),
		AccountID: strings.TrimSpace(req.AccountID),
		Machine:   strings.TrimSpace(req.Machine),
		Agent:     strings.TrimSpace(req.Agent),
	})
	if err != nil {
		return nil, err
	}
	out := make([]RateLimitWindow, len(rows))
	for i, row := range rows {
		out[i] = rateLimitWindowFromRow(row)
	}
	return out, nil
}

// RateLimitHistory returns a time-ordered series of snapshots for
// charting used_percent over the requested range.
func RateLimitHistory(
	ctx context.Context, store db.Store, req RateLimitHistoryRequest,
) ([]RateLimitWindow, error) {
	rows, err := store.RateLimitSnapshotHistory(ctx, db.RateLimitHistoryFilter{
		Vendor:     strings.TrimSpace(req.Vendor),
		AccountID:  strings.TrimSpace(req.AccountID),
		Machine:    strings.TrimSpace(req.Machine),
		Agent:      strings.TrimSpace(req.Agent),
		LimitID:    strings.TrimSpace(req.LimitID),
		WindowKind: strings.TrimSpace(req.WindowKind),
		Since:      strings.TrimSpace(req.Since),
		Until:      strings.TrimSpace(req.Until),
		MaxPoints:  req.MaxPoints,
	})
	if err != nil {
		return nil, err
	}
	out := make([]RateLimitWindow, len(rows))
	for i, row := range rows {
		out[i] = rateLimitWindowFromRow(row)
	}
	return out, nil
}
