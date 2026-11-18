//go:build pgtest

package postgres

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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

func TestRawProjectionPreservedHistoryKeepsSharedCapture(t *testing.T) {
	f := newProjectionFixture(t)
	outcome := projectionOutcome("Review the change")
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentRooCode
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	outcome.Outcome.Results[0].Result.Session.PRLinks = []parser.PRLink{link}
	a, receipt := f.accept(t, "device-a", "initial", "", parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
	b, _ := f.accept(t, "device-b", "original", "", parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, outcome))
	// Restore the capture shape written before prompt links were derived.
	var oldID, group string
	var payload []byte
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT session_id,group_id,payload FROM raw_content_revisions`).Scan(&oldID, &group, &payload))
	legacy, err := decodeRawPayload(payload)
	require.NoError(t, err)
	if field := reflect.ValueOf(&legacy).Elem().FieldByName("StructuredPRLinks"); field.IsValid() {
		field.SetZero()
	}
	revision, err := rawContentRevision(legacy)
	require.NoError(t, err)
	legacyID := "raw-row-" + rawDigest("row-v1", group, revision)
	payload, err = encodeRawPayload(legacy)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	if legacyID != oldID {
		_, err = tx.ExecContext(t.Context(), `INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload) VALUES($1,$2,$3,$4)`, legacyID, group, revision, payload)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `UPDATE raw_session_branches SET session_id=$1,captured_session_id=$1,content_revision=$2 WHERE captured_session_id=$3`, legacyID, revision, oldID)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `UPDATE session_sources SET session_id=$1,physical_session_id=NULL,content_revision=$2 WHERE session_id=$3`, legacyID, revision, oldID)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, oldID)
		require.NoError(t, err)
		require.NoError(t, f.sink.writePayload(t.Context(), tx, legacyID, group, revision, legacy))
	}
	require.NoError(t, tx.Commit())
	outcome.Outcome.Results[0].Result.Messages = nil
	a, _ = f.accept(t, "device-a", "replay", receipt.Receipt, parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestRawProjectionRolloutBackfillsPromptLinks(t *testing.T) {
	f := newProjectionFixture(t)
	outcome := projectionOutcome("Review https://github.com/o/r/pull/2")
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	outcome.Outcome.Results[0].Result.Session.PRLinks = []parser.PRLink{link}
	m, _ := f.accept(t, "device-a", "initial", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	// A pre-upgrade session row contains the agent-recorded link only.
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET pr_links=$2 WHERE id=$1`, before.SessionID, db.EncodePRLinks([]db.PRLink{link}))
	require.NoError(t, err)
	rollout, err := f.sink.ScheduleCurrentHeads(t.Context(), "prompt-links", "parser-2", 10)
	require.NoError(t, err)
	require.Equal(t, 1, rollout.Selected)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "prompt-links-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], m, outcome))
	after, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, after.SessionID)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, after.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{link, {URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
}
