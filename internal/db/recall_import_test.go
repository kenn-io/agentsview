package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	assert.Equal(t, "toolu_1", got.Evidence[0].ToolUseID)
	assert.Equal(t, 3, got.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, 7, got.Evidence[0].MessageEndOrdinal)
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
	session, err := d.GetSession(context.Background(), "s-missing")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "agentsview", session.Project)
	assert.Equal(t, "codex", session.Agent)
	assert.Equal(t, "/repo/agentsview", session.Cwd)
	assert.Equal(t, "main", session.GitBranch)
	assert.Equal(t, "recall-import-placeholder", session.SourceVersion)
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
