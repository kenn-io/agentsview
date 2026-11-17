//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestRawProjectionPreservedHistoryFindsPromptLinks(t *testing.T) {
	f := newProjectionFixture(t)
	outcome := projectionOutcome("https://github.com/o/r/pull/2")
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentRooCode
	structured, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	outcome.Outcome.Results[0].Result.Session.PRLinks = []parser.PRLink{structured}
	a, receipt := f.accept(t, "device-a", "initial", "", parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
	first, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var payload []byte
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT payload FROM raw_content_revisions WHERE session_id=$1`, first.SessionID).Scan(&payload))
	legacy, err := decodeRawPayload(payload)
	require.NoError(t, err)
	legacy.Session.PRLinks = []db.PRLink{structured}
	legacy.StructuredPRLinks = nil
	payload, err = encodeRawPayload(legacy)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_content_revisions SET payload=$2 WHERE session_id=$1`, first.SessionID, payload)
	require.NoError(t, err)
	metadata, err := encodeRawPayload(ingest.PreparedSession{Session: legacy.Session})
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_session_branches SET prior_payload=$2 WHERE captured_session_id=$1`, first.SessionID, metadata)
	require.NoError(t, err)
	outcome.Outcome.Results[0].Result.Messages = nil
	a, _ = f.accept(t, "device-a", "replay", receipt.Receipt, parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{structured, {URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
}

func TestRawProjectionPromptLinkInAppendedDeviceCapture(t *testing.T) {
	f := newProjectionFixture(t)
	short := projectionOutcome("start")
	a, _ := f.accept(t, "device-a", "short", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
	long := projectionOutcome("start")
	long.Outcome.Results[0].Result.Messages = append(long.Outcome.Results[0].Result.Messages,
		parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "https://github.com/o/r/pull/2"})
	b, _ := f.accept(t, "device-b", "long", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
}
