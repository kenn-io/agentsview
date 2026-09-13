//go:build pgtest

package postgres

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type parityPhysicalClassification struct {
	table, semantic, binding, excluded string
}

var parityPhysicalClassifications = []parityPhysicalClassification{
	{table: "sessions", semantic: "project,agent,agent_label,entrypoint,session_kind,first_message,session_name,started_at,ended_at,message_count,user_message_count,parent_session_id,parser_parent_session_id,relationship_type,total_output_tokens,peak_context_tokens,has_total_output_tokens,has_peak_context_tokens,is_automated,prompt_evidence_discarded,tool_failure_signal_count,tool_retry_count,edit_churn_count,consecutive_failure_max,outcome,outcome_confidence,ended_with_role,final_failure_streak,compaction_count,mid_task_compaction_count,context_pressure_max,health_score,health_grade,short_prompt_count,unstructured_start,missing_success_criteria_count,missing_verification_count,duplicate_prompt_count,no_code_context_count,runaway_tool_loop_count,termination_status,has_tool_calls,has_context_data,cwd,git_branch,source_session_id,source_version,transcript_fidelity,parser_malformed_lines,is_truncated,secret_leak_count", binding: "id,machine,owner_marker,display_name,source_display_name,deleted_at,source_deleted_at,deletion_cause,quality_signal_version,transcript_revision,source_archive_id,source_database_generation,provenance_kind,raw_group_id,raw_content_revision,data_version,secrets_rules_version", excluded: "created_at,signals_pending_since,file_path,updated_at"},
	{table: "messages", semantic: "ordinal,role,content,thinking_text,timestamp,has_thinking,has_tool_use,content_length,is_system,model,reasoning_effort,token_usage,context_tokens,output_tokens,provider_id,has_context_tokens,has_output_tokens,claude_message_id,claude_request_id,source_type,source_subtype,prompt_source,source_uuid,source_parent_uuid,is_sidechain,is_compact_boundary", binding: "session_id"},
	{table: "usage_events", semantic: "message_ordinal,source,model,provider_id,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,reasoning_tokens,cost_microdollars,cost_status,cost_source,occurred_at,dedup_key", binding: "session_id", excluded: "id"},
	{table: "starred_sessions", binding: "session_id", excluded: "created_at"},
	{table: "excluded_sessions", binding: "id", excluded: "created_at"},
	{table: "session_aliases", binding: "session_id,alias_id", excluded: "created_at"},
	{table: "pinned_messages", binding: "id,session_id,message_id,ordinal,source_uuid,note", excluded: "created_at"},
	{table: "source_archives", excluded: "source_archive_id,source_archive_salt"},
	{table: "source_project_identity_observations", excluded: "source_archive_id,source_archive_salt,project,machine,root_path,git_remote,git_remote_name,repository_path,worktree_name,worktree_root_path,worktree_relationship,checkout_state,git_branch,remote_resolution,remote_candidate_count,observed_at,normalized_remote,key_source,key"},
	{table: "source_project_identity_observation_scopes", excluded: "source_archive_id,project,machine,root_path,git_remote,publication_scope"},
	{table: "source_session_project_identity_snapshots", excluded: "source_archive_id,source_database_generation,source_session_id,project,machine,root_path,git_remote,git_remote_name,repository_path,worktree_name,worktree_root_path,worktree_relationship,checkout_state,git_branch,remote_resolution,remote_candidate_count,observed_at,normalized_remote,key_source,key"},
	{table: "source_session_project_identity_snapshot_scopes", excluded: "source_archive_id,source_database_generation,source_session_id,publication_scope"},
	{table: "source_worktree_project_mappings", excluded: "source_archive_id,machine,path_prefix,layout,project,original_project,enabled,updated_at"},
	{table: "source_worktree_project_mapping_scopes", excluded: "source_archive_id,machine,path_prefix,publication_scope"},
	{table: "tool_calls", semantic: "tool_name,category,call_index,tool_use_id,input_json,skill_name,result_content_length,result_content,subagent_session_id,message_ordinal,file_path", binding: "session_id", excluded: "id"},
	{table: "tool_result_events", semantic: "tool_call_message_ordinal,call_index,tool_use_id,agent_id,subagent_session_id,source,status,content,content_length,timestamp,event_index", binding: "session_id", excluded: "id"},
	{table: "secret_findings", semantic: "rule_name,confidence,location_kind,message_ordinal,call_index,event_index,match_start,match_end,match_index,redacted_match", binding: "session_id,rules_version", excluded: "id,created_at"},
	{table: "raw_objects", binding: "sha256,size_bytes", excluded: "verified_at"},
	{table: "raw_manifests", binding: "manifest_id,device_id,provider,configured_root_id,source_key,source_key_sha256,capture_id,parent_receipt,receipt,generation,kind,canonical_json", excluded: "captured_at,accepted_at"},
	{table: "raw_manifest_entries", binding: "manifest_id,entry_index,path,path_sha256,entry_type,size_bytes"},
	{table: "raw_manifest_objects", binding: "manifest_id,entry_index,object_index,sha256,size_bytes"},
	{table: "raw_source_heads", binding: "device_id,provider,configured_root_id,source_key,source_key_sha256,manifest_id,receipt,generation", excluded: "updated_at"},
	{table: "raw_source_projections", binding: "source_id,device_id,provider,configured_root_id,source_key_sha256,selected_manifest_id,processing_version,projection_generation,selected_job_id,successful_manifest_id,last_attempt_manifest_id,membership_complete,diagnostics"},
	{table: "raw_projection_generations", binding: "source_id,generation,manifest_id,processing_version"},
	{table: "raw_session_groups", binding: "group_id,provider,logical_key,base_alias"},
	{table: "raw_content_revisions", binding: "session_id,group_id,content_revision,payload,recency_state"},
	{table: "raw_session_branches", binding: "branch_id,source_id,group_id,member_id,session_id,content_revision,manifest_id,processing_version,projection_generation,active,prior_payload"},
	{table: "session_sources", binding: "branch_id,group_id,source_id,session_id,physical_session_id,manifest_id,content_revision,processing_version,projection_generation"},
	{table: "raw_source_contributions", binding: "branch_id,manifest_id,projection_generation,processing_version,prior_contributed,payload"},
	{table: "raw_session_public_aliases", binding: "alias_id,group_id,anchor_branch"},
	{table: "raw_curation", binding: "group_id,branch_id,field,value"},
	{table: "raw_pins", binding: "group_id,branch_id,message_key,ordinal,content_revision,pinned,note", excluded: "created_at"},
	{table: "raw_corpus_state", binding: "singleton,corpus_revision,identity_revision,selection_revision"},
	{table: "raw_session_links", semantic: "kind,ordinal,call_index,event_index,target_alias", binding: "branch_id"},
}

// Generated from a provisioned tenant and then fixed as a catalog contract.
// Values are column:type:nullability, sorted by column name.
var parityPhysicalCatalogSignatures = map[string]string{
	"sessions":                             "agent:text:false,agent_label:text:false,compaction_count:integer:false,consecutive_failure_max:integer:false,context_pressure_max:double precision:true,created_at:timestamp with time zone:true,cwd:text:false,data_version:integer:false,deleted_at:timestamp with time zone:true,deletion_cause:text:true,display_name:text:true,duplicate_prompt_count:integer:false,edit_churn_count:integer:false,ended_at:timestamp with time zone:true,ended_with_role:text:false,entrypoint:text:false,file_path:text:true,final_failure_streak:integer:false,first_message:text:true,git_branch:text:false,has_context_data:boolean:false,has_peak_context_tokens:boolean:false,has_tool_calls:boolean:false,has_total_output_tokens:boolean:false,health_grade:text:true,health_score:integer:true,id:text:false,is_automated:boolean:false,is_truncated:boolean:false,machine:text:false,message_count:integer:false,mid_task_compaction_count:integer:false,missing_success_criteria_count:integer:false,missing_verification_count:integer:false,no_code_context_count:integer:false,outcome:text:false,outcome_confidence:text:false,owner_marker:text:false,parent_session_id:text:true,parser_malformed_lines:integer:false,parser_parent_session_id:text:true,peak_context_tokens:integer:false,project:text:false,prompt_evidence_discarded:boolean:false,provenance_kind:text:false,quality_signal_version:integer:false,raw_content_revision:text:false,raw_group_id:text:false,relationship_type:text:false,runaway_tool_loop_count:integer:false,secret_leak_count:integer:false,secrets_rules_version:text:false,session_kind:text:false,session_name:text:true,short_prompt_count:integer:false,signals_pending_since:text:true,source_archive_id:text:false,source_database_generation:text:false,source_deleted_at:timestamp with time zone:true,source_display_name:text:true,source_session_id:text:false,source_version:text:false,started_at:timestamp with time zone:true,tenant_id:text:false,termination_status:text:true,tool_failure_signal_count:integer:false,tool_retry_count:integer:false,total_output_tokens:integer:false,transcript_fidelity:text:false,transcript_revision:text:false,unstructured_start:boolean:false,updated_at:timestamp with time zone:false,user_message_count:integer:false",
	"messages":                             "claude_message_id:text:false,claude_request_id:text:false,content:text:false,content_length:integer:false,context_tokens:integer:false,has_context_tokens:boolean:false,has_output_tokens:boolean:false,has_thinking:boolean:false,has_tool_use:boolean:false,is_compact_boundary:boolean:false,is_sidechain:boolean:false,is_system:boolean:false,model:text:false,ordinal:integer:false,output_tokens:integer:false,prompt_source:text:false,provider_id:text:false,reasoning_effort:text:false,role:text:false,session_id:text:false,source_parent_uuid:text:false,source_subtype:text:false,source_type:text:false,source_uuid:text:false,tenant_id:text:false,thinking_text:text:false,timestamp:timestamp with time zone:true,token_usage:text:false",
	"usage_events":                         "cache_creation_input_tokens:integer:false,cache_read_input_tokens:integer:false,cost_microdollars:bigint:true,cost_source:text:false,cost_status:text:false,dedup_key:text:false,id:bigint:false,input_tokens:integer:false,message_ordinal:integer:true,model:text:false,occurred_at:timestamp with time zone:true,output_tokens:integer:false,provider_id:text:false,reasoning_tokens:integer:false,session_id:text:false,source:text:false,tenant_id:text:false",
	"starred_sessions":                     "created_at:timestamp with time zone:false,session_id:text:false,tenant_id:text:false",
	"excluded_sessions":                    "created_at:timestamp with time zone:false,id:text:false,tenant_id:text:false",
	"session_aliases":                      "alias_id:text:false,created_at:timestamp with time zone:false,session_id:text:false,tenant_id:text:false",
	"pinned_messages":                      "created_at:timestamp with time zone:false,id:bigint:false,message_id:integer:false,note:text:true,ordinal:integer:false,session_id:text:false,source_uuid:text:false,tenant_id:text:false",
	"source_archives":                      "source_archive_id:text:false,source_archive_salt:text:false,tenant_id:text:false",
	"source_project_identity_observations": "checkout_state:text:false,git_branch:text:false,git_remote:text:false,git_remote_name:text:false,key:text:false,key_source:text:false,machine:text:false,normalized_remote:text:false,observed_at:timestamp with time zone:false,project:text:false,remote_candidate_count:integer:false,remote_resolution:text:false,repository_path:text:false,root_path:text:false,source_archive_id:text:false,source_archive_salt:text:false,tenant_id:text:false,worktree_name:text:false,worktree_relationship:text:false,worktree_root_path:text:false",
	"source_project_identity_observation_scopes":      "git_remote:text:false,machine:text:false,project:text:false,publication_scope:text:false,root_path:text:false,source_archive_id:text:false,tenant_id:text:false",
	"source_session_project_identity_snapshots":       "checkout_state:text:false,git_branch:text:false,git_remote:text:false,git_remote_name:text:false,key:text:false,key_source:text:false,machine:text:false,normalized_remote:text:false,observed_at:timestamp with time zone:false,project:text:false,remote_candidate_count:integer:false,remote_resolution:text:false,repository_path:text:false,root_path:text:false,source_archive_id:text:false,source_database_generation:text:false,source_session_id:text:false,tenant_id:text:false,worktree_name:text:false,worktree_relationship:text:false,worktree_root_path:text:false",
	"source_session_project_identity_snapshot_scopes": "publication_scope:text:false,source_archive_id:text:false,source_database_generation:text:false,source_session_id:text:false,tenant_id:text:false",
	"source_worktree_project_mappings":                "enabled:boolean:false,layout:text:false,machine:text:false,original_project:text:false,path_prefix:text:false,project:text:false,source_archive_id:text:false,tenant_id:text:false,updated_at:text:false",
	"source_worktree_project_mapping_scopes":          "machine:text:false,path_prefix:text:false,publication_scope:text:false,source_archive_id:text:false,tenant_id:text:false",
	"tool_calls":                                      "call_index:integer:false,category:text:false,file_path:text:true,id:bigint:false,input_json:text:true,message_ordinal:integer:false,result_content:text:true,result_content_length:integer:true,session_id:text:false,skill_name:text:true,subagent_session_id:text:true,tenant_id:text:false,tool_name:text:false,tool_use_id:text:false",
	"tool_result_events":                              "agent_id:text:true,call_index:integer:false,content:text:false,content_length:integer:false,event_index:integer:false,id:bigint:false,session_id:text:false,source:text:false,status:text:false,subagent_session_id:text:true,tenant_id:text:false,timestamp:timestamp with time zone:true,tool_call_message_ordinal:integer:false,tool_use_id:text:true",
	"secret_findings":                                 "call_index:integer:true,confidence:text:false,created_at:timestamp with time zone:false,event_index:integer:true,id:bigint:false,location_kind:text:false,match_end:integer:false,match_index:integer:false,match_start:integer:false,message_ordinal:integer:false,redacted_match:text:false,rule_name:text:false,rules_version:text:false,session_id:text:false,tenant_id:text:false",
	"raw_objects":                                     "sha256:text:false,size_bytes:bigint:false,tenant_id:text:false,verified_at:timestamp with time zone:false",
	"raw_manifests":                                   "accepted_at:timestamp with time zone:false,canonical_json:bytea:false,capture_id:text:false,captured_at:timestamp with time zone:false,configured_root_id:text:false,device_id:text:false,generation:bigint:false,kind:text:false,manifest_id:text:false,parent_receipt:text:false,provider:text:false,receipt:text:false,source_key:text:false,source_key_sha256:text:false,tenant_id:text:false",
	"raw_manifest_entries":                            "entry_index:integer:false,entry_type:text:false,manifest_id:text:false,path:text:false,path_sha256:text:false,size_bytes:bigint:false,tenant_id:text:false",
	"raw_manifest_objects":                            "entry_index:integer:false,manifest_id:text:false,object_index:integer:false,sha256:text:false,size_bytes:bigint:false,tenant_id:text:false",
	"raw_source_heads":                                "configured_root_id:text:false,device_id:text:false,generation:bigint:false,manifest_id:text:true,provider:text:false,receipt:text:true,source_key:text:false,source_key_sha256:text:false,tenant_id:text:false,updated_at:timestamp with time zone:false",
	"raw_source_projections":                          "configured_root_id:text:false,device_id:text:false,diagnostics:text:false,last_attempt_manifest_id:text:true,membership_complete:boolean:false,processing_version:text:false,projection_generation:bigint:false,provider:text:false,selected_job_id:bigint:false,selected_manifest_id:text:false,source_id:text:false,source_key_sha256:text:false,successful_manifest_id:text:true,tenant_id:text:false",
	"raw_projection_generations":                      "generation:bigint:false,manifest_id:text:false,processing_version:text:false,source_id:text:false,tenant_id:text:false",
	"raw_session_groups":                              "base_alias:text:false,group_id:text:false,logical_key:text:false,provider:text:false,tenant_id:text:false",
	"raw_content_revisions":                           "content_revision:text:false,group_id:text:false,payload:bytea:false,recency_state:jsonb:false,session_id:text:false,tenant_id:text:false",
	"raw_session_branches":                            "active:boolean:false,branch_id:text:false,content_revision:text:false,group_id:text:false,manifest_id:text:false,member_id:text:false,prior_payload:bytea:false,processing_version:text:false,projection_generation:bigint:false,session_id:text:false,source_id:text:false,tenant_id:text:false",
	"session_sources":                                 "branch_id:text:false,content_revision:text:false,group_id:text:false,manifest_id:text:false,physical_session_id:text:true,processing_version:text:false,projection_generation:bigint:false,session_id:text:false,source_id:text:false,tenant_id:text:false",
	"raw_source_contributions":                        "branch_id:text:false,manifest_id:text:false,payload:bytea:false,prior_contributed:boolean:false,processing_version:text:false,projection_generation:bigint:false,tenant_id:text:false",
	"raw_session_public_aliases":                      "alias_id:text:false,anchor_branch:text:false,group_id:text:false,tenant_id:text:false",
	"raw_curation":                                    "branch_id:text:false,field:text:false,group_id:text:false,tenant_id:text:false,value:jsonb:false",
	"raw_pins":                                        "branch_id:text:false,content_revision:text:false,created_at:timestamp with time zone:false,group_id:text:false,message_key:text:false,note:text:false,ordinal:integer:false,pinned:boolean:false,tenant_id:text:false",
	"raw_corpus_state":                                "corpus_revision:bigint:false,identity_revision:bigint:false,selection_revision:bigint:false,singleton:smallint:false,tenant_id:text:false",
	"raw_session_links":                               "branch_id:text:false,call_index:integer:false,event_index:integer:false,kind:text:false,ordinal:integer:false,target_alias:text:false,tenant_id:text:false",
}

func TestParityPhysicalCatalogCoverage(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	rows, err := f.admin.QueryContext(t.Context(), `SELECT c.relname,a.attname,format_type(a.atttypid,a.atttypmod),NOT a.attnotnull
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
		WHERE n.nspname=$1 AND c.relkind IN ('r','p') ORDER BY c.relname,a.attname`, f.schema)
	require.NoError(t, err)
	defer rows.Close()
	type physicalColumn struct {
		name, typ string
		nullable  bool
	}
	got := map[string][]physicalColumn{}
	for rows.Next() {
		var table string
		var column physicalColumn
		require.NoError(t, rows.Scan(&table, &column.name, &column.typ, &column.nullable))
		got[table] = append(got[table], column)
	}
	require.NoError(t, rows.Err())
	for _, classification := range parityPhysicalClassifications {
		expectedNames := []string{"tenant_id"}
		for _, group := range []string{classification.semantic, classification.binding, classification.excluded} {
			if group != "" {
				expectedNames = append(expectedNames, strings.Split(group, ",")...)
			}
		}
		sort.Strings(expectedNames)
		actualNames := make([]string, len(got[classification.table]))
		var signature []string
		for i, column := range got[classification.table] {
			actualNames[i] = column.name
			signature = append(signature, fmt.Sprintf("%s:%s:%t", column.name, column.typ, column.nullable))
		}
		assert.Equal(t, expectedNames, actualNames, classification.table+" classified column inventory")
		actualSignature := strings.Join(signature, ",")
		assert.Equal(t, parityPhysicalCatalogSignatures[classification.table], actualSignature, classification.table+" type/nullability inventory")
	}
}
