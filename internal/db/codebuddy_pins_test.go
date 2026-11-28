package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A CodeBuddy 4.x reply has no source identity, and IDE 5.x renders the
// converted reply with its reasoning and tool calls. A pin on the 4.x reply
// follows the converted reply at the same time, on a replace and on a
// rebuild. A time two replies share, or another agent's session, keeps the
// content match.
func TestCodeBuddyConvertedReplyKeepsPin(t *testing.T) {
	const (
		promptAt = "2026-01-01T00:00:01Z"
		replyAt  = "2026-01-01T00:00:02Z"
		laterAt  = "2026-01-01T00:01:40Z"
	)
	converted := "[Thinking]\ncheck the files\n[/Thinking]\n\nold reply\n\n[Bash]\n$ ls"
	for _, tc := range []struct {
		name        string
		agent       string
		old         []Message
		incoming    []Message
		wantOrdinal int
		wantUUID    string
		wantPin     bool
	}{
		{
			name:  "reply with source uuid",
			agent: "codebuddy",
			incoming: []Message{
				{Ordinal: 0, Role: "user", Content: "old prompt", Timestamp: promptAt, SourceUUID: "u1"},
				{Ordinal: 1, Role: "assistant", Content: converted, Timestamp: replyAt, SourceUUID: "cb4:req1:a1"},
				{Ordinal: 2, Role: "user", Content: "continued", Timestamp: laterAt, SourceUUID: "u2"},
			},
			wantOrdinal: 1, wantUUID: "cb4:req1:a1", wantPin: true,
		},
		{
			name:  "reply without source uuid at a new ordinal",
			agent: "codebuddy",
			incoming: []Message{
				{Ordinal: 0, Role: "user", Content: "old prompt", Timestamp: promptAt, SourceUUID: "u1"},
				{Ordinal: 1, Role: "assistant", IsSystem: true, Timestamp: promptAt, SourceUUID: "cb4:req0:a0"},
				{Ordinal: 2, Role: "assistant", Content: converted, Timestamp: replyAt},
				{Ordinal: 3, Role: "user", Content: "continued", Timestamp: laterAt, SourceUUID: "u2"},
			},
			wantOrdinal: 2, wantPin: true,
		},
		{
			name:  "two replies at the same time",
			agent: "codebuddy",
			old: []Message{
				{Ordinal: 0, Role: "user", Content: "old prompt", Timestamp: promptAt},
				{Ordinal: 1, Role: "assistant", Content: "old reply", Timestamp: replyAt},
				{Ordinal: 2, Role: "assistant", Content: "second reply", Timestamp: replyAt},
			},
			incoming: []Message{
				{Ordinal: 0, Role: "user", Content: "old prompt", Timestamp: promptAt, SourceUUID: "u1"},
				{Ordinal: 1, Role: "assistant", Content: converted, Timestamp: replyAt},
				{Ordinal: 2, Role: "assistant", Content: "second reply", Timestamp: replyAt},
			},
		},
		{
			name:  "another agent",
			agent: "claude",
			incoming: []Message{
				{Ordinal: 0, Role: "user", Content: "old prompt", Timestamp: promptAt, SourceUUID: "u1"},
				{Ordinal: 1, Role: "assistant", Content: converted, Timestamp: replyAt},
			},
		},
	} {
		for _, writer := range []string{"replace", "copy"} {
			t.Run(tc.name+"/"+writer, func(t *testing.T) {
				old := tc.old
				if old == nil {
					old = []Message{
						{Ordinal: 0, Role: "user", Content: "old prompt", Timestamp: promptAt},
						{Ordinal: 1, Role: "assistant", Content: "old reply", Timestamp: replyAt},
					}
				}
				withSession := func(msgs []Message) []Message {
					out := make([]Message, len(msgs))
					for i, m := range msgs {
						m.SessionID = "s1"
						m.ContentLength = len(m.Content)
						out[i] = m
					}
					return out
				}
				d := testDB(t)
				insertSession(t, d, "s1", "project", func(s *Session) { s.Agent = tc.agent })
				insertMessages(t, d, withSession(old)...)
				stored, err := d.GetAllMessages(t.Context(), "s1")
				require.NoError(t, err)
				require.Equal(t, "old reply", stored[1].Content)
				note := "keep this answer"
				_, err = d.PinMessage(t.Context(), "s1", stored[1].ID, &note)
				require.NoError(t, err)
				before, err := d.ListPinnedMessages(t.Context(), "s1", "")
				require.NoError(t, err)
				require.Len(t, before, 1)

				if writer == "copy" {
					fresh := testDB(t)
					insertSession(t, fresh, "s1", "project", func(s *Session) { s.Agent = tc.agent })
					insertMessages(t, fresh, withSession(tc.incoming)...)
					path := d.Path()
					require.NoError(t, d.Close())
					require.NoError(t, fresh.CopySessionMetadataFrom(t.Context(), path))
					d = fresh
				} else {
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s1", withSession(tc.incoming)))
				}

				pins, err := d.ListPinnedMessages(t.Context(), "s1", "")
				require.NoError(t, err)
				if !tc.wantPin {
					assert.Empty(t, pins)
					return
				}
				require.Len(t, pins, 1)
				assert.Equal(t, tc.wantOrdinal, pins[0].Ordinal)
				require.NotNil(t, pins[0].Note)
				assert.Equal(t, note, *pins[0].Note)
				assert.Equal(t, before[0].CreatedAt, pins[0].CreatedAt)
				msgs, err := d.GetAllMessages(t.Context(), "s1")
				require.NoError(t, err)
				var pinned *Message
				for i := range msgs {
					if msgs[i].ID == pins[0].MessageID {
						pinned = &msgs[i]
					}
				}
				require.NotNil(t, pinned, "the pin points at a stored message")
				assert.Equal(t, converted, pinned.Content)
				assert.Equal(t, tc.wantOrdinal, pinned.Ordinal)
				assert.Equal(t, tc.wantUUID, pinned.SourceUUID)
			})
		}
	}
}
