package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/rawderive"
)

const (
	parityPhysicalPageSize = 128
	parityPhysicalMaxBytes = 32 << 20
	parityPhysicalMaxRows  = 100000
)

type parityPhysicalMember struct {
	Graph       rawderive.ParityGraph
	Physical    rawderive.ParityDigest
	Binding     rawderive.ParityDigest
	InvalidCode string
}

type parityPhysicalHashes struct {
	physical hash.Hash
	binding  hash.Hash
}

var errParityPhysicalLimit = errors.New("parity physical member exceeds retained bounds")

type parityRetainedBudget struct {
	bytes int
	rows  int
}

func (b *parityRetainedBudget) add(row bool, values ...string) error {
	nextBytes := b.bytes
	for _, value := range values {
		if len(value) > parityPhysicalMaxBytes-nextBytes {
			return errParityPhysicalLimit
		}
		nextBytes += len(value)
	}
	if row && b.rows >= parityPhysicalMaxRows {
		return errParityPhysicalLimit
	}
	b.bytes = nextBytes
	if row {
		b.rows++
	}
	return nil
}

func (b *parityRetainedBudget) addSession(s db.Session) error {
	values := []string{
		s.ID, s.Project, s.Machine, s.Agent, s.AgentLabel, s.Entrypoint,
		s.SessionKind, s.RelationshipType, s.Outcome, s.OutcomeConfidence,
		s.EndedWithRole, s.SecretsRulesVersion, s.Cwd, s.GitBranch,
		s.SourceSessionID, s.SourceVersion, s.TranscriptFidelity, s.CreatedAt,
	}
	values = append(values, s.ParentSessionIDs...)
	for _, pointer := range []*string{
		s.FirstMessage, s.DisplayName, s.SessionName, s.StartedAt, s.EndedAt,
		s.ParentSessionID, s.ParserParentSessionID, s.SignalsPendingSince,
		s.HealthGrade, s.DeletedAt, s.DeletionCause, s.SourceMissingAt,
		s.TerminationStatus, s.FilePath, s.LastEntryUUID, s.FileHash,
		s.LocalModifiedAt, s.TranscriptRevision,
	} {
		if pointer != nil {
			values = append(values, *pointer)
		}
	}
	return b.add(false, values...)
}

func newParityPhysicalHashes() parityPhysicalHashes {
	p := sha256.New()
	b := sha256.New()
	_, _ = p.Write([]byte("agentsview-parity-physical-v1"))
	_, _ = b.Write([]byte("agentsview-parity-physical-binding-v1"))
	return parityPhysicalHashes{physical: p, binding: b}
}

func parityHashBytes(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func parityHashString(h hash.Hash, value string) { parityHashBytes(h, []byte(value)) }

func parityHashInt(h hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = h.Write(encoded[:])
}

func parityHashBool(h hash.Hash, value bool) {
	if value {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}
}

func parityHashNullString(h hash.Hash, value sql.NullString) {
	parityHashBool(h, value.Valid)
	if value.Valid {
		parityHashString(h, value.String)
	}
}

func parityDigestFromHash(h hash.Hash) rawderive.ParityDigest {
	var out rawderive.ParityDigest
	copy(out[:], h.Sum(nil))
	return out
}

// readParityPhysicalMember reads every component through the caller's pinned
// repeatable-read transaction. The transaction is deliberately not hidden in
// this helper so no child query can escape the snapshot.
func readParityPhysicalMember(ctx context.Context, tx *sql.Tx, binding rawderive.ParityBinding, physicalID string, key rawderive.ParityMemberKey) (parityPhysicalMember, error) {
	member := parityPhysicalMember{Graph: rawderive.ParityGraph{Key: key}}
	if tx == nil || physicalID == "" {
		return member, fmt.Errorf("invalid parity physical member")
	}
	var rowCount int64
	var byteCount int64
	err := tx.QueryRowContext(ctx, `SELECT
	 (SELECT count(*) FROM messages WHERE session_id=$1)+
	 (SELECT count(*) FROM tool_calls WHERE session_id=$1)+
	 (SELECT count(*) FROM tool_result_events WHERE session_id=$1)+
	 (SELECT count(*) FROM usage_events WHERE session_id=$1)+
	 (SELECT count(*) FROM secret_findings WHERE session_id=$1),
	 COALESCE((SELECT sum(octet_length(role)+octet_length(content)+octet_length(thinking_text)+octet_length(model)+octet_length(reasoning_effort)+octet_length(token_usage)+
	  octet_length(provider_id)+octet_length(claude_message_id)+octet_length(claude_request_id)+octet_length(source_type)+octet_length(source_subtype)+octet_length(prompt_source)+
	  octet_length(source_uuid)+octet_length(source_parent_uuid)) FROM messages WHERE session_id=$1),0)+
	 COALESCE((SELECT sum(octet_length(tool_name)+octet_length(category)+octet_length(tool_use_id)+octet_length(COALESCE(input_json,''))+octet_length(COALESCE(skill_name,''))+
	  octet_length(COALESCE(result_content,''))+octet_length(COALESCE(subagent_session_id,''))+octet_length(COALESCE(file_path,''))) FROM tool_calls WHERE session_id=$1),0)+
	 COALESCE((SELECT sum(octet_length(COALESCE(tool_use_id,''))+octet_length(COALESCE(agent_id,''))+octet_length(COALESCE(subagent_session_id,''))+octet_length(source)+
	  octet_length(status)+octet_length(content)) FROM tool_result_events WHERE session_id=$1),0)+
	 COALESCE((SELECT sum(octet_length(source)+octet_length(model)+octet_length(provider_id)+octet_length(cost_status)+octet_length(cost_source)+octet_length(dedup_key)) FROM usage_events WHERE session_id=$1),0)+
	 COALESCE((SELECT sum(octet_length(rule_name)+octet_length(confidence)+octet_length(location_kind)+octet_length(redacted_match)+octet_length(rules_version)) FROM secret_findings WHERE session_id=$1),0)+
	 COALESCE((SELECT octet_length(id)+octet_length(project)+octet_length(machine)+octet_length(agent)+octet_length(agent_label)+octet_length(entrypoint)+octet_length(session_kind)+
	  octet_length(COALESCE(first_message,''))+octet_length(COALESCE(display_name,''))+octet_length(COALESCE(source_display_name,''))+octet_length(COALESCE(session_name,''))+
	  octet_length(COALESCE(parent_session_id,''))+octet_length(COALESCE(parser_parent_session_id,''))+octet_length(relationship_type)+octet_length(outcome)+octet_length(outcome_confidence)+
	  octet_length(ended_with_role)+octet_length(COALESCE(health_grade,''))+octet_length(cwd)+octet_length(git_branch)+octet_length(source_session_id)+octet_length(source_version)+
	  octet_length(transcript_fidelity)+octet_length(secrets_rules_version)+octet_length(COALESCE(termination_status,''))+octet_length(owner_marker)+octet_length(COALESCE(deletion_cause,''))+
	  octet_length(transcript_revision)+octet_length(source_archive_id)+octet_length(source_database_generation)+octet_length(provenance_kind)+octet_length(raw_group_id)+octet_length(raw_content_revision)
	 FROM sessions WHERE id=$1),0)`, physicalID).Scan(&rowCount, &byteCount)
	if err != nil {
		return member, err
	}
	bindingRows, bindingBytes, err := parityPhysicalBindingSize(ctx, tx, physicalID, key.SourceID, true)
	if err != nil {
		return member, err
	}
	rowCount += bindingRows
	byteCount += bindingBytes
	if rowCount > parityPhysicalMaxRows || byteCount > parityPhysicalMaxBytes {
		member.InvalidCode = "limit_exceeded"
		return member, nil
	}

	hashes := newParityPhysicalHashes()
	budget := &parityRetainedBudget{}
	if err := budget.add(false, key.SourceID, key.LogicalKey, key.Kind); err != nil {
		member.InvalidCode = "limit_exceeded"
		return member, nil
	}
	session, promptDiscarded, sourceDeleted, excluded, err := readParitySession(ctx, tx, physicalID, &hashes)
	if err != nil {
		return member, err
	}
	session.ID = physicalID
	if err = budget.addSession(session); err != nil {
		member.InvalidCode = "limit_exceeded"
		return member, nil
	}
	messages, invalid, err := readParityMessages(ctx, tx, physicalID, &hashes, budget)
	if err != nil {
		if errors.Is(err, errParityPhysicalLimit) {
			member.InvalidCode = "limit_exceeded"
			return member, nil
		}
		return member, err
	}
	if invalid {
		member.InvalidCode = "invalid"
	}
	usage, err := readParityUsage(ctx, tx, physicalID, budget)
	if err != nil {
		if errors.Is(err, errParityPhysicalLimit) {
			member.InvalidCode = "limit_exceeded"
			return member, nil
		}
		return member, err
	}
	findings, err := readParityFindings(ctx, tx, physicalID, budget)
	if err != nil {
		if errors.Is(err, errParityPhysicalLimit) {
			member.InvalidCode = "limit_exceeded"
			return member, nil
		}
		return member, err
	}
	links, err := readParityLinks(ctx, tx, physicalID, key.SourceID, hashes.binding, budget)
	if err != nil {
		if errors.Is(err, errParityPhysicalLimit) {
			member.InvalidCode = "limit_exceeded"
			return member, nil
		}
		return member, err
	}
	if session.ParserParentSessionID != nil {
		link := rawderive.ParityLink{Kind: "parser-parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: *session.ParserParentSessionID}
		if err = budget.add(true, link.Kind, link.Unresolved); err != nil {
			member.InvalidCode = "limit_exceeded"
			return member, nil
		}
		parityHashString(hashes.binding, link.Kind)
		parityHashInt(hashes.binding, -1)
		parityHashInt(hashes.binding, -1)
		parityHashInt(hashes.binding, -1)
		parityHashString(hashes.binding, link.Unresolved)
		links = append(links, link)
	}
	curatedExcluded, err := readParityOverlaysAndBinding(ctx, tx, physicalID, key, hashes.binding)
	if err != nil {
		return member, err
	}

	prepared := ingest.PreparedSession{
		Session: session, Messages: messages, UsageEvents: usage,
		Signals: ingest.SignalFields(session), Findings: findings,
	}
	member.Graph.Prepared = prepared
	member.Graph.PromptEvidenceDiscarded = promptDiscarded
	member.Graph.Links = links
	member.Graph.Overlay = rawderive.ParityOverlay{Excluded: excluded || curatedExcluded, SourceDeleted: sourceDeleted}
	member.Binding = parityDigestFromHash(hashes.binding)
	if member.InvalidCode != "" {
		parityHashString(hashes.physical, member.InvalidCode)
		member.Physical = parityDigestFromHash(hashes.physical)
		return member, nil
	}
	fingerprint, err := rawderive.FingerprintParity(ctx, binding, member.Graph)
	if err != nil {
		if errors.Is(err, rawderive.ErrParityGraphVersion) {
			member.InvalidCode = "unsupported"
		} else if strings.Contains(err.Error(), "exceeds supported bounds") {
			member.InvalidCode = "limit_exceeded"
		} else if strings.Contains(err.Error(), "dangling parity") || strings.Contains(err.Error(), "duplicate parity") {
			member.InvalidCode = "invalid"
		} else {
			return member, err
		}
		parityHashString(hashes.physical, member.InvalidCode)
		member.Physical = parityDigestFromHash(hashes.physical)
		return member, nil
	}
	parityHashBytes(hashes.physical, fingerprint.Semantic[:])
	parityHashBytes(hashes.physical, member.Binding[:])
	member.Physical = parityDigestFromHash(hashes.physical)
	return member, nil
}

func parityPhysicalBindingSize(ctx context.Context, q hostedQuerier, physicalID, sourceID string, includeRaw bool) (int64, int64, error) {
	var rows, bytes int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(sum(n),0),COALESCE(sum(bytes),0) FROM (
	 SELECT count(*) n,COALESCE(sum(octet_length(s.session_id)),0) bytes FROM starred_sessions s WHERE s.session_id=$1
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(e.id)),0) FROM excluded_sessions e WHERE e.id=$1
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(a.session_id)+octet_length(a.alias_id)),0) FROM session_aliases a WHERE a.session_id=$1
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(p.id::text)+octet_length(p.session_id)+octet_length(p.message_id::text)+octet_length(p.ordinal::text)+octet_length(p.source_uuid)+COALESCE(octet_length(p.note),0)),0) FROM pinned_messages p WHERE p.session_id=$1
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(p.source_id)+octet_length(p.device_id)+octet_length(p.provider)+octet_length(p.configured_root_id)+octet_length(p.source_key_sha256)+octet_length(p.selected_manifest_id)+octet_length(p.processing_version)+octet_length(p.projection_generation::text)+octet_length(p.selected_job_id::text)+COALESCE(octet_length(p.successful_manifest_id),0)+COALESCE(octet_length(p.last_attempt_manifest_id),0)+octet_length(p.membership_complete::text)+octet_length(p.diagnostics)),0) FROM raw_source_projections p WHERE p.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(g.source_id)+octet_length(g.generation::text)+octet_length(g.manifest_id)+octet_length(g.processing_version)),0) FROM raw_projection_generations g WHERE g.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(b.branch_id)+octet_length(b.source_id)+octet_length(b.group_id)+octet_length(b.member_id)+octet_length(b.session_id)+octet_length(b.content_revision)+octet_length(b.manifest_id)+octet_length(b.processing_version)+octet_length(b.projection_generation::text)+octet_length(b.active::text)+octet_length(b.prior_payload)),0) FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(s.branch_id)+octet_length(s.group_id)+octet_length(s.source_id)+octet_length(s.session_id)+COALESCE(octet_length(s.physical_session_id),0)+octet_length(s.manifest_id)+octet_length(s.content_revision)+octet_length(s.processing_version)+octet_length(s.projection_generation::text)),0) FROM session_sources s WHERE s.physical_session_id=$1 OR s.session_id=$1 OR s.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(r.session_id)+octet_length(r.group_id)+octet_length(r.content_revision)+octet_length(r.payload)+octet_length(r.recency_state::text)),0) FROM raw_content_revisions r WHERE r.session_id=$1 OR r.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2)
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(c.branch_id)+octet_length(c.manifest_id)+octet_length(c.projection_generation::text)+octet_length(c.processing_version)+octet_length(c.prior_contributed::text)+octet_length(c.payload)),0) FROM raw_source_contributions c WHERE c.branch_id IN (SELECT b.branch_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2)
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(c.group_id)+octet_length(c.branch_id)+octet_length(c.field)+octet_length(c.value::text)),0) FROM raw_curation c WHERE c.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2)
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(p.group_id)+octet_length(p.branch_id)+octet_length(p.message_key)+octet_length(p.ordinal::text)+octet_length(p.content_revision)+octet_length(p.pinned::text)+octet_length(p.note)),0) FROM raw_pins p WHERE p.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2)
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(a.alias_id)+octet_length(a.group_id)+octet_length(a.anchor_branch)),0) FROM raw_session_public_aliases a WHERE a.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2)
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(g.group_id)+octet_length(g.provider)+octet_length(g.logical_key)+octet_length(g.base_alias)),0) FROM raw_session_groups g WHERE g.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2)
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(l.branch_id)+octet_length(l.kind)+octet_length(l.ordinal::text)+octet_length(l.call_index::text)+octet_length(l.event_index::text)+octet_length(l.target_alias)),0) FROM raw_session_links l JOIN raw_session_branches b ON b.branch_id=l.branch_id WHERE b.session_id=$1 OR b.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(h.device_id)+octet_length(h.provider)+octet_length(h.configured_root_id)+octet_length(h.source_key)+octet_length(h.source_key_sha256)+COALESCE(octet_length(h.manifest_id),0)+COALESCE(octet_length(h.receipt),0)+octet_length(h.generation::text)),0) FROM raw_source_heads h JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE $3 AND p.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(m.manifest_id)+octet_length(m.device_id)+octet_length(m.provider)+octet_length(m.configured_root_id)+octet_length(m.source_key)+octet_length(m.source_key_sha256)+octet_length(m.capture_id)+octet_length(m.parent_receipt)+octet_length(m.receipt)+octet_length(m.generation::text)+octet_length(m.kind)+octet_length(m.canonical_json)),0) FROM raw_manifests m JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE $3 AND p.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(e.manifest_id)+octet_length(e.entry_index::text)+octet_length(e.path)+octet_length(e.path_sha256)+octet_length(e.entry_type)+octet_length(e.size_bytes::text)),0) FROM raw_manifest_entries e JOIN raw_manifests m USING(manifest_id) JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE $3 AND p.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(o.manifest_id)+octet_length(o.entry_index::text)+octet_length(o.object_index::text)+octet_length(o.sha256)+octet_length(o.size_bytes::text)),0) FROM raw_manifest_objects o JOIN raw_manifests m USING(manifest_id) JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE $3 AND p.source_id=$2
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(o.sha256)+octet_length(o.size_bytes::text)),0) FROM raw_objects o WHERE $3 AND EXISTS(SELECT 1 FROM raw_manifest_objects mo JOIN raw_manifests m USING(manifest_id) JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE p.source_id=$2 AND mo.sha256=o.sha256 AND mo.size_bytes=o.size_bytes)
	) sizes`, physicalID, sourceID, includeRaw).Scan(&rows, &bytes)
	return rows, bytes, err
}

func readParitySession(ctx context.Context, q hostedQuerier, id string, hashes *parityPhysicalHashes) (db.Session, bool, bool, bool, error) {
	var s db.Session
	var started, ended, deleted, sourceDeleted *time.Time
	var sourceDisplay sql.NullString
	var ownerMarker, sourceArchive, sourceGeneration, provenance, rawGroup, rawRevision string
	var promptDiscarded bool
	err := q.QueryRowContext(ctx, `SELECT id,project,machine,agent,agent_label,entrypoint,session_kind,
	 first_message,display_name,source_display_name,session_name,started_at,ended_at,message_count,user_message_count,
	 parent_session_id,parser_parent_session_id,relationship_type,total_output_tokens,peak_context_tokens,
	 has_total_output_tokens,has_peak_context_tokens,is_automated,prompt_evidence_discarded,
	 tool_failure_signal_count,tool_retry_count,edit_churn_count,consecutive_failure_max,outcome,outcome_confidence,
	 ended_with_role,final_failure_streak,compaction_count,mid_task_compaction_count,context_pressure_max,health_score,health_grade,
	 quality_signal_version,short_prompt_count,unstructured_start,missing_success_criteria_count,missing_verification_count,
	 duplicate_prompt_count,no_code_context_count,runaway_tool_loop_count,has_tool_calls,has_context_data,
	 cwd,git_branch,source_session_id,source_version,transcript_fidelity,parser_malformed_lines,is_truncated,secret_leak_count,
	 secrets_rules_version,termination_status,owner_marker,deleted_at,source_deleted_at,deletion_cause,transcript_revision,
	 source_archive_id,source_database_generation,provenance_kind,raw_group_id,raw_content_revision,data_version
	 FROM sessions WHERE id=$1`, id).Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent, &s.AgentLabel, &s.Entrypoint, &s.SessionKind,
		&s.FirstMessage, &s.DisplayName, &sourceDisplay, &s.SessionName, &started, &ended, &s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.ParserParentSessionID, &s.RelationshipType, &s.TotalOutputTokens, &s.PeakContextTokens,
		&s.HasTotalOutputTokens, &s.HasPeakContextTokens, &s.IsAutomated, &promptDiscarded,
		&s.ToolFailureSignalCount, &s.ToolRetryCount, &s.EditChurnCount, &s.ConsecutiveFailureMax, &s.Outcome, &s.OutcomeConfidence,
		&s.EndedWithRole, &s.FinalFailureStreak, &s.CompactionCount, &s.MidTaskCompactionCount, &s.ContextPressureMax, &s.HealthScore, &s.HealthGrade,
		&s.QualitySignalVersion, &s.ShortPromptCount, &s.UnstructuredStart, &s.MissingSuccessCriteriaCount, &s.MissingVerificationCount,
		&s.DuplicatePromptCount, &s.NoCodeContextCount, &s.RunawayToolLoopCount, &s.HasToolCalls, &s.HasContextData,
		&s.Cwd, &s.GitBranch, &s.SourceSessionID, &s.SourceVersion, &s.TranscriptFidelity, &s.ParserMalformedLines, &s.IsTruncated, &s.SecretLeakCount,
		&s.SecretsRulesVersion, &s.TerminationStatus, &ownerMarker, &deleted, &sourceDeleted, &s.DeletionCause, &s.TranscriptRevision,
		&sourceArchive, &sourceGeneration, &provenance, &rawGroup, &rawRevision, &s.DataVersion,
	)
	if err != nil {
		return db.Session{}, false, false, false, err
	}
	parityHashString(hashes.physical, "stored-signal-recency")
	parityHashString(hashes.physical, s.Outcome)
	parityHashString(hashes.physical, s.OutcomeConfidence)
	parityHashBool(hashes.physical, s.HealthScore != nil)
	if s.HealthScore != nil {
		parityHashInt(hashes.physical, int64(*s.HealthScore))
	}
	parityHashBool(hashes.physical, s.HealthGrade != nil)
	if s.HealthGrade != nil {
		parityHashString(hashes.physical, *s.HealthGrade)
	}
	if started != nil {
		value := FormatISO8601(*started)
		s.StartedAt = &value
	}
	if ended != nil {
		value := FormatISO8601(*ended)
		s.EndedAt = &value
	}
	if deleted != nil {
		value := FormatISO8601(*deleted)
		s.DeletedAt = &value
	}
	s.QualitySignals = s.StoredQualitySignals()
	transcriptRevision := ""
	if s.TranscriptRevision != nil {
		transcriptRevision = *s.TranscriptRevision
	}
	for _, value := range []string{s.ID, s.Machine, ownerMarker, sourceArchive, sourceGeneration, provenance, rawGroup, rawRevision, s.SecretsRulesVersion, transcriptRevision} {
		parityHashString(hashes.binding, value)
	}
	parityHashNullString(hashes.binding, nullString(s.DisplayName))
	parityHashNullString(hashes.binding, nullString(s.ParentSessionID))
	parityHashNullString(hashes.binding, sourceDisplay)
	parityHashBool(hashes.binding, deleted != nil)
	parityHashBool(hashes.binding, sourceDeleted != nil)
	parityHashNullString(hashes.binding, nullString(s.DeletionCause))
	parityHashInt(hashes.binding, int64(s.QualitySignalVersion))
	parityHashInt(hashes.binding, int64(s.DataVersion))
	return s, promptDiscarded, sourceDeleted != nil, deleted != nil, nil
}

func nullString(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *value, Valid: true}
}

func readParityMessages(ctx context.Context, tx *sql.Tx, id string, hashes *parityPhysicalHashes, budget *parityRetainedBudget) ([]db.Message, bool, error) {
	var messages []db.Message
	after := -1
	for {
		rows, err := tx.QueryContext(ctx, `SELECT session_id,ordinal,role,content,thinking_text,timestamp,has_thinking,has_tool_use,content_length,is_system,model,reasoning_effort,token_usage,context_tokens,output_tokens,provider_id,has_context_tokens,has_output_tokens,claude_message_id,claude_request_id,source_type,source_subtype,prompt_source,source_uuid,source_parent_uuid,is_sidechain,is_compact_boundary FROM messages WHERE session_id=$1 AND ordinal>$2 ORDER BY ordinal LIMIT $3`, id, after, parityPhysicalPageSize)
		if err != nil {
			return nil, false, err
		}
		page, scanErr := scanPGMessages(rows)
		rows.Close()
		if scanErr != nil {
			return nil, false, scanErr
		}
		for _, message := range page {
			parityHashInt(hashes.physical, int64(message.Ordinal))
			parityHashBool(hashes.physical, message.Timestamp != "")
		}
		for i := range page {
			page[i].SessionID = id
			message := &page[i]
			if err := budget.add(true, message.Role, message.Content, message.ThinkingText, message.Timestamp,
				message.Model, message.ReasoningEffort, message.ProviderID, string(message.TokenUsage),
				message.ClaudeMessageID, message.ClaudeRequestID, message.SourceType, message.SourceSubtype,
				message.PromptSource, message.SourceUUID, message.SourceParentUUID); err != nil {
				return nil, false, err
			}
		}
		messages = append(messages, page...)
		if len(page) < parityPhysicalPageSize {
			break
		}
		after = page[len(page)-1].Ordinal
	}
	var orphan bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM tool_calls c LEFT JOIN messages m ON m.session_id=c.session_id AND m.ordinal=c.message_ordinal
	 WHERE c.session_id=$1 AND m.session_id IS NULL UNION ALL
	 SELECT 1 FROM tool_result_events e LEFT JOIN tool_calls c ON c.session_id=e.session_id AND c.message_ordinal=e.tool_call_message_ordinal AND c.call_index=e.call_index
	 WHERE e.session_id=$1 AND c.session_id IS NULL)`, id).Scan(&orphan); err != nil {
		return nil, false, err
	}
	if err := attachParityToolRows(ctx, tx, id, messages, hashes.physical, budget); err != nil {
		return nil, false, err
	}
	return messages, orphan, nil
}

func attachParityToolRows(ctx context.Context, q hostedQuerier, id string, messages []db.Message, h hash.Hash, budget *parityRetainedBudget) error {
	ordinals := make(map[int]int, len(messages))
	for i := range messages {
		ordinals[messages[i].Ordinal] = i
	}
	afterOrdinal, afterCall := -1, -1
	for {
		rows, err := q.QueryContext(ctx, `SELECT message_ordinal,call_index,tool_name,category,tool_use_id,input_json,skill_name,result_content_length,result_content,subagent_session_id,file_path
			FROM tool_calls WHERE session_id=$1 AND (message_ordinal,call_index)>($2,$3) ORDER BY message_ordinal,call_index LIMIT $4`, id, afterOrdinal, afterCall, parityPhysicalPageSize)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var tc db.ToolCall
			var values [6]sql.NullString
			var length sql.NullInt64
			if err := rows.Scan(&afterOrdinal, &afterCall, &tc.ToolName, &tc.Category, &values[0], &values[1], &values[2], &length, &values[3], &values[4], &values[5]); err != nil {
				rows.Close()
				return err
			}
			parityHashInt(h, int64(afterOrdinal))
			parityHashInt(h, int64(afterCall))
			for _, value := range values {
				parityHashNullString(h, value)
			}
			parityHashBool(h, length.Valid)
			if length.Valid {
				parityHashInt(h, length.Int64)
				tc.ResultContentLength = int(length.Int64)
			}
			tc.SessionID, tc.CallIndex = id, afterCall
			tc.ToolUseID, tc.InputJSON, tc.SkillName = values[0].String, values[1].String, values[2].String
			tc.ResultContent, tc.SubagentSessionID, tc.FilePath = values[3].String, values[4].String, values[5].String
			if messageIndex, ok := ordinals[afterOrdinal]; ok {
				if err := budget.add(true, tc.ToolName, tc.Category, tc.ToolUseID, tc.InputJSON, tc.FilePath,
					tc.SkillName, tc.ResultContent, tc.SubagentSessionID, tc.Rendering); err != nil {
					rows.Close()
					return err
				}
				messages[messageIndex].ToolCalls = append(messages[messageIndex].ToolCalls, tc)
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count < parityPhysicalPageSize {
			break
		}
	}
	type callPosition struct{ message, call int }
	positions := make(map[[2]int]callPosition)
	for messageIndex := range messages {
		for callIndex := range messages[messageIndex].ToolCalls {
			positions[[2]int{messages[messageIndex].Ordinal, messages[messageIndex].ToolCalls[callIndex].CallIndex}] = callPosition{messageIndex, callIndex}
		}
	}
	afterOrdinal, afterCall, afterEvent := -1, -1, -1
	for {
		rows, err := q.QueryContext(ctx, `SELECT tool_call_message_ordinal,call_index,event_index,tool_use_id,agent_id,subagent_session_id,source,status,content,content_length,timestamp
			FROM tool_result_events WHERE session_id=$1 AND (tool_call_message_ordinal,call_index,event_index)>($2,$3,$4)
			ORDER BY tool_call_message_ordinal,call_index,event_index LIMIT $5`, id, afterOrdinal, afterCall, afterEvent, parityPhysicalPageSize)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var event db.ToolResultEvent
			var values [3]sql.NullString
			var timestamp sql.NullTime
			if err := rows.Scan(&afterOrdinal, &afterCall, &afterEvent, &values[0], &values[1], &values[2], &event.Source, &event.Status, &event.Content, &event.ContentLength, &timestamp); err != nil {
				rows.Close()
				return err
			}
			parityHashInt(h, int64(afterOrdinal))
			parityHashInt(h, int64(afterCall))
			parityHashInt(h, int64(afterEvent))
			for _, value := range values {
				parityHashNullString(h, value)
			}
			parityHashBool(h, timestamp.Valid)
			event.ToolUseID, event.AgentID, event.SubagentSessionID = values[0].String, values[1].String, values[2].String
			event.EventIndex = afterEvent
			if timestamp.Valid {
				event.Timestamp = FormatISO8601(timestamp.Time)
			}
			if position, ok := positions[[2]int{afterOrdinal, afterCall}]; ok {
				if err := budget.add(true, event.ToolUseID, event.AgentID, event.SubagentSessionID,
					event.Source, event.Status, event.Content, event.Timestamp, string(event.RawContentDigest)); err != nil {
					rows.Close()
					return err
				}
				call := &messages[position.message].ToolCalls[position.call]
				call.ResultEvents = append(call.ResultEvents, event)
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count < parityPhysicalPageSize {
			break
		}
	}
	db.RestoreMessageResultContent(messages)
	return nil
}

func readParityUsage(ctx context.Context, q hostedQuerier, id string, budget *parityRetainedBudget) ([]db.UsageEvent, error) {
	var out []db.UsageEvent
	var after int64
	for {
		rows, err := q.QueryContext(ctx, `SELECT id,message_ordinal,source,model,provider_id,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,reasoning_tokens,cost_microdollars,cost_status,cost_source,occurred_at,dedup_key FROM usage_events WHERE session_id=$1 AND id>$2 ORDER BY id LIMIT $3`, id, after, parityPhysicalPageSize)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			var rowID int64
			var event db.UsageEvent
			var cost sql.NullInt64
			var occurred *time.Time
			if err := rows.Scan(&rowID, &event.MessageOrdinal, &event.Source, &event.Model, &event.ProviderID, &event.InputTokens, &event.OutputTokens, &event.CacheCreationInputTokens, &event.CacheReadInputTokens, &event.ReasoningTokens, &cost, &event.CostStatus, &event.CostSource, &occurred, &event.DedupKey); err != nil {
				rows.Close()
				return nil, err
			}
			after = rowID
			event.SessionID = id
			if cost.Valid {
				event.Cost = &money.Money{Microdollars: cost.Int64}
			}
			if occurred != nil {
				event.OccurredAt = FormatISO8601(*occurred)
			}
			if err := budget.add(true, event.Source, event.Model, event.ProviderID, event.CostStatus,
				event.CostSource, event.OccurredAt, event.DedupKey); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, event)
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if count < parityPhysicalPageSize {
			return out, nil
		}
	}
}

func readParityFindings(ctx context.Context, q hostedQuerier, id string, budget *parityRetainedBudget) ([]db.SecretFinding, error) {
	var out []db.SecretFinding
	var after int64
	for {
		rows, err := q.QueryContext(ctx, `SELECT id,rule_name,confidence,location_kind,message_ordinal,call_index,event_index,match_start,match_end,match_index,redacted_match,rules_version FROM secret_findings WHERE session_id=$1 AND id>$2 ORDER BY id LIMIT $3`, id, after, parityPhysicalPageSize)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			var rowID int64
			var finding db.SecretFinding
			if err := rows.Scan(&rowID, &finding.RuleName, &finding.Confidence, &finding.LocationKind, &finding.MessageOrdinal, &finding.CallIndex, &finding.EventIndex, &finding.MatchStart, &finding.MatchEnd, &finding.MatchIndex, &finding.RedactedMatch, &finding.RulesVersion); err != nil {
				rows.Close()
				return nil, err
			}
			after = rowID
			finding.SessionID = id
			if err := budget.add(true, finding.RuleName, finding.Confidence, finding.LocationKind,
				finding.RedactedMatch, finding.RulesVersion); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, finding)
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if count < parityPhysicalPageSize {
			return out, nil
		}
	}
}

func readParityLinks(ctx context.Context, q hostedQuerier, physicalID, sourceID string, h hash.Hash, budget *parityRetainedBudget) ([]rawderive.ParityLink, error) {
	var links []rawderive.ParityLink
	var afterBranch, afterKind, afterTarget string
	afterOrdinal, afterCall, afterEvent := -1, -1, -1
	for {
		rows, err := q.QueryContext(ctx, `SELECT l.branch_id,l.kind,l.ordinal,l.call_index,l.event_index,l.target_alias
			FROM raw_session_links l JOIN raw_session_branches b ON b.branch_id=l.branch_id WHERE b.session_id=$1 AND b.source_id=$9
			AND (l.branch_id,l.kind,l.ordinal,l.call_index,l.event_index,l.target_alias)>($2,$3,$4,$5,$6,$7)
			ORDER BY l.branch_id,l.kind,l.ordinal,l.call_index,l.event_index,l.target_alias LIMIT $8`, physicalID,
			afterBranch, afterKind, afterOrdinal, afterCall, afterEvent, afterTarget, parityPhysicalPageSize, sourceID)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			var link rawderive.ParityLink
			if err := rows.Scan(&afterBranch, &link.Kind, &link.Ordinal, &link.CallIndex, &link.EventIndex, &link.Unresolved); err != nil {
				rows.Close()
				return nil, err
			}
			afterKind, afterOrdinal, afterCall, afterEvent, afterTarget = link.Kind, link.Ordinal, link.CallIndex, link.EventIndex, link.Unresolved
			parityHashString(h, link.Kind)
			parityHashInt(h, int64(link.Ordinal))
			parityHashInt(h, int64(link.CallIndex))
			parityHashInt(h, int64(link.EventIndex))
			parityHashString(h, link.Unresolved)
			if err := budget.add(true, link.Kind, link.Target.SourceID, link.Target.LogicalKey,
				link.Target.Kind, link.Unresolved); err != nil {
				rows.Close()
				return nil, err
			}
			links = append(links, link)
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if count < parityPhysicalPageSize {
			return links, nil
		}
	}
}

func readParityOverlaysAndBinding(ctx context.Context, q hostedQuerier, physicalID string, key rawderive.ParityMemberKey, h hash.Hash) (bool, error) {
	var excluded bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM excluded_sessions WHERE id=$1)`, physicalID).Scan(&excluded); err != nil {
		return false, err
	}
	parityHashBool(h, excluded)
	if err := hashParityRawPayloadRows(ctx, q, physicalID, key.SourceID, h); err != nil {
		return false, err
	}
	curatedExcluded, err := readParityEffectiveExclusion(ctx, q, physicalID, key.SourceID)
	if err != nil {
		return false, err
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT jsonb_build_array(alias_id)::text AS parity_page_key,session_id,alias_id FROM session_aliases WHERE session_id=$1 AND jsonb_build_array(alias_id)::text>@after ORDER BY jsonb_build_array(alias_id)::text LIMIT @limit`, []any{physicalID}},
		{`SELECT jsonb_build_array(session_id)::text AS parity_page_key,session_id FROM starred_sessions WHERE session_id=$1 AND jsonb_build_array(session_id)::text>@after ORDER BY jsonb_build_array(session_id)::text LIMIT @limit`, []any{physicalID}},
		{`SELECT jsonb_build_array(message_id,id)::text AS parity_page_key,id::text,session_id,message_id::text,ordinal::text,source_uuid,note FROM pinned_messages WHERE session_id=$1 AND jsonb_build_array(message_id,id)::text>@after ORDER BY jsonb_build_array(message_id,id)::text LIMIT @limit`, []any{physicalID}},
		{`SELECT jsonb_build_array(source_id)::text AS parity_page_key,source_id,device_id,provider,configured_root_id,source_key_sha256,selected_manifest_id,processing_version,projection_generation::text,selected_job_id::text,successful_manifest_id,last_attempt_manifest_id,membership_complete::text,diagnostics FROM raw_source_projections WHERE source_id=$1 AND jsonb_build_array(source_id)::text>@after ORDER BY jsonb_build_array(source_id)::text LIMIT @limit`, []any{key.SourceID}},
		{`SELECT jsonb_build_array(group_id)::text AS parity_page_key,group_id,provider,logical_key,base_alias FROM raw_session_groups WHERE group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2) AND jsonb_build_array(group_id)::text>@after ORDER BY jsonb_build_array(group_id)::text LIMIT @limit`, []any{physicalID, key.SourceID}},
		{`SELECT jsonb_build_array(branch_id)::text AS parity_page_key,branch_id,group_id,source_id,session_id,physical_session_id,manifest_id,content_revision,processing_version,projection_generation::text FROM session_sources WHERE (physical_session_id=$1 OR session_id=$1 OR source_id=$2) AND jsonb_build_array(branch_id)::text>@after ORDER BY jsonb_build_array(branch_id)::text LIMIT @limit`, []any{physicalID, key.SourceID}},
		{`SELECT jsonb_build_array(c.group_id,c.branch_id,c.field)::text AS parity_page_key,c.group_id,c.branch_id,c.field,c.value::text FROM raw_curation c WHERE c.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2) AND jsonb_build_array(c.group_id,c.branch_id,c.field)::text>@after ORDER BY jsonb_build_array(c.group_id,c.branch_id,c.field)::text LIMIT @limit`, []any{physicalID, key.SourceID}},
		{`SELECT jsonb_build_array(p.group_id,p.branch_id,p.message_key)::text AS parity_page_key,p.group_id,p.branch_id,p.message_key,p.ordinal::text,p.content_revision,p.pinned::text,p.note FROM raw_pins p WHERE p.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2) AND jsonb_build_array(p.group_id,p.branch_id,p.message_key)::text>@after ORDER BY jsonb_build_array(p.group_id,p.branch_id,p.message_key)::text LIMIT @limit`, []any{physicalID, key.SourceID}},
		{`SELECT jsonb_build_array(a.alias_id,a.group_id)::text AS parity_page_key,a.alias_id,a.group_id,a.anchor_branch FROM raw_session_public_aliases a WHERE a.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2) AND jsonb_build_array(a.alias_id,a.group_id)::text>@after ORDER BY jsonb_build_array(a.alias_id,a.group_id)::text LIMIT @limit`, []any{physicalID, key.SourceID}},
		{`SELECT jsonb_build_array(generation)::text AS parity_page_key,generation::text,manifest_id,processing_version FROM raw_projection_generations WHERE source_id=$1 AND jsonb_build_array(generation)::text>@after ORDER BY jsonb_build_array(generation)::text LIMIT @limit`, []any{key.SourceID}},
		{`SELECT jsonb_build_array(h.device_id,h.provider,h.configured_root_id,h.source_key_sha256)::text AS parity_page_key,h.device_id,h.provider,h.configured_root_id,h.source_key,h.source_key_sha256,h.manifest_id,h.receipt,h.generation::text FROM raw_source_heads h JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE p.source_id=$1 AND jsonb_build_array(h.device_id,h.provider,h.configured_root_id,h.source_key_sha256)::text>@after ORDER BY jsonb_build_array(h.device_id,h.provider,h.configured_root_id,h.source_key_sha256)::text LIMIT @limit`, []any{key.SourceID}},
		{`SELECT jsonb_build_array(m.manifest_id)::text AS parity_page_key,m.manifest_id,m.device_id,m.provider,m.configured_root_id,m.source_key,m.source_key_sha256,m.capture_id,m.parent_receipt,m.receipt,m.generation::text,m.kind,m.canonical_json FROM raw_manifests m JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE p.source_id=$1 AND jsonb_build_array(m.manifest_id)::text>@after ORDER BY jsonb_build_array(m.manifest_id)::text LIMIT @limit`, []any{key.SourceID}},
		{`SELECT jsonb_build_array(e.manifest_id,e.entry_index)::text AS parity_page_key,e.manifest_id,e.entry_index::text,e.path,e.path_sha256,e.entry_type,e.size_bytes::text FROM raw_manifest_entries e JOIN raw_manifests m USING(manifest_id) JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE p.source_id=$1 AND jsonb_build_array(e.manifest_id,e.entry_index)::text>@after ORDER BY jsonb_build_array(e.manifest_id,e.entry_index)::text LIMIT @limit`, []any{key.SourceID}},
		{`SELECT jsonb_build_array(o.manifest_id,o.entry_index,o.object_index)::text AS parity_page_key,o.manifest_id,o.entry_index::text,o.object_index::text,o.sha256,o.size_bytes::text FROM raw_manifest_objects o JOIN raw_manifests m USING(manifest_id) JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE p.source_id=$1 AND jsonb_build_array(o.manifest_id,o.entry_index,o.object_index)::text>@after ORDER BY jsonb_build_array(o.manifest_id,o.entry_index,o.object_index)::text LIMIT @limit`, []any{key.SourceID}},
		{`SELECT jsonb_build_array(o.sha256)::text AS parity_page_key,o.sha256,o.size_bytes::text FROM raw_objects o WHERE EXISTS(SELECT 1 FROM raw_manifest_objects mo JOIN raw_manifests m USING(manifest_id) JOIN raw_source_projections p USING(device_id,provider,configured_root_id,source_key_sha256) WHERE p.source_id=$1 AND mo.sha256=o.sha256 AND mo.size_bytes=o.size_bytes) AND jsonb_build_array(o.sha256)::text>@after ORDER BY jsonb_build_array(o.sha256)::text LIMIT @limit`, []any{key.SourceID}},
	}
	for _, query := range queries {
		if err := hashParityTextPages(ctx, q, query.sql, query.args, h); err != nil {
			return false, err
		}
	}
	return excluded || curatedExcluded, nil
}

func hashParityTextPages(ctx context.Context, q hostedQuerier, query string, args []any, h hash.Hash) error {
	after := ""
	var totalRows, totalBytes int64
	for {
		pageArgs := append(append([]any(nil), args...), after, parityPhysicalPageSize)
		pageQuery := strings.ReplaceAll(query, "@after", fmt.Sprintf("$%d", len(args)+1))
		pageQuery = strings.ReplaceAll(pageQuery, "@limit", fmt.Sprintf("$%d", len(args)+2))
		rows, err := q.QueryContext(ctx, pageQuery, pageArgs...)
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		count := 0
		for rows.Next() {
			dest := make([]any, len(cols))
			values := make([]sql.NullString, len(cols))
			for i := range dest {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return err
			}
			after = values[0].String
			for i := 1; i < len(values); i++ {
				parityHashString(h, cols[i])
				parityHashNullString(h, values[i])
				totalBytes += int64(len(cols[i]) + len(values[i].String))
			}
			count++
			totalRows++
			if totalRows > parityPhysicalMaxRows || totalBytes > parityPhysicalMaxBytes {
				rows.Close()
				return errParityPhysicalLimit
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count < parityPhysicalPageSize {
			return nil
		}
	}
}

func readParityEffectiveExclusion(ctx context.Context, q hostedQuerier, physicalID, sourceID string) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT b.group_id,b.branch_id
		FROM raw_session_branches b JOIN session_sources s
		ON s.branch_id=b.branch_id AND s.group_id=b.group_id AND s.source_id=b.source_id AND s.session_id=b.session_id
		WHERE b.source_id=$1 AND s.physical_session_id=$2 ORDER BY b.branch_id LIMIT 2`, sourceID, physicalID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var groupID, branchID string
	count := 0
	for rows.Next() {
		if err := rows.Scan(&groupID, &branchID); err != nil {
			return false, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	if count != 1 {
		return false, fmt.Errorf("ambiguous parity member source binding")
	}
	var value []byte
	err = q.QueryRowContext(ctx, `SELECT value FROM raw_curation WHERE group_id=$1 AND field='excluded' AND branch_id IN ('',$2) ORDER BY (branch_id=$2) DESC LIMIT 1`, groupID, branchID).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return string(value) == "true", nil
}

func hashParityRawPayloadRows(ctx context.Context, q hostedQuerier, physicalID, sourceID string, h hash.Hash) error {
	for _, query := range []string{
		`SELECT jsonb_build_array(branch_id)::text AS parity_page_key,branch_id,source_id,group_id,member_id,session_id,content_revision,manifest_id,processing_version,projection_generation::text,active::text,prior_payload
		 FROM raw_session_branches WHERE (session_id=$1 OR source_id=$2) AND jsonb_build_array(branch_id)::text>@after ORDER BY jsonb_build_array(branch_id)::text LIMIT @limit`,
		`SELECT jsonb_build_array(r.session_id)::text AS parity_page_key,r.session_id,r.group_id,r.content_revision,r.payload,r.recency_state::text
		 FROM raw_content_revisions r WHERE (r.session_id=$1 OR r.group_id IN (SELECT b.group_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2))
		 AND jsonb_build_array(r.session_id)::text>@after ORDER BY jsonb_build_array(r.session_id)::text LIMIT @limit`,
		`SELECT jsonb_build_array(c.branch_id,c.projection_generation,c.manifest_id)::text AS parity_page_key,c.branch_id,c.manifest_id,c.projection_generation::text,c.processing_version,c.prior_contributed::text,c.payload
		 FROM raw_source_contributions c WHERE c.branch_id IN (SELECT b.branch_id FROM raw_session_branches b WHERE b.session_id=$1 OR b.source_id=$2)
		 AND jsonb_build_array(c.branch_id,c.projection_generation,c.manifest_id)::text>@after ORDER BY jsonb_build_array(c.branch_id,c.projection_generation,c.manifest_id)::text LIMIT @limit`,
	} {
		if err := hashParityTextPages(ctx, q, query, []any{physicalID, sourceID}, h); err != nil {
			return err
		}
	}
	return nil
}
