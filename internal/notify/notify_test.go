package notify

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	readyAt = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	after   = readyAt.Add(time.Minute)
)

func turnEndSnapshot(mods ...func(*Snapshot)) Snapshot {
	s := Snapshot{
		SessionID:         "s1",
		Project:           "proj",
		Agent:             "claude",
		DisplayName:       "Fix the login bug",
		TerminationStatus: "awaiting_user",
		NextOrdinal:       10,
		LastRole:          "assistant",
		LocalModifiedAt:   after,
		LastMessage:       "Done — tests pass.",
	}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func enabledCfg() Config {
	return Config{
		Enabled:           true,
		NotifyNewReply:    false,
		MergeWindow:       time.Minute,
		SuppressSubagents: true,
	}
}

func TestDeciderTurnEnd(t *testing.T) {
	now := after
	d := NewDecider(enabledCfg, func() time.Time { return now })

	t.Run("first awaiting_user notifies", func(t *testing.T) {
		got := d.Decide(turnEndSnapshot(), State{}, readyAt)
		require.NotNil(t, got)
		assert.Equal(t, KindTurnEnd, got.Notification.Kind)
		assert.Equal(t, int64(10), got.State.TurnEndOrdinal)
		assert.Equal(t, "/sessions/s1?msg=last", got.Notification.DeepLinkPath)
		assert.Contains(t, got.Notification.Title, "Fix the login bug")
	})

	t.Run("same ordinal again is deduplicated", func(t *testing.T) {
		st := State{TurnEndOrdinal: 10, ReplyNotifyOrdinal: 10}
		got := d.Decide(turnEndSnapshot(), st, readyAt)
		assert.Nil(t, got)
	})

	t.Run("next turn notifies again", func(t *testing.T) {
		st := State{TurnEndOrdinal: 10, ReplyNotifyOrdinal: 10}
		s := turnEndSnapshot(func(s *Snapshot) { s.NextOrdinal = 14 })
		got := d.Decide(s, st, readyAt)
		require.NotNil(t, got)
		assert.Equal(t, KindTurnEnd, got.Notification.Kind)
		assert.Equal(t, int64(14), got.State.TurnEndOrdinal)
	})
}

func TestDeciderNoTurnEndClaimsFromOtherStatuses(t *testing.T) {
	now := after
	d := NewDecider(enabledCfg, func() time.Time { return now })

	// Interruption (clean), pending tool call, and truncated files
	// never claim the turn finished.
	for _, status := range []string{"clean", "tool_call_pending", "truncated", ""} {
		s := turnEndSnapshot(func(s *Snapshot) { s.TerminationStatus = status })
		got := d.Decide(s, State{}, readyAt)
		if got != nil {
			assert.NotEqual(t, KindTurnEnd, got.Notification.Kind,
				"status %q must not produce turn_end", status)
		}
	}
}

func TestDeciderNewReplyMergeWindow(t *testing.T) {
	now := after
	cfg := enabledCfg()
	cfg.NotifyNewReply = true
	d := NewDecider(func() Config { return cfg }, func() time.Time { return now })

	s := turnEndSnapshot(func(s *Snapshot) {
		s.TerminationStatus = "tool_call_pending"
	})

	t.Run("fresh assistant output notifies once", func(t *testing.T) {
		got := d.Decide(s, State{}, readyAt)
		require.NotNil(t, got)
		assert.Equal(t, KindNewReply, got.Notification.Kind)
		assert.Equal(t, int64(10), got.State.ReplyNotifyOrdinal)
	})

	t.Run("repeated check inside merge window is merged away", func(t *testing.T) {
		st := State{
			ReplyNotifyOrdinal: 10,
			ReplyNotifiedAt:    now.Add(-30 * time.Second),
		}
		s2 := turnEndSnapshot(func(s *Snapshot) {
			s.TerminationStatus = "tool_call_pending"
			s.NextOrdinal = 12
		})
		assert.Nil(t, d.Decide(s2, st, readyAt))
	})

	t.Run("after the merge window a further reply notifies", func(t *testing.T) {
		st := State{
			ReplyNotifyOrdinal: 10,
			ReplyNotifiedAt:    now.Add(-2 * time.Minute),
		}
		s2 := turnEndSnapshot(func(s *Snapshot) {
			s.TerminationStatus = "tool_call_pending"
			s.NextOrdinal = 12
		})
		got := d.Decide(s2, st, readyAt)
		require.NotNil(t, got)
		assert.Equal(t, KindNewReply, got.Notification.Kind)
	})

	t.Run("new-reply does not consume the turn-end cursor", func(t *testing.T) {
		st := State{
			ReplyNotifyOrdinal: 10,
			ReplyNotifiedAt:    now,
		}
		// The same content later resolves to awaiting_user: the
		// end-of-turn must still fire even though a reply
		// reminder already covered ordinal 10.
		got := d.Decide(turnEndSnapshot(), st, readyAt)
		require.NotNil(t, got)
		assert.Equal(t, KindTurnEnd, got.Notification.Kind)
	})

	t.Run("disabled reminder stays silent", func(t *testing.T) {
		cfg.NotifyNewReply = false
		defer func() { cfg.NotifyNewReply = true }()
		got := d.Decide(s, State{}, readyAt)
		assert.Nil(t, got)
	})

	t.Run("tool execution with no output stays silent", func(t *testing.T) {
		s2 := turnEndSnapshot(func(s *Snapshot) {
			s.TerminationStatus = "tool_call_pending"
			s.LastRole = "user"
		})
		assert.Nil(t, d.Decide(s2, State{}, readyAt))
	})
}

func TestDeciderGates(t *testing.T) {
	now := after

	t.Run("disabled master switch", func(t *testing.T) {
		d := NewDecider(func() Config { return Config{Enabled: false} },
			func() time.Time { return now })
		assert.Nil(t, d.Decide(turnEndSnapshot(), State{}, readyAt))
	})

	t.Run("initial sync and resync churn stay silent", func(t *testing.T) {
		d := NewDecider(enabledCfg, func() time.Time { return now })
		s := turnEndSnapshot(func(s *Snapshot) {
			s.LocalModifiedAt = readyAt.Add(-time.Hour)
		})
		assert.Nil(t, d.Decide(s, State{}, readyAt))
	})

	t.Run("subagent sessions suppressed by default", func(t *testing.T) {
		d := NewDecider(enabledCfg, func() time.Time { return now })
		s := turnEndSnapshot(func(s *Snapshot) { s.RelationshipType = "subagent" })
		assert.Nil(t, d.Decide(s, State{}, readyAt))
	})

	t.Run("subagent notifications allowed when configured", func(t *testing.T) {
		cfg := enabledCfg()
		cfg.SuppressSubagents = false
		d := NewDecider(func() Config { return cfg }, func() time.Time { return now })
		s := turnEndSnapshot(func(s *Snapshot) { s.RelationshipType = "subagent" })
		got := d.Decide(s, State{}, readyAt)
		require.NotNil(t, got)
	})

	t.Run("automated sessions suppressed", func(t *testing.T) {
		d := NewDecider(enabledCfg, func() time.Time { return now })
		s := turnEndSnapshot(func(s *Snapshot) { s.IsAutomated = true })
		assert.Nil(t, d.Decide(s, State{}, readyAt))
	})

	t.Run("agent filter", func(t *testing.T) {
		cfg := enabledCfg()
		cfg.Agents = []string{"codex"}
		d := NewDecider(func() Config { return cfg }, func() time.Time { return now })
		assert.Nil(t, d.Decide(turnEndSnapshot(), State{}, readyAt))
		got := d.Decide(turnEndSnapshot(func(s *Snapshot) {
			s.Agent = "codex"
		}), State{}, readyAt)
		require.NotNil(t, got)
	})

	t.Run("project filter", func(t *testing.T) {
		cfg := enabledCfg()
		cfg.Projects = []string{"other"}
		d := NewDecider(func() Config { return cfg }, func() time.Time { return now })
		assert.Nil(t, d.Decide(turnEndSnapshot(), State{}, readyAt))
	})
}

func TestExcerptTruncatesLongBodies(t *testing.T) {
	long := make([]rune, 300)
	for i := range long {
		long[i] = '字'
	}
	got := excerpt(string(long))
	require.Len(t, []rune(got), bodyMaxRunes+1)
	assert.True(t, []rune(got)[bodyMaxRunes] == '…')
}
