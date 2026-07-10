package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportAcceptedRecallEntriesJSONLImportsReviewedKeepers(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo/agentsview"
		s.GitBranch = "main"
	})
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","trigger":"file read failure","confidence":0.92,"uncertainty":"low","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s1","episode_id":"ep1","run_id":"run1","extractor_method":"single","model":"fake-model","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"],"snippets":["Verify cwd before retrying"]}}
{"candidate_id":"m-reject","type":"fact","scope":"project","title":"Rejected","body":"Rejected.","project":"agentsview","agent":"codex","session_id":"s1","label":"wrong","transferable":false,"provenance_ok":false,"evidence":{"ordinal_start":1,"ordinal_end":1}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Imported)
	assert.Equal(t, 1, result.Skipped)
	got, err := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Check cwd before file reads", got.Title)
	assert.Equal(t, "s1", got.SourceSessionID)
	assert.Equal(t, "ep1", got.SourceEpisodeID)
	assert.Equal(t, "run1", got.SourceRunID)
	assert.Equal(t, "single", got.ExtractorMethod)
	assert.True(t, got.Transferable)
	assert.Equal(t, "human_reviewed", got.ReviewState)
	assert.False(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Empty(t, got.Evidence[0].MessageStartSourceUUID)
	assert.Empty(t, got.Evidence[0].MessageEndSourceUUID)
	assert.Empty(t, got.Evidence[0].ContentDigest)
	assert.Equal(t, "toolu_1", got.Evidence[0].ToolUseID)
	assert.Equal(t, 3, got.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, 7, got.Evidence[0].MessageEndOrdinal)
}

func TestImportAcceptedRecallEntriesJSONLDeduplicatesEvidenceToolUses(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m-dedup","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":[" toolu_1 ","toolu_1","toolu_2","  "],"snippets":["Verify cwd","before retrying"]}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Imported)
	got, err := d.GetRecallEntry(context.Background(), "m-dedup")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Evidence, 2)
	assert.Equal(t, "toolu_1", got.Evidence[0].ToolUseID)
	assert.Equal(t, "Verify cwd\nbefore retrying", got.Evidence[0].Snippet)
	assert.Equal(t, "toolu_2", got.Evidence[1].ToolUseID)
	assert.Empty(t, got.Evidence[1].Snippet)
}

func TestImportAcceptedRecallEntriesJSONLRejectsMissingEvidence(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"No evidence","body":"Missing ordinal evidence.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing evidence")
}

func TestImportAcceptedRecallEntriesJSONLRejectsNegativeEvidenceOrdinal(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":-1,"ordinal_end":0}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid evidence ordinal range")
	got, getErr := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, getErr)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLRejectsInvalidType(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m1","type":"local_fix","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid recall type "local_fix"`)
	got, getErr := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, getErr)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLRejectsInvalidScope(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"workspace","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid recall scope "workspace"`)
	got, getErr := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, getErr)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLRejectsInvalidConfidence(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","confidence":1.2,"project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid confidence 1.2")
	got, getErr := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, getErr)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLRejectsControlCharactersInIdentityFields(
	t *testing.T,
) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "candidate id",
			input: `{"candidate_id":"m\u0001bad","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}`,
			want:  "candidate_id must not contain control characters",
		},
		{
			name:  "session id",
			input: `{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s\u0001bad","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}`,
			want:  "session_id must not contain control characters",
		},
		{
			name:  "run id",
			input: `{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","run_id":"run\u0001bad","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}`,
			want:  "run_id must not contain control characters",
		},
		{
			name:  "episode id",
			input: `{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","episode_id":"ep\u0001bad","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}`,
			want:  "episode_id must not contain control characters",
		},
		{
			name:  "supersedes recall id",
			input: `{"candidate_id":"m1","supersedes_entry_id":"old\u0001bad","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}`,
			want:  "supersedes_entry_id must not contain control characters",
		},
		{
			name:  "tool use id",
			input: `{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu\u0001bad"]}}`,
			want:  "tool_use_id must not contain control characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "agentsview")

			_, err := d.ImportAcceptedRecallEntriesJSONL(
				context.Background(), strings.NewReader(tc.input+"\n"),
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			got, getErr := d.GetRecallEntry(context.Background(), "m1")
			require.NoError(t, getErr)
			assert.Nil(t, got)
		})
	}
}

func TestImportAcceptedRecallEntriesJSONLCreatesPlaceholderSourceSession(t *testing.T) {
	d := testDB(t)
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s-missing","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Imported)
	got, err := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "s-missing", got.SourceSessionID)
	assert.Equal(t, "human_reviewed", got.ReviewState)
	assert.False(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Empty(t, got.Evidence[0].MessageStartSourceUUID)
	assert.Empty(t, got.Evidence[0].MessageEndSourceUUID)
	assert.Empty(t, got.Evidence[0].ContentDigest)
	session, err := d.GetSession(context.Background(), "s-missing")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "agentsview", session.Project)
	assert.Equal(t, "codex", session.Agent)
	assert.Equal(t, "/repo/agentsview", session.Cwd)
	assert.Equal(t, "main", session.GitBranch)
	assert.Equal(t, "recall-import-placeholder", session.SourceVersion)
}

func TestImportAcceptedRecallEntriesJSONLPlaceholderSessionStateParity(
	t *testing.T,
) {
	states := []struct {
		name string
		want error
		seed func(*testing.T, *DB)
	}{
		{
			name: "excluded",
			want: ErrSessionExcluded,
			seed: func(t *testing.T, d *DB) {
				_, err := d.getWriter().Exec(
					"INSERT INTO excluded_sessions (id) VALUES (?)",
					"s-blocked",
				)
				require.NoError(t, err)
			},
		},
		{
			name: "trashed",
			want: ErrSessionTrashed,
			seed: func(t *testing.T, d *DB) {
				insertSession(t, d, "s-blocked", "agentsview")
				_, err := d.getWriter().Exec(`
					UPDATE sessions
					SET deleted_at = '2026-07-09T00:00:00Z'
					WHERE id = 's-blocked'`)
				require.NoError(t, err)
			},
		},
	}
	modes := []struct {
		name   string
		dryRun bool
	}{
		{name: "real import"},
		{name: "dry run", dryRun: true},
	}

	for _, state := range states {
		for _, mode := range modes {
			t.Run(state.name+"/"+mode.name, func(t *testing.T) {
				d := testDB(t)
				state.seed(t, d)
				input := strings.NewReader(`
{"candidate_id":"m1","type":"fact","scope":"project","title":"Blocked placeholder","body":"Placeholder imports must respect session lifecycle state.","project":"agentsview","session_id":"s-blocked","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)

				result, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
					context.Background(), input,
					RecallImportOptions{
						DryRun:                  mode.dryRun,
						RequireExistingSessions: false,
					},
				)

				require.ErrorIs(t, err, state.want)
				assert.Zero(t, result.Imported)
				assert.Zero(t, result.WouldImport)
				got, getErr := d.GetRecallEntry(context.Background(), "m1")
				require.NoError(t, getErr)
				assert.Nil(t, got)
			})
		}
	}
}

func TestImportAcceptedRecallEntriesJSONLRejectsReviewStateInput(t *testing.T) {
	d := testDB(t)
	input := strings.NewReader(`
{"candidate_id":"m1","type":"fact","scope":"project","title":"Spoofed review","body":"The payload must not choose its trust state.","session_id":"s-missing","review_state":"human_reviewed","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "review_state")
	got, getErr := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, getErr)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLRejectsHostEvidenceMetadataInput(
	t *testing.T,
) {
	tests := []struct {
		name  string
		field string
	}{
		{name: "start source uuid", field: `"message_start_source_uuid":"spoofed"`},
		{name: "end source uuid", field: `"message_end_source_uuid":"spoofed"`},
		{name: "content digest", field: `"content_digest":"spoofed"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			input := strings.NewReader(`
{"candidate_id":"m1","type":"fact","scope":"project","title":"Spoofed evidence","body":"The payload must not choose host evidence metadata.","session_id":"s-missing","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0,` + tt.field + `}}
`)

			_, err := d.ImportAcceptedRecallEntriesJSONL(
				context.Background(), input,
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "host-controlled")
			got, getErr := d.GetRecallEntry(context.Background(), "m1")
			require.NoError(t, getErr)
			assert.Nil(t, got)
		})
	}
}

func TestImportAcceptedRecallEntriesJSONLRequireExistingSessionsRejectsMissingSession(t *testing.T) {
	d := testDB(t)
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s-missing","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(),
		input,
		RecallImportOptions{RequireExistingSessions: true},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "source session s-missing not found")
	got, err := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, err)
	assert.Nil(t, got)
	session, err := d.GetSession(context.Background(), "s-missing")
	require.NoError(t, err)
	assert.Nil(t, session)
}

func TestImportAcceptedRecallEntriesJSONLRequireExistingSessionsRejectsTrashedSession(
	t *testing.T,
) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	insertMessages(t, d, userMsg("s1", 0, "Evidence remains in the archive."))
	_, err := d.getWriter().Exec(`
		UPDATE sessions
		SET deleted_at = '2026-07-09T00:00:00Z'
		WHERE id = 's1'`)
	require.NoError(t, err)
	input := strings.NewReader(`
{"candidate_id":"m1","type":"fact","scope":"project","title":"Hidden-session evidence","body":"Trashed sessions cannot authorize new trusted recall.","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)

	_, err = d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(), input,
		RecallImportOptions{RequireExistingSessions: true},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "source session s1 not found")
	got, getErr := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, getErr)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLRequireExistingSessionsRejectsMissingEvidenceRange(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(),
		input,
		RecallImportOptions{RequireExistingSessions: true},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "source evidence s1:3-7 not found")
	got, err := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLRequireExistingSessionsValidatesEvidenceAndToolUse(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	messages := []Message{
		userMsg("s1", 3, "User saw cwd failure."),
		asstMsg("s1", 4, "I will inspect cwd."),
		userMsg("s1", 5, "Retry failed."),
		asstMsg("s1", 6, "[Read: main.go]"),
		userMsg("s1", 7, "That fixed it."),
	}
	messages[3].HasToolUse = true
	messages[3].ToolCalls = []ToolCall{
		{
			SessionID: "s1",
			ToolName:  "Read",
			Category:  "Read",
			ToolUseID: "toolu_1",
		},
	}
	for i := range messages {
		messages[i].SourceUUID = fmt.Sprintf("source-%d", messages[i].Ordinal)
	}
	insertMessages(t, d, messages...)
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"]}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(),
		input,
		RecallImportOptions{RequireExistingSessions: true},
	)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Imported)
	got, err := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "human_reviewed", got.ReviewState)
	assert.True(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, "toolu_1", got.Evidence[0].ToolUseID)
	assert.Equal(t, "source-3", got.Evidence[0].MessageStartSourceUUID)
	assert.Equal(t, "source-7", got.Evidence[0].MessageEndSourceUUID)
	assert.Len(t, got.Evidence[0].ContentDigest, 64)
}

func TestImportAcceptedRecallEntriesJSONLNeverCommitsStaleEvidenceSnapshot(
	t *testing.T,
) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	messages := []Message{
		userMsg("s1", 3, "The original evidence begins."),
		asstMsg("s1", 4, "The original evidence ends."),
	}
	messages[0].SourceUUID = "source-3"
	messages[1].SourceUUID = "source-4"
	insertMessages(t, d, messages...)

	external, err := sql.Open("sqlite3", makeDSN(d.Path(), false))
	require.NoError(t, err)
	defer external.Close()
	_, err = external.Exec(`PRAGMA busy_timeout = 5000`)
	require.NoError(t, err)
	rewrite, err := external.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer rewrite.Rollback()
	_, err = rewrite.Exec(`
		UPDATE messages
		SET content = 'The evidence changed before import commit.'
		WHERE session_id = 's1' AND ordinal = 4`)
	require.NoError(t, err)

	type importOutcome struct {
		result RecallImportResult
		err    error
	}
	outcomes := make(chan importOutcome, 1)
	go func() {
		result, importErr := d.ImportAcceptedRecallEntriesJSONLWithOptions(
			context.Background(),
			strings.NewReader(`
{"candidate_id":"m-race","type":"fact","scope":"project","title":"Snapshot-sensitive evidence","body":"Trusted import must bind and insert against one transcript snapshot.","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":4}}
`),
			RecallImportOptions{RequireExistingSessions: true},
		)
		outcomes <- importOutcome{result: result, err: importErr}
	}()

	var earlyOutcome *importOutcome
	require.Eventually(t, func() bool {
		select {
		case outcome := <-outcomes:
			earlyOutcome = &outcome
			return true
		default:
			return d.rawWriter().Stats().InUse > 0
		}
	}, 5*time.Second, 10*time.Millisecond,
		"import should reach or safely abort its writer transaction")
	require.NoError(t, rewrite.Commit())

	var outcome importOutcome
	if earlyOutcome != nil {
		outcome = *earlyOutcome
	} else {
		select {
		case outcome = <-outcomes:
		case <-time.After(10 * time.Second):
			require.FailNow(t, "import did not finish after concurrent rewrite")
		}
	}
	got, getErr := d.GetRecallEntry(context.Background(), "m-race")
	require.NoError(t, getErr)
	if outcome.err != nil {
		assert.Nil(t, got,
			"a snapshot-conflicted import must leave no partial entry")
		return
	}
	require.Equal(t, 1, outcome.result.Imported)
	require.NotNil(t, got)
	if !got.ProvenanceOK {
		return
	}
	require.Len(t, got.Evidence, 1)
	window, err := d.BuildRecallEvidenceWindow(
		context.Background(), "s1", 3, 4,
	)
	require.NoError(t, err)
	metadata, err := window.BindSelection(RecallEvidenceSelection{
		MessageStartOrdinal: 3,
		MessageEndOrdinal:   4,
	})
	require.NoError(t, err)
	assert.Equal(t, metadata.ContentDigest, got.Evidence[0].ContentDigest,
		"a successful trusted import must match the committed transcript")
}

func TestImportAcceptedRecallEntriesJSONLRequireExistingSessionsRejectsMissingToolUse(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	insertMessages(t, d,
		userMsg("s1", 3, "User saw cwd failure."),
		asstMsg("s1", 4, "I will inspect cwd."),
		userMsg("s1", 5, "Retry failed."),
		asstMsg("s1", 6, "[Read: main.go]"),
		userMsg("s1", 7, "That fixed it."),
	)
	input := strings.NewReader(`
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_missing"]}}
`)

	_, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(),
		input,
		RecallImportOptions{RequireExistingSessions: true},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "source tool use toolu_missing not found")
	got, err := d.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLSkipsDuplicateIDs(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	line := `{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}`
	first, err := d.ImportAcceptedRecallEntriesJSONL(
		context.Background(), strings.NewReader(line+"\n"),
	)
	require.NoError(t, err)
	require.Equal(t, 1, first.Imported)

	second, err := d.ImportAcceptedRecallEntriesJSONL(
		context.Background(), strings.NewReader(line+"\n"),
	)

	require.NoError(t, err)
	assert.Equal(t, 0, second.Imported)
	assert.Equal(t, 1, second.Skipped)
	require.Len(t, second.SkippedEntries, 1)
	assert.Equal(t, "m1", second.SkippedEntries[0].CandidateID)
	assert.Equal(t, "duplicate", second.SkippedEntries[0].Reason)
}

func TestImportAcceptedRecallEntriesJSONLDuplicateRemainsIdempotentAfterEvidenceRemoved(
	t *testing.T,
) {
	const line = `{"candidate_id":"m-idempotent","type":"fact","scope":"project","title":"Verified archive behavior","body":"An exact re-import remains idempotent after the transcript changes.","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":4}}`
	tests := []struct {
		name   string
		dryRun bool
	}{
		{name: "real import", dryRun: false},
		{name: "dry run", dryRun: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "agentsview")
			messages := []Message{
				userMsg("s1", 3, "The verified evidence begins."),
				asstMsg("s1", 4, "The verified evidence ends."),
			}
			messages[0].SourceUUID = "source-3"
			messages[1].SourceUUID = "source-4"
			insertMessages(t, d, messages...)

			initial, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
				context.Background(), strings.NewReader(line+"\n"),
				RecallImportOptions{RequireExistingSessions: true},
			)
			require.NoError(t, err)
			require.Equal(t, 1, initial.Imported)
			before, err := d.GetRecallEntry(context.Background(), "m-idempotent")
			require.NoError(t, err)
			require.NotNil(t, before)
			require.Len(t, before.Evidence, 1)
			beforeDigest := before.Evidence[0].ContentDigest
			require.NotEmpty(t, beforeDigest)

			_, err = d.getWriter().Exec(`
				DELETE FROM messages
				WHERE session_id = 's1' AND ordinal = 4`)
			require.NoError(t, err)

			duplicate, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
				context.Background(), strings.NewReader(line+"\n"),
				RecallImportOptions{
					DryRun:                  tt.dryRun,
					RequireExistingSessions: true,
				},
			)

			require.NoError(t, err)
			assert.Zero(t, duplicate.Imported)
			assert.Zero(t, duplicate.WouldImport)
			assert.Equal(t, 1, duplicate.Skipped)
			require.Len(t, duplicate.SkippedEntries, 1)
			assert.Equal(t, "m-idempotent", duplicate.SkippedEntries[0].CandidateID)
			assert.Equal(t, "duplicate", duplicate.SkippedEntries[0].Reason)

			after, err := d.GetRecallEntry(context.Background(), "m-idempotent")
			require.NoError(t, err)
			require.NotNil(t, after)
			require.Len(t, after.Evidence, 1)
			assert.Equal(t, before, after)
			assert.Equal(t, beforeDigest, after.Evidence[0].ContentDigest)

			newCandidate := strings.Replace(
				line, `"m-idempotent"`, `"m-new"`, 1,
			)
			_, err = d.ImportAcceptedRecallEntriesJSONLWithOptions(
				context.Background(), strings.NewReader(newCandidate+"\n"),
				RecallImportOptions{
					DryRun:                  tt.dryRun,
					RequireExistingSessions: true,
				},
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "source evidence s1:3-4 not found")
			got, getErr := d.GetRecallEntry(context.Background(), "m-new")
			require.NoError(t, getErr)
			assert.Nil(t, got)
		})
	}
}

func TestImportAcceptedRecallEntriesJSONLSupersedesExistingRecallEntry(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	insertSession(t, d, "s2", "agentsview", func(s *Session) {
		s.Agent = "codex"
	})
	_, err := d.InsertRecallEntry(RecallEntry{
		ID:              "old",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Old retry policy",
		Body:            "Retry flaky command once before escalating.",
		Project:         "agentsview",
		Agent:           "codex",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	input := strings.NewReader(`
{"candidate_id":"new","supersedes_entry_id":"old","type":"fact","scope":"project","title":"Current retry policy","body":"Retry flaky command three times before escalating.","project":"agentsview","agent":"codex","session_id":"s2","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":4,"ordinal_end":5}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONL(context.Background(), input)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Imported)
	var raw struct {
		ImportedEntries []struct {
			SupersedesEntryID string `json:"supersedes_entry_id"`
		} `json:"imported_entries"`
	}
	data, err := json.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Len(t, raw.ImportedEntries, 1)
	assert.Equal(t, "old", raw.ImportedEntries[0].SupersedesEntryID)
	oldRecallEntry, err := d.GetRecallEntry(context.Background(), "old")
	require.NoError(t, err)
	require.NotNil(t, oldRecallEntry)
	assert.Equal(t, "archived", oldRecallEntry.Status)
	assert.Equal(t, "new", oldRecallEntry.SupersededByEntryID)
	newRecallEntry, err := d.GetRecallEntry(context.Background(), "new")
	require.NoError(t, err)
	require.NotNil(t, newRecallEntry)
	assert.Equal(t, "accepted", newRecallEntry.Status)
	assert.Equal(t, "old", newRecallEntry.SupersedesEntryID)
}

func TestImportAcceptedRecallEntriesJSONLRejectsPlaceholderSupersession(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "trusted-source", "agentsview")
			_, err := d.InsertRecallEntry(RecallEntry{
				ID:              "trusted-entry",
				Type:            "fact",
				Scope:           "project",
				Status:          "accepted",
				ReviewState:     "human_reviewed",
				Title:           "Trusted retry policy",
				Body:            "Retry flaky commands once.",
				Project:         "agentsview",
				SourceSessionID: "trusted-source",
				ProvenanceOK:    true,
			})
			require.NoError(t, err)
			input := strings.NewReader(`
{"candidate_id":"placeholder-replacement","supersedes_entry_id":"trusted-entry","type":"fact","scope":"project","title":"Unverified retry policy","body":"Retry flaky commands three times.","project":"agentsview","session_id":"missing-source","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)

			_, err = d.ImportAcceptedRecallEntriesJSONLWithOptions(
				context.Background(), input, RecallImportOptions{DryRun: dryRun},
			)

			require.ErrorContains(
				t, err,
				"unverified recall import cannot supersede provenance-valid entry trusted-entry",
			)
			trusted, getErr := d.GetRecallEntry(context.Background(), "trusted-entry")
			require.NoError(t, getErr)
			require.NotNil(t, trusted)
			assert.Equal(t, "accepted", trusted.Status)
			assert.Empty(t, trusted.SupersededByEntryID)
			assert.True(t, trusted.ProvenanceOK)
			replacement, getErr := d.GetRecallEntry(
				context.Background(), "placeholder-replacement",
			)
			require.NoError(t, getErr)
			assert.Nil(t, replacement)
		})
	}
}

func TestImportAcceptedRecallEntriesJSONLRejectsAlreadySupersededTarget(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			d := testDB(t)
			for _, id := range []string{"s1", "s2", "s3"} {
				insertSession(t, d, id, "agentsview")
			}
			_, err := d.InsertRecallEntry(RecallEntry{
				ID:              "old",
				Type:            "fact",
				Scope:           "project",
				Status:          "accepted",
				Title:           "Old retry policy",
				Body:            "Retry flaky commands once.",
				Project:         "agentsview",
				SourceSessionID: "s1",
			})
			require.NoError(t, err)
			_, err = d.SupersedeRecallEntry(context.Background(), "old", RecallEntry{
				ID:              "first-replacement",
				Type:            "fact",
				Scope:           "project",
				Status:          "accepted",
				Title:           "First replacement",
				Body:            "Retry flaky commands twice.",
				Project:         "agentsview",
				SourceSessionID: "s2",
			})
			require.NoError(t, err)
			input := strings.NewReader(`
{"candidate_id":"second-replacement","supersedes_entry_id":"old","type":"fact","scope":"project","title":"Second replacement","body":"Retry flaky commands three times.","project":"agentsview","session_id":"s3","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)

			_, err = d.ImportAcceptedRecallEntriesJSONLWithOptions(
				context.Background(), input, RecallImportOptions{DryRun: dryRun},
			)

			require.ErrorContains(t, err, "superseded entry old is not active")
			second, getErr := d.GetRecallEntry(context.Background(), "second-replacement")
			require.NoError(t, getErr)
			assert.Nil(t, second)
		})
	}
}

func TestImportAcceptedRecallEntriesJSONLDryRunTracksSupersessionWithinStream(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			d := testDB(t)
			for _, id := range []string{"s1", "s2", "s3"} {
				insertSession(t, d, id, "agentsview")
			}
			_, err := d.InsertRecallEntry(RecallEntry{
				ID:              "old",
				Type:            "fact",
				Scope:           "project",
				Status:          "accepted",
				Title:           "Old retry policy",
				Body:            "Retry flaky commands once.",
				Project:         "agentsview",
				SourceSessionID: "s1",
			})
			require.NoError(t, err)
			input := strings.NewReader(`
{"candidate_id":"first-replacement","supersedes_entry_id":"old","type":"fact","scope":"project","title":"First replacement","body":"Retry flaky commands twice.","project":"agentsview","session_id":"s2","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
{"candidate_id":"second-replacement","supersedes_entry_id":"old","type":"fact","scope":"project","title":"Second replacement","body":"Retry flaky commands three times.","project":"agentsview","session_id":"s3","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)

			result, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
				context.Background(), input, RecallImportOptions{DryRun: dryRun},
			)

			require.ErrorContains(t, err, "superseded entry old is not active")
			if dryRun {
				assert.Equal(t, 1, result.WouldImport)
				assert.Zero(t, result.Imported)
			} else {
				assert.Equal(t, 1, result.Imported)
				assert.Zero(t, result.WouldImport)
			}
			second, getErr := d.GetRecallEntry(context.Background(), "second-replacement")
			require.NoError(t, getErr)
			assert.Nil(t, second)
		})
	}
}

func TestImportAcceptedRecallEntriesJSONLDryRunProjectsSupersessionChain(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"s1", "s2"} {
		insertSession(t, d, id, "agentsview")
	}
	input := strings.NewReader(`
{"candidate_id":"first","type":"fact","scope":"project","title":"First policy","body":"Retry flaky commands twice.","project":"agentsview","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
{"candidate_id":"second","supersedes_entry_id":"first","type":"fact","scope":"project","title":"Second policy","body":"Retry flaky commands three times.","project":"agentsview","session_id":"s2","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(), input, RecallImportOptions{DryRun: true},
	)

	require.NoError(t, err)
	assert.Equal(t, 2, result.WouldImport)
	assert.Zero(t, result.Imported)
}

func TestImportAcceptedRecallEntriesJSONLTrimsIdentityAndScopeFields(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo/agentsview"
		s.GitBranch = "main"
	})
	messages := []Message{
		userMsg("s1", 3, "User saw cwd failure."),
		asstMsg("s1", 4, "I will inspect cwd."),
		userMsg("s1", 5, "Retry failed."),
		asstMsg("s1", 6, "[Read: main.go]"),
		userMsg("s1", 7, "That fixed it."),
	}
	messages[3].HasToolUse = true
	messages[3].ToolCalls = []ToolCall{
		{
			SessionID: "s1",
			ToolName:  "Read",
			Category:  "Read",
			ToolUseID: "toolu_1",
		},
	}
	insertMessages(t, d, messages...)
	input := strings.NewReader(`
{"candidate_id":" m-trim ","type":" debugging_method ","scope":" repository ","title":" Check cwd before file reads ","body":" Verify cwd before retrying failed reads. ","trigger":" file read failure ","confidence":0.92,"uncertainty":" low ","project":" agentsview ","cwd":" /repo/agentsview ","git_branch":" main ","agent":" codex ","session_id":" s1 ","episode_id":" ep1 ","run_id":" run1 ","extractor_method":" single ","model":" fake-model ","label":" correct ","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":[" toolu_1 "],"snippets":["Verify cwd before retrying"]}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(),
		input,
		RecallImportOptions{RequireExistingSessions: true},
	)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Imported)
	got, err := d.GetRecallEntry(context.Background(), "m-trim")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "debugging_method", got.Type)
	assert.Equal(t, "repository", got.Scope)
	assert.Equal(t, "Check cwd before file reads", got.Title)
	assert.Equal(t, "Verify cwd before retrying failed reads.", got.Body)
	assert.Equal(t, "file read failure", got.Trigger)
	assert.Equal(t, "low", got.Uncertainty)
	assert.Equal(t, "agentsview", got.Project)
	assert.Equal(t, "/repo/agentsview", got.CWD)
	assert.Equal(t, "main", got.GitBranch)
	assert.Equal(t, "codex", got.Agent)
	assert.Equal(t, "s1", got.SourceSessionID)
	assert.Equal(t, "ep1", got.SourceEpisodeID)
	assert.Equal(t, "run1", got.SourceRunID)
	assert.Equal(t, "single", got.ExtractorMethod)
	assert.Equal(t, "fake-model", got.Model)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, "toolu_1", got.Evidence[0].ToolUseID)
}

func TestImportAcceptedRecallEntriesJSONLDryRunReportsWouldImportAndSkips(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	input := strings.NewReader(`
{"candidate_id":"m-keeper","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","project":"agentsview","agent":"codex","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7}}
{"candidate_id":"m-not-transferable","type":"fact","scope":"project","title":"Local-only detail","body":"Local detail.","project":"agentsview","agent":"codex","session_id":"s1","label":"correct","transferable":false,"provenance_ok":true,"evidence":{"ordinal_start":1,"ordinal_end":1}}
{"candidate_id":"m-wrong","type":"fact","scope":"project","title":"Wrong detail","body":"Wrong detail.","project":"agentsview","agent":"codex","session_id":"s1","label":"wrong","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":2,"ordinal_end":2}}
`)

	result, err := d.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(), input, RecallImportOptions{DryRun: true},
	)

	require.NoError(t, err)
	assert.Equal(t, 0, result.Imported)
	assert.Equal(t, 1, result.WouldImport)
	assert.Equal(t, 2, result.Skipped)
	require.Len(t, result.WouldImportEntries, 1)
	assert.Equal(t, "m-keeper", result.WouldImportEntries[0].CandidateID)
	assert.Equal(t, "Check cwd before file reads", result.WouldImportEntries[0].Title)
	assert.Equal(t, "s1", result.WouldImportEntries[0].SourceSessionID)
	require.Len(t, result.SkippedEntries, 2)
	assert.Equal(t, "m-not-transferable", result.SkippedEntries[0].CandidateID)
	assert.Equal(t, "not_transferable", result.SkippedEntries[0].Reason)
	assert.Equal(t, "m-wrong", result.SkippedEntries[1].CandidateID)
	assert.Equal(t, "label_not_keeper", result.SkippedEntries[1].Reason)

	got, err := d.GetRecallEntry(context.Background(), "m-keeper")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestImportAcceptedRecallEntriesJSONLDryRunDedupsCandidateIDsLikeRealImport(t *testing.T) {
	const input = `
{"candidate_id":"dup1","type":"fact","scope":"project","title":"First","body":"First body.","project":"agentsview","agent":"codex","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":1,"ordinal_end":1}}
{"candidate_id":"dup1","type":"fact","scope":"project","title":"Second","body":"Second body.","project":"agentsview","agent":"codex","session_id":"s1","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":1,"ordinal_end":1}}
`

	dryDB := testDB(t)
	insertSession(t, dryDB, "s1", "agentsview")
	dry, err := dryDB.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(), strings.NewReader(input),
		RecallImportOptions{DryRun: true},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, dry.WouldImport)
	assert.Equal(t, 1, dry.Skipped)
	require.Len(t, dry.SkippedEntries, 1)
	assert.Equal(t, "duplicate", dry.SkippedEntries[0].Reason)

	realDB := testDB(t)
	insertSession(t, realDB, "s1", "agentsview")
	real, err := realDB.ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Background(), strings.NewReader(input),
		RecallImportOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, real.Imported)
	assert.Equal(t, 1, real.Skipped)

	// The dry-run's projected counts must match the real importer's so a
	// duplicate candidate_id is never double-counted as WouldImport.
	assert.Equal(t, real.Imported, dry.WouldImport)
	assert.Equal(t, real.Skipped, dry.Skipped)
}
