package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestSnapshotFingerprintTracksPlanUsage pins both halves of the plan
// usage fingerprint contract: a session without plan usage keeps the
// fingerprint it had before the table existed, so an upgrade re-pushes
// nothing, and a plan usage change makes the session a push candidate.
func TestSnapshotFingerprintTracksPlanUsage(t *testing.T) {
	s := &Sync{machine: "m"}
	snapshot := &db.SessionMirrorSnapshot{Session: db.Session{
		ID: "plan", Project: "alpha", Machine: "m1", Agent: "codex-cloud",
		CreatedAt: "2026-10-01T00:00:00Z",
	}}
	fingerprint := func() string {
		t.Helper()
		fp, err := s.snapshotFingerprint(snapshot, nil)
		require.NoError(t, err)
		return fp.full
	}

	// Fingerprint this session had before session_plan_usage existed; sessions
	// without plan usage must keep it.
	const withoutPlanUsage = "3ffa01ac4b265fffea3c52f5731cafd35b542c2db053e8d5f993bac323c0d07b"
	assert.Equal(t, withoutPlanUsage, fingerprint())

	snapshot.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(1.0)}
	first := fingerprint()
	assert.NotEqual(t, withoutPlanUsage, first)
	snapshot.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(2.0)}
	assert.NotEqual(t, first, fingerprint())
}

func TestSnapshotFingerprintTracksDialogue(t *testing.T) {
	s := &Sync{machine: "m"}
	snapshot := &db.SessionMirrorSnapshot{
		Session:  db.Session{ID: "dialogue"},
		Messages: []db.Message{{Content: "rendered", Ordinal: 0}, {Content: "legacy", Ordinal: 1}},
	}
	fingerprint := func() string {
		t.Helper()
		fp, err := s.snapshotFingerprint(snapshot, nil)
		require.NoError(t, err)
		return fp.full
	}
	legacy := fingerprint()
	assert.Nil(t, db.DialogueFingerprintValues(snapshot.Messages), "all-NULL dialogue is omitted")
	snapshot.Messages[0].DialogueText = new("answerneedle")
	first := fingerprint()
	assert.NotEqual(t, legacy, first)
	snapshot.Messages[0].DialogueText = new("updatedneedle")
	assert.NotEqual(t, first, fingerprint())
	snapshot.Messages[0].DialogueText = new("")
	assert.NotEqual(t, legacy, fingerprint(), "empty dialogue differs from a legacy NULL")
	snapshot.Messages[0].DialogueText = nil
	snapshot.Messages[1].DialogueText = new("answerneedle")
	assert.NotEqual(t, first, fingerprint(), "dialogue remains associated with its message")
	snapshot.Messages[1].DialogueText = nil
	assert.Equal(t, legacy, fingerprint())
}

// TestSnapshotFingerprintSplit pins the session fingerprint stored before
// the content fingerprint existed, so an upgrade rewrites nothing, and
// checks that the content fingerprint ignores session fields and follows
// every dependent table.
func TestSnapshotFingerprintSplit(t *testing.T) {
	s := &Sync{machine: "m"}
	snapshot := &db.SessionMirrorSnapshot{
		Session: db.Session{
			ID: "golden", Project: "alpha", Agent: "claude", DataVersion: 7,
			CreatedAt: "2026-10-01T00:00:00Z",
		},
		Messages: []db.Message{
			{Ordinal: 0, Role: "user", Content: "hi", DialogueText: new("hi")},
			{
				Ordinal: 1, Role: "assistant", Content: "ok",
				ToolCalls: []db.ToolCall{{ToolName: "Bash", ToolUseID: "t1"}},
			},
		},
		UsageFingerprint:    "usage-fp",
		ToolCallFingerprint: "tool-fp",
		Findings:            []db.SecretFinding{{SessionID: "golden", RuleName: "r"}},
		Pins:                []db.PinnedMessage{{ID: 1, SessionID: "golden"}},
		CommitLinks:         []db.CommitLinkRow{{SessionID: "golden", SHA: "abc"}},
		ImageText:           []db.ImageTextRow{{SessionID: "golden", Text: "img"}},
		PlanUsage:           &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(1.0)},
	}
	fingerprint := func() sessionFingerprint {
		t.Helper()
		fp, err := s.snapshotFingerprint(snapshot, &db.SessionUsageTotal{})
		require.NoError(t, err)
		return fp
	}

	first := fingerprint()
	const beforeSplit = "fd493dcbb439d59f17e007806eccee99fd1d64b9eff40737f5fd09eb753498f6"
	assert.Equal(t, beforeSplit, first.full)

	snapshot.Session.DataVersion = 8
	snapshot.Session.DisplayName = new("renamed")
	renamed := fingerprint()
	assert.NotEqual(t, first.full, renamed.full)
	assert.Equal(t, first.content, renamed.content, "session fields leave the content fingerprint alone")

	for name, change := range map[string]func(){
		"message":    func() { snapshot.Messages[1].Content = "changed" },
		"dialogue":   func() { snapshot.Messages[0].DialogueText = new("changed") },
		"usage":      func() { snapshot.UsageFingerprint = "changed" },
		"tool calls": func() { snapshot.ToolCallFingerprint = "changed" },
		"finding":    func() { snapshot.Findings[0].RuleName = "changed" },
		"pin":        func() { snapshot.Pins[0].ID = 2 },
		"commit":     func() { snapshot.CommitLinks[0].SHA = "changed" },
		"image text": func() { snapshot.ImageText[0].Text = "changed" },
		"plan usage": func() { snapshot.PlanUsage.WeeklyPercent = new(2.0) },
	} {
		before := fingerprint().content
		change()
		assert.NotEqual(t, before, fingerprint().content, name)
	}
}

func TestPlanSessionWrite(t *testing.T) {
	local := sessionFingerprint{full: "full-new", content: "content-a"}
	stored := mirrorSession{
		fingerprint: "full-new", contentFingerprint: "content-a",
		archiveID: "archive", contentVersion: 7, factsCurrent: true,
	}
	rowOnly := sessionWrite{row: true, keptContent: "content-a", keptVersion: 7}
	for _, tc := range []struct {
		name   string
		local  sessionFingerprint
		stored func(*mirrorSession)
		found  bool
		full   bool
		want   sessionWrite
	}{
		{name: "unchanged", found: true, want: sessionWrite{}},
		{name: "unchanged on a full push", found: true, full: true, want: sessionWrite{}},
		{name: "missing from the mirror", want: rewriteSession},
		{
			name: "content changed", found: true,
			local: sessionFingerprint{full: "full-other", content: "content-b"},
			want:  rewriteSession,
		},
		{
			name: "session fields changed", found: true,
			local: sessionFingerprint{full: "full-other", content: "content-a"},
			want:  rowOnly,
		},
		{
			name: "row written before content fingerprints", found: true,
			local:  sessionFingerprint{full: "full-other", content: "content-a"},
			stored: func(m *mirrorSession) { m.contentFingerprint = "" },
			want:   rewriteSession,
		},
		{
			name: "stale dashboard facts", found: true,
			stored: func(m *mirrorSession) { m.factsCurrent = false },
			want:   rowOnly,
		},
		{
			name: "another archive on an incremental push", found: true,
			stored: func(m *mirrorSession) { m.archiveID = "other" },
			want:   sessionWrite{},
		},
		{
			name: "another archive on a full push", found: true, full: true,
			stored: func(m *mirrorSession) { m.archiveID = "other" },
			want:   rowOnly,
		},
		{
			// A row from before the split matches on the full fingerprint and
			// records no content fingerprint; the kept rows still match.
			name: "stale facts on a row written before content fingerprints", found: true,
			stored: func(m *mirrorSession) { m.contentFingerprint, m.factsCurrent = "", false },
			want:   rowOnly,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := stored
			if tc.stored != nil {
				tc.stored(&m)
			}
			fp := local
			if tc.local != (sessionFingerprint{}) {
				fp = tc.local
			}
			assert.Equal(t, tc.want, planSessionWrite(fp, m, tc.found, "archive", tc.full))
		})
	}
}
