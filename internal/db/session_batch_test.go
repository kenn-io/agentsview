package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func messageCountWrite(id string, count int) SessionBatchWrite {
	messages := make([]Message, count)
	for i := range messages {
		content := fmt.Sprintf("message-%03d", i)
		messages[i] = Message{
			SessionID:     id,
			Ordinal:       i,
			Role:          "user",
			Content:       content,
			ContentLength: len(content),
			Timestamp:     time.Unix(int64(i), 0).UTC().Format(time.RFC3339),
		}
	}
	return SessionBatchWrite{
		Session: Session{
			ID: id, Project: "project", Machine: defaultMachine,
			Agent: "claude", MessageCount: count, UserMessageCount: count,
		},
		Messages:        messages,
		DataVersion:     CurrentDataVersion(),
		ReplaceMessages: true,
	}
}

func requireSessionMessageCount(t *testing.T, d *DB, id string, want int) {
	t.Helper()
	messages, err := d.GetAllMessages(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, messages, want)
}

func TestWriteSessionBatchMessageCountCondition(t *testing.T) {
	tests := []struct {
		name     string
		incoming int
		guard    bool
		wantErr  bool
	}{
		{name: "shorter rejected", incoming: 24, guard: true, wantErr: true},
		{name: "equal allowed", incoming: 96, guard: true},
		{name: "longer allowed", incoming: 120, guard: true},
		{name: "zero value preserves replacement", incoming: 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{
				messageCountWrite("session", 96),
			})
			require.NoError(t, err)

			write := messageCountWrite("session", tt.incoming)
			write.RejectMessageCountDecrease = tt.guard
			_, err = d.WriteSessionBatchAtomic([]SessionBatchWrite{write})
			if !tt.wantErr {
				require.NoError(t, err)
				requireSessionMessageCount(t, d, "session", tt.incoming)
				return
			}

			var shorter *SessionWouldShortenError
			require.ErrorAs(t, err, &shorter)
			require.Equal(t, "session", shorter.SessionID)
			require.Equal(t, 96, shorter.ExistingMessages)
			require.Equal(t, 24, shorter.IncomingMessages)
			requireSessionMessageCount(t, d, "session", 96)
		})
	}
}

func TestWriteSessionBatchAtomicShorterMemberRollsBack(t *testing.T) {
	d := testDB(t)
	root := messageCountWrite("root", 96)
	child := messageCountWrite("child", 30)
	child.Session.ParentSessionID = Ptr("root")
	child.Session.RelationshipType = "subagent"
	_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{root, child})
	require.NoError(t, err)

	root = messageCountWrite("root", 120)
	child = messageCountWrite("child", 10)
	root.RejectMessageCountDecrease = true
	child.RejectMessageCountDecrease = true
	callbackCalled := false
	result, err := d.WriteSessionBatchAtomic(
		[]SessionBatchWrite{root, child},
		func() error {
			callbackCalled = true
			return nil
		},
	)
	var shorter *SessionWouldShortenError
	require.ErrorAs(t, err, &shorter)
	require.Equal(t, "child", shorter.SessionID)
	require.Zero(t, result.WrittenSessions)
	require.False(t, callbackCalled)
	requireSessionMessageCount(t, d, "root", 96)
	requireSessionMessageCount(t, d, "child", 30)
}

func TestWriteSessionBatchMessageCountDecisionIsSerialized(t *testing.T) {
	d := testDB(t)
	_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{
		messageCountWrite("session", 96),
	})
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	first := messageCountWrite("session", 120)
	first.RejectMessageCountDecrease = true
	go func() {
		_, err := d.WriteSessionBatchAtomic(
			[]SessionBatchWrite{first},
			func() error {
				close(entered)
				<-release
				return nil
			},
		)
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first writer did not reach beforeCommit")
	}

	secondDone := make(chan error, 1)
	second := messageCountWrite("session", 24)
	second.RejectMessageCountDecrease = true
	go func() {
		_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{second})
		secondDone <- err
	}()
	close(release)
	require.NoError(t, <-firstDone)

	var shorter *SessionWouldShortenError
	require.ErrorAs(t, <-secondDone, &shorter)
	require.Equal(t, 120, shorter.ExistingMessages)
	require.Equal(t, 24, shorter.IncomingMessages)
	requireSessionMessageCount(t, d, "session", 120)
}
