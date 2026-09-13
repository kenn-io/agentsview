//go:build pgtest

package postgres

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func parityPhysicalBinding(tenant string) rawderive.ParityBinding {
	return rawderive.ParityBinding{
		Request: rawderive.ParityRequest{
			RunID: "00000000-0000-4000-8000-000000000001", RuntimeID: "00000000-0000-4000-8000-000000000002", BaselineProfile: "before",
			Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentClaude, RootID: "root-a"},
		},
		BaselineID: "00000000-0000-4000-8000-000000000003", Tenant: tenant,
		Versions:   rawderive.ParityVersions{Data: 1, Quality: 1, SecretRules: "rules-v1", Preparation: rawderive.ParityPreparationVersion, Projection: rawderive.ParityProjectionVersion, Comparison: rawderive.ParitySchemaVersion},
		ObservedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
	}
}

func seedParityPhysicalMember(t *testing.T, f hostedFixture) {
	t.Helper()
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(
		id,project,machine,agent,session_name,message_count,user_message_count,data_version,quality_signal_version,secrets_rules_version,prompt_evidence_discarded,source_session_id,provenance_kind,raw_group_id,raw_content_revision)
		VALUES('physical','project','device-a','claude','provider title',1,1,1,1,'rules-v1',TRUE,'source-session','raw','group-a','revision-a');
		INSERT INTO messages(session_id,ordinal,role,content,content_length,output_tokens,has_output_tokens,token_usage)
		VALUES('physical',0,'assistant','hello',5,3,TRUE,'{"output_tokens":3}');
		INSERT INTO tool_calls(session_id,message_ordinal,call_index,tool_name,category,tool_use_id,input_json,result_content_length,result_content)
		VALUES('physical',0,0,'Read','read','call-a','{"path":"a"}',4,'');
		INSERT INTO tool_result_events(session_id,tool_call_message_ordinal,call_index,event_index,source,status,content,content_length)
		VALUES('physical',0,0,0,'tool','completed','done',4);
		INSERT INTO usage_events(session_id,message_ordinal,source,model,input_tokens,output_tokens,cost_microdollars,cost_status,cost_source,dedup_key)
		VALUES('physical',0,'api','model-a',2,3,NULL,'unpriced','catalog','usage-a');
		INSERT INTO secret_findings(session_id,rule_name,confidence,location_kind,message_ordinal,match_start,match_end,match_index,redacted_match,rules_version)
		VALUES('physical','rule-a','stored-corrupt-confidence','message',0,0,91,0,'redacted','rules-v1')`)
	require.NoError(t, err)
}

func TestParityPhysicalRowsDetectIndependentChildMutation(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	binding := parityPhysicalBinding(f.tenant)
	key := rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"}
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	before, err := readParityPhysicalMember(t.Context(), tx, binding, "physical", key)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	assert.Equal(t, "provider title", *before.Graph.Prepared.Session.SessionName)
	assert.True(t, before.Graph.PromptEvidenceDiscarded)
	assert.Equal(t, "done", before.Graph.Prepared.Messages[0].ToolCalls[0].ResultContent, "single event restores the elided summary")
	assert.Nil(t, before.Graph.Prepared.UsageEvents[0].Cost)
	assert.Equal(t, 91, before.Graph.Prepared.Findings[0].MatchEnd)
	assert.Equal(t, "stored-corrupt-confidence", before.Graph.Prepared.Findings[0].Confidence)
	semanticBefore, err := rawderive.FingerprintParity(t.Context(), binding, before.Graph)
	require.NoError(t, err)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET outcome='another-stored-value',outcome_confidence='another-confidence',health_score=99,health_grade='Q' WHERE id='physical'`)
	require.NoError(t, err)
	tx, err = f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	storedChanged, err := readParityPhysicalMember(t.Context(), tx, binding, "physical", key)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	semanticStoredChanged, err := rawderive.FingerprintParity(t.Context(), binding, storedChanged.Graph)
	require.NoError(t, err)
	assert.Equal(t, semanticBefore.Semantic, semanticStoredChanged.Semantic, "recency normalization settles stored outcome values")
	assert.NotEqual(t, before.Physical, storedChanged.Physical, "physical fingerprint retains original stored recency values")

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE tool_result_events SET status='errored' WHERE session_id='physical'`)
	require.NoError(t, err)
	tx, err = f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	after, err := readParityPhysicalMember(t.Context(), tx, binding, "physical", key)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	assert.NotEqual(t, storedChanged.Physical, after.Physical)
	assert.NotEqual(t, storedChanged.Graph.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].Status, after.Graph.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].Status)
}

func TestParityPhysicalRowsFingerprintStoredSessionAndPinIdentity(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note) VALUES('physical',7,0,'message-a','note')`)
	require.NoError(t, err)
	binding := parityPhysicalBinding(f.tenant)
	key := rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"}
	read := func() parityPhysicalMember {
		tx, txErr := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		require.NoError(t, txErr)
		defer tx.Rollback()
		member, readErr := readParityPhysicalMember(t.Context(), tx, binding, "physical", key)
		require.NoError(t, readErr)
		return member
	}
	previous := read()
	for _, mutation := range []string{
		`UPDATE sessions SET display_name='stored display' WHERE id='physical'`,
		`UPDATE sessions SET parent_session_id='stored parent' WHERE id='physical'`,
		`UPDATE pinned_messages SET id=id+1000 WHERE session_id='physical'`,
	} {
		_, err = f.runtime.ExecContext(t.Context(), mutation)
		require.NoError(t, err)
		changed := read()
		assert.NotEqual(t, previous.Physical, changed.Physical, mutation)
		previous = changed
	}
}

func TestParityPhysicalRowsUseOneSnapshot(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	binding := parityPhysicalBinding(f.tenant)
	key := rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"}
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	before, err := readParityPhysicalMember(t.Context(), tx, binding, "physical", key)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE messages SET output_tokens=9 WHERE session_id='physical' AND ordinal=0`)
	require.NoError(t, err)
	after, err := readParityPhysicalMember(t.Context(), tx, binding, "physical", key)
	require.NoError(t, err)
	assert.Equal(t, before.Physical, after.Physical)

	newTx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	changed, err := readParityPhysicalMember(t.Context(), newTx, binding, "physical", key)
	require.NoError(t, err)
	require.NoError(t, newTx.Rollback())
	assert.NotEqual(t, before.Physical, changed.Physical)
}

func TestParityPhysicalRowsRejectOrphanChildren(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO tool_calls(session_id,message_ordinal,call_index,tool_name,category) VALUES('physical',99,0,'orphan','read')`)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
	require.NoError(t, err)
	assert.Equal(t, "invalid", member.InvalidCode)
}

func TestParityPhysicalRowsPreserveNoMessageUsageAndStoredSignals(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(
		id,project,machine,agent,message_count,user_message_count,data_version,quality_signal_version,secrets_rules_version,
		tool_retry_count,outcome,outcome_confidence,health_score,health_grade)
		VALUES('billing','project','device-a','claude',0,0,1,1,'rules-v1',17,'corrupt-outcome','corrupt-confidence',3,'Z');
		INSERT INTO usage_events(session_id,message_ordinal,source,model,input_tokens,output_tokens,cost_microdollars,cost_status,cost_source,dedup_key)
		VALUES('billing',NULL,'api','model-a',2,3,0,'priced','catalog','usage-without-message')`)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "billing", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-b", Kind: "session"})
	require.NoError(t, err)
	require.Len(t, member.Graph.Prepared.UsageEvents, 1)
	assert.Nil(t, member.Graph.Prepared.UsageEvents[0].MessageOrdinal)
	require.NotNil(t, member.Graph.Prepared.UsageEvents[0].Cost)
	assert.Equal(t, int64(0), member.Graph.Prepared.UsageEvents[0].Cost.Microdollars)
	assert.Equal(t, 17, member.Graph.Prepared.Session.ToolRetryCount)
	assert.Equal(t, "corrupt-outcome", member.Graph.Prepared.Session.Outcome)
}

func TestParityPhysicalRowsDistinguishSQLNullToolText(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE tool_calls SET input_json=NULL,result_content=NULL WHERE session_id='physical'`)
	require.NoError(t, err)
	read := func() parityPhysicalMember {
		tx, txErr := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		require.NoError(t, txErr)
		defer tx.Rollback()
		member, readErr := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
		require.NoError(t, readErr)
		return member
	}
	nullText := read()
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE tool_calls SET input_json='',result_content='' WHERE session_id='physical'`)
	require.NoError(t, err)
	emptyCallText := read()
	assert.Equal(t, nullText.Graph, emptyCallText.Graph, "existing typed loader normalizes nullable call text")
	assert.NotEqual(t, nullText.Physical, emptyCallText.Physical, "physical fingerprint retains call SQL NULL")
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE tool_result_events SET tool_use_id='',agent_id='',subagent_session_id='' WHERE session_id='physical'`)
	require.NoError(t, err)
	emptyEventText := read()
	assert.Equal(t, emptyCallText.Graph, emptyEventText.Graph, "existing typed loader normalizes nullable result text")
	assert.NotEqual(t, emptyCallText.Physical, emptyEventText.Physical, "physical fingerprint retains result SQL NULL")
}

func TestParityPhysicalRowsRejectOversizedBeforeLoadingContent(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count,data_version,quality_signal_version,secrets_rules_version)
		VALUES('oversized','project','device-a','claude',1,1,1,1,'rules-v1')`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content,thinking_text,token_usage)
		VALUES('oversized',0,'user',repeat('x',$1),'','')`, parityPhysicalMaxBytes+1)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "oversized", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-big", Kind: "session"})
	require.NoError(t, err)
	assert.Equal(t, "limit_exceeded", member.InvalidCode)
	assert.Empty(t, member.Graph.Prepared.Messages)
	assert.Empty(t, strings.TrimSpace(member.Graph.Prepared.Session.ID))
}

func TestParityPhysicalRowsRejectCompressedOversizedBindingBeforeLoading(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO pinned_messages(session_id,message_id,ordinal,note) VALUES('physical',9,0,repeat('x',$1))`, parityPhysicalMaxBytes+1)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
	require.NoError(t, err)
	assert.Equal(t, "limit_exceeded", member.InvalidCode)
	assert.Empty(t, member.Graph.Prepared.Messages)
}

func TestParityPhysicalRowsShareRepeatedSessionIDWithinRetainedBudget(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	physicalID := strings.Repeat("session-", 225)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count,data_version,quality_signal_version,secrets_rules_version)
		VALUES($1,'project','device-a','claude',20000,20000,1,1,'rules-v1')`, physicalID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content)
		SELECT $1,n,'user','' FROM generate_series(0,19999) n`, physicalID)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), physicalID, rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-large-id", Kind: "session"})
	require.NoError(t, err)
	assert.Empty(t, member.InvalidCode)
	require.Len(t, member.Graph.Prepared.Messages, 20000)
	assert.Equal(t, physicalID, member.Graph.Prepared.Messages[19999].SessionID)
}

func TestParityPhysicalRowsReadDenseChildPages(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO tool_calls(session_id,message_ordinal,call_index,tool_name,category,tool_use_id,input_json)
		SELECT 'physical',0,n,'Tool-'||n,'read','call-'||n,'{}' FROM generate_series(1,256) n;
		INSERT INTO tool_result_events(session_id,tool_call_message_ordinal,call_index,event_index,source,status,content,content_length)
		SELECT 'physical',0,n,0,'tool','completed','result-'||n,length('result-'||n) FROM generate_series(1,256) n`)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	member, err := readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
	require.NoError(t, err)
	require.Len(t, member.Graph.Prepared.Messages, 1)
	require.Len(t, member.Graph.Prepared.Messages[0].ToolCalls, 257)
	assert.Equal(t, "result-256", member.Graph.Prepared.Messages[0].ToolCalls[256].ResultEvents[0].Content)
}

func TestParityPhysicalRowsBindRawSourceAndCurationColumns(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, accepted := f.accept(t, "device-a", "parity-binding", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, projectionOutcome("binding-content")))
	identity, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	sourceID := rawSourceID(manifest)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'','display_name','"first"'::jsonb)`, identity.GroupID)
	require.NoError(t, err)

	binding := parityPhysicalBinding(f.tenant)
	binding.Request.Cohort.Provider = parser.AgentCodex
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT data_version,quality_signal_version,secrets_rules_version FROM sessions WHERE id=$1`, identity.SessionID).Scan(&binding.Versions.Data, &binding.Versions.Quality, &binding.Versions.SecretRules))
	reader := newParityReadRole(t, f.hostedFixture)
	read := func() parityPhysicalMember {
		tx, txErr := reader.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		require.NoError(t, txErr)
		defer tx.Rollback()
		member, readErr := readParityPhysicalMember(t.Context(), tx, binding, identity.SessionID, rawderive.ParityMemberKey{SourceID: sourceID, LogicalKey: "codex:portable", Kind: "session"})
		require.NoError(t, readErr)
		return member
	}
	before := read()
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_source_projections SET diagnostics='independent-source-mutation' WHERE source_id=$1`, sourceID)
	require.NoError(t, err)
	sourceChanged := read()
	assert.Equal(t, before.Graph, sourceChanged.Graph)
	assert.NotEqual(t, before.Binding, sourceChanged.Binding)
	assert.NotEqual(t, before.Physical, sourceChanged.Physical)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_curation SET value='"second"'::jsonb WHERE group_id=$1 AND field='display_name'`, identity.GroupID)
	require.NoError(t, err)
	curationChanged := read()
	assert.Equal(t, sourceChanged.Graph, curationChanged.Graph)
	assert.NotEqual(t, sourceChanged.Binding, curationChanged.Binding)
	assert.NotEqual(t, sourceChanged.Physical, curationChanged.Physical)

	f.accept(t, "device-a", "parity-binding-next", accepted.Receipt)
	manifestChanged := read()
	assert.Equal(t, curationChanged.Graph, manifestChanged.Graph)
	assert.NotEqual(t, curationChanged.Binding, manifestChanged.Binding)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_corpus_state SET corpus_revision=corpus_revision+1 WHERE singleton=1`)
	require.NoError(t, err)
	corpusChanged := read()
	assert.Equal(t, manifestChanged.Graph, corpusChanged.Graph)
	assert.Equal(t, manifestChanged.Binding, corpusChanged.Binding)
	assert.Equal(t, manifestChanged.Physical, corpusChanged.Physical)
}

func TestParityPhysicalRowsResolveExclusionForAuthenticatedMember(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.accept(t, "device-a", "parity-exclusion", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, projectionOutcome("member")))
	identity, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	sourceID := rawSourceID(manifest)
	var groupID, branchID, selectedManifest, contentRevision, processingVersion string
	var generation int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT group_id,branch_id,manifest_id,content_revision,processing_version,projection_generation FROM raw_session_branches WHERE source_id=$1`, sourceID).Scan(&groupID, &branchID, &selectedManifest, &contentRevision, &processingVersion, &generation))
	binding := parityPhysicalBinding(f.tenant)
	binding.Request.Cohort.Provider = parser.AgentCodex
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT data_version,quality_signal_version,secrets_rules_version FROM sessions WHERE id=$1`, identity.SessionID).Scan(&binding.Versions.Data, &binding.Versions.Quality, &binding.Versions.SecretRules))
	readExcluded := func() bool {
		tx, txErr := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		require.NoError(t, txErr)
		defer tx.Rollback()
		member, readErr := readParityPhysicalMember(t.Context(), tx, binding, identity.SessionID, rawderive.ParityMemberKey{SourceID: sourceID, LogicalKey: "codex:portable", Kind: "session"})
		require.NoError(t, readErr)
		return member.Graph.Overlay.Excluded
	}

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO raw_session_groups(group_id,provider,logical_key,base_alias) VALUES('unrelated-group','codex','unrelated','unrelated')`, nil},
		{`INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload) VALUES('unrelated-session','unrelated-group','unrelated-revision','{}')`, nil},
		{`INSERT INTO raw_session_branches(branch_id,source_id,group_id,member_id,session_id,content_revision,manifest_id,processing_version,projection_generation,active,prior_payload)
		 VALUES('unrelated-branch',$1,'unrelated-group','unrelated-member','unrelated-session','unrelated-revision',$2,$3,$4,TRUE,'{}')`, []any{sourceID, selectedManifest, processingVersion, generation}},
		{`INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES('unrelated-group','','excluded','true')`, nil},
	} {
		_, err = f.runtime.ExecContext(t.Context(), statement.sql, statement.args...)
		require.NoError(t, err)
	}
	assert.False(t, readExcluded(), "another group in the same source cannot exclude this member")

	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM raw_curation`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'sibling-branch','excluded','true')`, groupID)
	require.NoError(t, err)
	assert.False(t, readExcluded(), "a sibling branch cannot exclude this member")

	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM raw_curation`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'','excluded','true'),($1,$2,'excluded','false')`, groupID, branchID)
	require.NoError(t, err)
	assert.False(t, readExcluded(), "an explicit branch false overrides the group default")
}
