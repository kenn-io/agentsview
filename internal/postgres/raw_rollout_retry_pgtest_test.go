//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawRolloutRetriesCommitExactlyOnce(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		for _, operation := range []string{"projection", "curation"} {
			t.Run(code+"/"+operation, func(t *testing.T) { testRawRolloutRetry(t, code, operation) })
		}
	}
}

func testRawRolloutRetry(t *testing.T, code, operation string) {
	t.Helper()
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "retry", "")
	lease := f.lease(t, m)
	table := "raw_content_revisions"
	if operation != "projection" {
		require.NoError(t, f.sink.Project(t.Context(), lease, m, projectionOutcome("retained")))
		table = "raw_curation"
	}
	_, err := f.admin.Exec(`CREATE SEQUENCE rollout_attempt;
 CREATE FUNCTION rollout_retry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF nextval('rollout_attempt')=1 THEN RAISE EXCEPTION 'synthetic transaction conflict' USING ERRCODE=TG_ARGV[0]; END IF;
 RETURN NEW; END $$; GRANT USAGE ON SEQUENCE rollout_attempt TO "` + f.role + `";
 CREATE TRIGGER rollout_retry BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION rollout_retry('` + code + `')`)
	require.NoError(t, err)
	before := f.revisions(t)
	switch operation {
	case "projection":
		err = f.sink.Project(t.Context(), lease, m, projectionOutcome("retained"))
	case "curation":
		err = f.sink.SetCuration(t.Context(), "codex:portable", "starred", true)
	}
	require.NoError(t, err)
	var attempts int
	require.NoError(t, f.admin.QueryRow(`SELECT last_value FROM rollout_attempt`).Scan(&attempts))
	require.Equal(t, 2, attempts, "the conflict must be reached, then the full transaction retried")
	after := f.revisions(t)
	require.Equal(t, before.corpus+1, after.corpus)
	require.Equal(t, before.selection, after.selection)
	identityDelta := int64(1)
	if operation == "curation" {
		identityDelta = 0
	}
	require.Equal(t, before.identity+identityDelta, after.identity)
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, resolved.State)
	messages, err := (&Store{pg: f.runtime}).GetMessages(t.Context(), resolved.SessionID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "retained", messages[0].Content)
	require.Equal(t, "checking", messages[1].Content)
	require.Len(t, messages[1].ToolCalls, 1)
	require.Equal(t, "Bash", messages[1].ToolCalls[0].ToolName)
	var revisions int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_content_revisions`).Scan(&revisions))
	require.Equal(t, 1, revisions, "a failed attempt must not leave a content revision")
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&state))
	require.Equal(t, "complete", state)
	var rows int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE provenance_kind='raw'`).Scan(&rows))
	require.Equal(t, 1, rows)
	if operation == "curation" {
		var starred bool
		require.NoError(t, f.runtime.QueryRow(`SELECT EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=$1) AND EXISTS(SELECT 1 FROM raw_curation WHERE field='starred' AND value='true'::jsonb)`, resolved.SessionID).Scan(&starred))
		require.True(t, starred)
	}
}

func TestRawRolloutFirstSerializationFailureRegression(t *testing.T) {
	testRawRolloutRetry(t, "40001", "projection")
}
