// CodeBuddy fork fixture shared by the replica usage parity tests. It keeps
// the same billing identities and token counts as the SQLite sync test, so
// every backend can be checked against one hand-computed daily total.
package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const (
	CodeBuddyParentID = "codebuddy:fork-parent"
	CodeBuddyChildID  = "codebuddy:fork-child"

	CodeBuddyDay       = "2026-01-01"
	CodeBuddyFromTime  = "2026-01-01T00:00:00Z"
	CodeBuddySharedAt  = "2026-01-01T00:00:01Z"
	CodeBuddyParentAt  = "2026-01-01T00:00:03Z"
	CodeBuddyChildAt   = "2026-01-01T00:00:05Z"
	CodeBuddyModelName = "synthetic-glm"
	CodeBuddyProject   = "synthetic-project"
	CodeBuddyMachine   = "synthetic-machine"
	CodeBuddyAgentName = "codebuddy"

	// The inherited reply counts once (100/10). The parent's own reply and
	// the child's genuine retry count separately (200/20 and 300/30).
	CodeBuddyWantInputTokens  = 600
	CodeBuddyWantOutputTokens = 60
)

// Fork identity names come from the CBC parser's billing identity format.
// Responses in different fork files share cb:r1:m1:1; a retry of the same
// text with a new request ID receives its own cb: identity.
const (
	CodeBuddyInheritedID = "cb:r1:m1:1"
	CodeBuddyParentIDKey = "cb:r2:m2:1"
	CodeBuddyRetryID     = "cb:r3:m3:1"
)

// SeedCodeBuddyForkedArchive opens a temporary SQLite archive and seeds two forked
// CodeBuddy sessions. The inherited response is repeated in both sessions;
// the child's later response has the same text but a different request ID.
func SeedCodeBuddyForkedArchive(t *testing.T) *db.DB {
	t.Helper()
	local := OpenTestDB(t)
	ctx := t.Context()

	parent := codeBuddySession(CodeBuddyParentID, CodeBuddyFromTime, CodeBuddyParentAt, 4, 2)
	child := codeBuddySession(CodeBuddyChildID, CodeBuddyFromTime, CodeBuddyChildAt, 6, 3)
	for _, s := range []db.Session{parent, child} {
		require.NoError(t, local.UpsertSession(ctx, s))
	}
	require.NoError(t, local.InsertMessages(ctx, []db.Message{
		codeBuddyMessage(CodeBuddyParentID, 0, "user", "start", CodeBuddyFromTime),
		codeBuddyUsageMessage(CodeBuddyParentID, 1, "shared reply", CodeBuddySharedAt, CodeBuddyInheritedID, 100, 10),
		codeBuddyMessage(CodeBuddyParentID, 2, "user", "parent only", "2026-01-01T00:00:02Z"),
		codeBuddyUsageMessage(CodeBuddyParentID, 3, "parent reply", CodeBuddyParentAt, CodeBuddyParentIDKey, 200, 20),
		codeBuddyMessage(CodeBuddyChildID, 0, "user", "start", CodeBuddyFromTime),
		codeBuddyMessage(CodeBuddyChildID, 1, "user", "child only", "2026-01-01T00:00:04Z"),
		codeBuddyMessage(CodeBuddyChildID, 2, "assistant", "shared reply", CodeBuddySharedAt),
		codeBuddyUsageMessage(CodeBuddyChildID, 3, "shared reply", CodeBuddySharedAt, CodeBuddyInheritedID, 100, 10),
		codeBuddyMessage(CodeBuddyChildID, 4, "user", "child only", "2026-01-01T00:00:04Z"),
		codeBuddyUsageMessage(CodeBuddyChildID, 5, "parent reply", CodeBuddyChildAt, CodeBuddyRetryID, 300, 30),
	}))
	return local
}

func codeBuddySession(id, started, ended string, messages, userMessages int) db.Session {
	return db.Session{
		ID: id, Project: CodeBuddyProject, Machine: CodeBuddyMachine, Agent: CodeBuddyAgentName,
		StartedAt: &started, EndedAt: &ended,
		MessageCount: messages, UserMessageCount: userMessages,
	}
}

func codeBuddyMessage(sessionID string, ordinal int, role, content, timestamp string) db.Message {
	return db.Message{
		SessionID: sessionID, Ordinal: ordinal, Role: role,
		Content: content, ContentLength: len(content), Timestamp: timestamp,
	}
}

func codeBuddyUsageMessage(
	sessionID string, ordinal int, content, timestamp, sourceUUID string,
	input, output int,
) db.Message {
	m := codeBuddyMessage(sessionID, ordinal, "assistant", content, timestamp)
	m.Model = CodeBuddyModelName
	m.SourceUUID = sourceUUID
	m.TokenUsage = []byte(fmt.Sprintf(
		`{"input_tokens":%d,"output_tokens":%d}`, input, output,
	))
	m.ContextTokens = input
	m.HasContextTokens = true
	m.OutputTokens = output
	m.HasOutputTokens = true
	return m
}

// AssertCodeBuddyDailyTotals checks the daily rollup against the hand-computed fork
// total. A replica that counts the inherited response twice fails here.
func AssertCodeBuddyDailyTotals(t *testing.T, result db.DailyUsageResult, backend string) {
	t.Helper()
	require.Len(t, result.Daily, 1, "%s: one UTC day", backend)
	assert.Equal(t, CodeBuddyDay, result.Daily[0].Date, "%s: daily date", backend)
	assert.Equal(t, CodeBuddyWantInputTokens, result.Daily[0].InputTokens,
		"%s: shared input counted once plus two distinct replies", backend)
	assert.Equal(t, CodeBuddyWantOutputTokens, result.Daily[0].OutputTokens,
		"%s: shared output counted once plus two distinct replies", backend)
	assert.Equal(t, CodeBuddyWantInputTokens, result.Totals.InputTokens, "%s: input totals", backend)
	assert.Equal(t, CodeBuddyWantOutputTokens, result.Totals.OutputTokens, "%s: output totals", backend)
}
