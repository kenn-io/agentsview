package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestCommitLinksBothBackends(t *testing.T) {
	env := newHTTPBackendEnv(t)
	dbtest.SeedSession(t, env.DB, "claude:commit", "project-a")
	dbtest.SeedMessages(t, env.DB,
		db.Message{SessionID: "claude:commit", Ordinal: 0, Role: "user", Content: "Fix parser"},
		db.Message{SessionID: "claude:commit", Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{{SessionID: "claude:commit", Category: "Bash", ToolName: "Bash", ToolUseID: "commit-call", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abc1234] Fix parser", ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234] Fix parser"}}}}},
	)
	for name, svc := range map[string]service.SessionService{"direct": service.NewDirectBackend(env.DB, nil), "http": env.Backend("", false)} {
		t.Run(name, func(t *testing.T) {
			session, err := svc.SessionCommits(t.Context(), "claude:commit")
			require.NoError(t, err)
			require.Len(t, session.Commits, 1)
			assert.Equal(t, "abc1234", session.Commits[0].SHA)
			lookup, err := svc.CommitSessions(t.Context(), []string{"abc1234"})
			require.NoError(t, err)
			require.Len(t, lookup.Commits, 1)
			require.Len(t, lookup.Commits[0].Links, 1)
		})
	}
}
