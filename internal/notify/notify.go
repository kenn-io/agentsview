// Package notify decides which session changes are worth a desktop
// notification. It reuses the sync engine's parsed output: the
// archive's per-session termination_status (awaiting_user = a
// reliable end-of-turn signal) and next_ordinal cursor, so no new
// log collection or parsing happens here.
package notify

import (
	"slices"
	"time"
)

// Kind distinguishes the two notification shapes.
type Kind string

const (
	// KindTurnEnd fires when a session's termination_status became
	// "awaiting_user": the agent reached a clear stopping point
	// (Claude end_turn, Codex task_complete, ...) and is parked
	// waiting for the user. This is the reliable end-of-turn signal.
	KindTurnEnd Kind = "turn_end"

	// KindNewReply is the optional fallback for sessions without a
	// reliable end-of-turn signal (or while a turn is still
	// mid-flight with new assistant output). It never claims the
	// task is done — silence and pending tool calls are not
	// completion signals.
	KindNewReply Kind = "new_reply"
)

// Notification is one decided, deduplicated desktop notification.
type Notification struct {
	Kind      Kind   `json:"kind"`
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	Agent     string `json:"agent"`
	// Title and Body are pre-rendered display strings. Body is the
	// tail of the latest assistant message, so the notification
	// itself leaks no more than the transcript already shows.
	Title string `json:"title"`
	Body  string `json:"body"`
	// DeepLinkPath is the in-app route for the notification's
	// click target: the session scrolled to its last message.
	DeepLinkPath string `json:"deep_link_path"`
	CreatedAt    string `json:"created_at"`
}

// Snapshot is the archive-derived view of a session the decider
// needs. Populated by the store's candidate query.
type Snapshot struct {
	SessionID         string
	Project           string
	Agent             string
	DisplayName       string
	TerminationStatus string
	// NextOrdinal is the archive's next-message ordinal: a
	// monotone cursor over how much content has been parsed.
	NextOrdinal     int64
	LastRole        string
	LocalModifiedAt time.Time
	// RelationshipType is non-empty for sessions derived from a
	// parent (subagent, continuation, fork).
	RelationshipType string
	IsAutomated      bool
	// LastMessage is the tail of the newest message, used for the
	// notification body. May be empty.
	LastMessage string
}

// State is the persisted per-session dedup cursor. It lives in the
// archive, so restarts and SSE drops cannot re-notify for content
// that was already reported.
type State struct {
	// TurnEndOrdinal is the NextOrdinal value already reported as
	// an end-of-turn.
	TurnEndOrdinal int64 `json:"turn_end_ordinal,omitempty"`
	// ReplyNotifyOrdinal is the NextOrdinal value covered by the
	// most recent new-reply notification.
	ReplyNotifyOrdinal int64     `json:"reply_notify_ordinal,omitempty"`
	ReplyNotifiedAt    time.Time `json:"reply_notified_at,omitempty"`
}

// Config is the user-facing notification policy, mirrored from
// config.NotificationsConfig. Zero Agents/Projects means "all".
type Config struct {
	Enabled           bool
	NotifyNewReply    bool
	MergeWindow       time.Duration
	Agents            []string
	Projects          []string
	SuppressSubagents bool
}

// DefaultConfig is the initial policy when the config file has no
// notifications section.
func DefaultConfig() Config {
	return Config{
		Enabled:           false,
		NotifyNewReply:    false,
		MergeWindow:       time.Minute,
		Agents:            nil,
		Projects:          nil,
		SuppressSubagents: true,
	}
}

// Decider evaluates candidate snapshots against persisted dedup
// state. It is pure with respect to its inputs apart from the
// clock, which is injectable for tests.
type Decider struct {
	cfg func() Config
	now func() time.Time
}

// NewDecider builds a Decider. cfgFn must return the live policy;
// nowFn the wall clock (both injectable for tests).
func NewDecider(cfgFn func() Config, nowFn func() time.Time) *Decider {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Decider{cfg: cfgFn, now: nowFn}
}

// Decision is the outcome of evaluating one snapshot: either nil
// (nothing worth notifying) or a notification plus the updated
// state the caller must persist.
type Decision struct {
	Notification Notification
	State        State
}

// Decide evaluates one snapshot. readyAt is the daemon's readiness
// snapshot: sessions whose transcript was last modified before it
// belong to initial sync, history rebuilds, or metadata-only
// resync passes and must stay silent.
func (d *Decider) Decide(s Snapshot, st State, readyAt time.Time) *Decision {
	cfg := d.cfg()
	if !cfg.Enabled {
		return nil
	}
	if !s.LocalModifiedAt.After(readyAt) {
		return nil
	}
	if cfg.SuppressSubagents && s.RelationshipType == "subagent" {
		return nil
	}
	if s.IsAutomated {
		return nil
	}
	if len(cfg.Agents) > 0 && !slices.Contains(cfg.Agents, s.Agent) {
		return nil
	}
	if len(cfg.Projects) > 0 && !slices.Contains(cfg.Projects, s.Project) {
		return nil
	}
	if s.NextOrdinal <= 0 {
		return nil
	}

	// End of turn: the parser classified the session as parked
	// waiting for the user and the cursor has advanced past the
	// last reported end-of-turn. Fires exactly once per turn.
	if s.TerminationStatus == "awaiting_user" &&
		s.NextOrdinal > st.TurnEndOrdinal {
		return &Decision{
			Notification: d.render(KindTurnEnd, s),
			State: State{
				TurnEndOrdinal:     s.NextOrdinal,
				ReplyNotifyOrdinal: s.NextOrdinal,
				ReplyNotifiedAt:    st.ReplyNotifiedAt,
			},
		}
	}

	// Fallback "new reply" reminder: only when enabled, only for
	// fresh assistant output, and rate-limited by the merge
	// window so sustained streaming coalesces instead of spamming.
	// It deliberately does NOT advance the turn-end cursor: a
	// later awaiting_user for the same content still notifies.
	if cfg.NotifyNewReply &&
		s.LastRole == "assistant" &&
		s.NextOrdinal > st.ReplyNotifyOrdinal &&
		(st.ReplyNotifiedAt.IsZero() ||
			d.now().Sub(st.ReplyNotifiedAt) >= cfg.MergeWindow) {
		return &Decision{
			Notification: d.render(KindNewReply, s),
			State: State{
				TurnEndOrdinal:     st.TurnEndOrdinal,
				ReplyNotifyOrdinal: s.NextOrdinal,
				ReplyNotifiedAt:    d.now(),
			},
		}
	}
	return nil
}

func (d *Decider) render(kind Kind, s Snapshot) Notification {
	name := s.DisplayName
	if name == "" {
		name = s.Project
	}
	n := Notification{
		Kind:         kind,
		SessionID:    s.SessionID,
		Project:      s.Project,
		Agent:        s.Agent,
		DeepLinkPath: "/sessions/" + s.SessionID + "?msg=last",
		CreatedAt:    d.now().UTC().Format(time.RFC3339Nano),
	}
	if kind == KindTurnEnd {
		n.Title = name + " — reply finished"
		n.Body = "The agent finished this turn and is waiting for you."
	} else {
		n.Title = name + " — new reply"
		n.Body = excerpt(s.LastMessage)
	}
	return n
}

const bodyMaxRunes = 160

func excerpt(s string) string {
	runes := []rune(s)
	if len(runes) > bodyMaxRunes {
		return string(runes[:bodyMaxRunes]) + "…"
	}
	return s
}
