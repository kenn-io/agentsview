package ingest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

// sharedTitleAgents are the providers whose title lives in a store that can be
// absent for a given session: Codex's session index, Antigravity's
// conversation_summaries.db, and Qoder's chat_sessions table.
var sharedTitleAgents = []parser.AgentType{
	parser.AgentCodex,
	parser.AgentAntigravity,
	parser.AgentQoder,
}

// TestConvertSessionPreservesSharedTitleWhenNoSignal pins the write-intent
// rule for a shared-title provider: when the parser reports no title signal at
// all, the stored session_name must survive a reparse rather than be cleared.
// Clearing it would lose a title that a later index or database write is
// expected to supply.
func TestConvertSessionPreservesSharedTitleWhenNoSignal(t *testing.T) {
	for _, agent := range sharedTitleAgents {
		t.Run(string(agent), func(t *testing.T) {
			session, err := ingest.ConvertSessionContext(
				t.Context(),
				parser.ParsedSession{ID: string(agent) + ":abc", Agent: agent},
				nil,
			)
			require.NoError(t, err)
			assert.True(t, session.PreserveSessionName,
				"a missing title signal must not clear the stored name")
		})
	}
}

// TestConvertSessionExplicitBlankTitleClears pins the other half of the
// contract: a title the provider reports as present is authoritative even when
// it is empty, so the stored name may be cleared.
func TestConvertSessionExplicitBlankTitleClears(t *testing.T) {
	for _, agent := range sharedTitleAgents {
		t.Run(string(agent), func(t *testing.T) {
			session, err := ingest.ConvertSessionContext(
				t.Context(),
				parser.ParsedSession{
					ID:                 string(agent) + ":abc",
					Agent:              agent,
					SessionNamePresent: true,
				},
				nil,
			)
			require.NoError(t, err)
			assert.False(t, session.PreserveSessionName,
				"an explicitly present title is authoritative, blank included")
		})
	}
}

// TestConvertSessionPreservesSharedTitleOnlyForSharedSources keeps the rule
// scoped to the providers that need it: a transcript-carried title has no
// shared store to fall back on, so an absent title means absent.
func TestConvertSessionPreservesSharedTitleOnlyForSharedSources(t *testing.T) {
	for _, agent := range []parser.AgentType{
		parser.AgentClaude, parser.AgentCursor,
	} {
		t.Run(string(agent), func(t *testing.T) {
			session, err := ingest.ConvertSessionContext(
				t.Context(),
				parser.ParsedSession{ID: string(agent) + ":abc", Agent: agent},
				nil,
			)
			require.NoError(t, err)
			assert.False(t, session.PreserveSessionName,
				"only shared-title providers preserve the stored name")
		})
	}
}
