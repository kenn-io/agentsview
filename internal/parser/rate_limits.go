package parser

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ParsedRateLimitSnapshot is one subscription rate-limit report an agent
// wrote into a session: how much of each limit window the account had used
// and when each window resets. Codex records one on its token_count events;
// see docs/internal/session-format-sources.md. A parser emits a snapshot
// only when the reported values change, so repeated identical reports
// collapse to the first.
type ParsedRateLimitSnapshot struct {
	// ObservedAt is the timestamp of the record that carried the report.
	ObservedAt time.Time
	// AccountKey is an opaque digest of the account ID the session recorded,
	// so windows from different accounts stay apart. Empty when the session
	// recorded no account.
	AccountKey string
	// LimitID names the limit as the agent reported it, for example "codex".
	LimitID string
	// PlanType is the plan the agent reported, for example "pro".
	PlanType  string
	Primary   *ParsedRateLimitWindow
	Secondary *ParsedRateLimitWindow
}

// ParsedRateLimitWindow is one window of a rate-limit report.
type ParsedRateLimitWindow struct {
	// UsedPercent is the share of the window already used, 0 to 100.
	UsedPercent float64
	// WindowMinutes is the window length the agent reported; nil when it
	// reported none.
	WindowMinutes *int64
	// ResetsAt is when the window resets; nil when the agent reported none.
	ResetsAt *time.Time
}

// rateLimitAccountKey returns the opaque account key stored for a recorded
// account ID: enough to tell accounts apart without keeping the ID itself.
func rateLimitAccountKey(accountID string) string {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(accountID))
	return hex.EncodeToString(sum[:8])
}

// parseCodexRateLimitWindow decodes one Codex RateLimitWindow, or nil when
// the window is absent or carries no used_percent.
func parseCodexRateLimitWindow(raw gjson.Result) *ParsedRateLimitWindow {
	if !raw.IsObject() {
		return nil
	}
	used := raw.Get("used_percent")
	if used.Type != gjson.Number || math.IsNaN(used.Num) || math.IsInf(used.Num, 0) {
		return nil
	}
	window := &ParsedRateLimitWindow{UsedPercent: used.Num}
	if minutes := raw.Get("window_minutes"); minutes.Type == gjson.Number {
		v := minutes.Int()
		window.WindowMinutes = &v
	}
	if resets := raw.Get("resets_at"); resets.Type == gjson.Number && resets.Int() > 0 {
		t := time.Unix(resets.Int(), 0).UTC()
		window.ResetsAt = &t
	}
	return window
}

// valueDigest identifies a snapshot's reported values, without its time, so
// repeated identical reports can be recognized.
func (s ParsedRateLimitSnapshot) valueDigest() [sha256.Size]byte {
	h := sha256.New()
	for _, str := range []string{s.AccountKey, s.LimitID, s.PlanType} {
		_ = binary.Write(h, binary.LittleEndian, uint32(len(str)))
		h.Write([]byte(str))
	}
	for _, w := range []*ParsedRateLimitWindow{s.Primary, s.Secondary} {
		if w == nil {
			h.Write([]byte{0})
			continue
		}
		h.Write([]byte{1})
		_ = binary.Write(h, binary.LittleEndian, math.Float64bits(w.UsedPercent))
		minutes, resets := int64(-1), int64(-1)
		if w.WindowMinutes != nil {
			minutes = *w.WindowMinutes
		}
		if w.ResetsAt != nil {
			resets = w.ResetsAt.Unix()
		}
		_ = binary.Write(h, binary.LittleEndian, minutes)
		_ = binary.Write(h, binary.LittleEndian, resets)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// observeRateLimits records the rate_limits object of a Codex token_count
// event when it reports at least one window and differs from the previous
// report. Only Codex itself is read: the forks sharing this parser have not
// been checked against their producers.
func (b *codexSessionBuilder) observeRateLimits(raw gjson.Result) {
	if b.agent != AgentCodex || !raw.IsObject() || b.sourceTimestamp.IsZero() {
		return
	}
	primary := parseCodexRateLimitWindow(raw.Get("primary"))
	secondary := parseCodexRateLimitWindow(raw.Get("secondary"))
	if primary == nil && secondary == nil {
		return
	}
	snap := ParsedRateLimitSnapshot{
		ObservedAt: b.sourceTimestamp.UTC(),
		AccountKey: b.accountKey,
		LimitID:    strings.Clone(raw.Get("limit_id").Str),
		PlanType:   strings.Clone(raw.Get("plan_type").Str),
		Primary:    primary,
		Secondary:  secondary,
	}
	digest := snap.valueDigest()
	if b.lastRateLimitSeen && digest == b.lastRateLimitDigest {
		return
	}
	b.lastRateLimitDigest = digest
	b.lastRateLimitSeen = true
	b.rateLimits = append(b.rateLimits, snap)
}
