package bunmodel

import (
	"reflect"
	"slices"

	"github.com/uptrace/bun/schema"
)

// Index is one portable ordinary index owned by the common schema.
type Index struct {
	Name    string
	Columns []string
	Unique  bool
}

// Table registers one canonical model and its ordinary indexes.
type Table struct {
	Name    string
	Model   any
	Indexes []Index
}

var commonTables = []Table{
	{Name: "source_archives", Model: (*SourceArchive)(nil)},
	{Name: "sessions", Model: (*Session)(nil), Indexes: []Index{
		{Name: "idx_sessions_ended", Columns: []string{"ended_at", "id"}},
		{Name: "idx_sessions_project", Columns: []string{"project"}},
		{Name: "idx_sessions_machine", Columns: []string{"machine"}},
		{Name: "idx_sessions_parent", Columns: []string{"parent_session_id"}},
		{Name: "idx_sessions_started", Columns: []string{"started_at"}},
		{Name: "idx_sessions_agent", Columns: []string{"agent"}},
		{Name: "idx_sessions_termination_status", Columns: []string{"termination_status"}},
	}},
	{Name: "messages", Model: (*Message)(nil), Indexes: []Index{
		{Name: "idx_messages_session_role", Columns: []string{"session_id", "role"}},
		{Name: "idx_messages_timestamp", Columns: []string{"timestamp"}},
	}},
	{Name: "usage_events", Model: (*UsageEvent)(nil), Indexes: []Index{
		{Name: "idx_usage_events_session", Columns: []string{"session_id"}},
		{Name: "idx_usage_events_occurred", Columns: []string{"occurred_at"}},
	}},
	{Name: "cursor_usage_events", Model: (*CursorUsageEvent)(nil), Indexes: []Index{
		{Name: "idx_cursor_usage_events_occurred", Columns: []string{"occurred_at"}},
		{Name: "idx_cursor_usage_events_model", Columns: []string{"model"}},
	}},
	{Name: "model_pricing", Model: (*ModelPricing)(nil)},
	{Name: "model_pricing_bands", Model: (*ModelPricingBand)(nil)},
	{Name: "source_project_identity_observations", Model: (*SourceProjectIdentityObservation)(nil), Indexes: []Index{
		{Name: "idx_source_project_identity_observations_project", Columns: []string{"project"}},
	}},
	{Name: "source_session_project_identity_snapshots", Model: (*SourceSessionProjectIdentitySnapshot)(nil), Indexes: []Index{
		{Name: "idx_source_session_project_identity_snapshots_project", Columns: []string{"source_archive_id", "project"}},
	}},
	{Name: "source_worktree_project_mappings", Model: (*SourceWorktreeProjectMapping)(nil)},
	{Name: "tool_calls", Model: (*ToolCall)(nil), Indexes: []Index{
		{Name: "idx_tool_calls_dedup", Columns: []string{"session_id", "message_ordinal", "call_index"}, Unique: true},
		{Name: "idx_tool_calls_session", Columns: []string{"session_id"}},
		{Name: "idx_tool_calls_category", Columns: []string{"category"}},
		{Name: "idx_tool_calls_file_path", Columns: []string{"file_path"}},
	}},
	{Name: "tool_result_events", Model: (*ToolResultEvent)(nil), Indexes: []Index{
		{Name: "idx_tool_result_events_session", Columns: []string{"session_id"}},
		{Name: "idx_tool_result_events_dedup", Columns: []string{"session_id", "tool_call_message_ordinal", "call_index", "event_index"}, Unique: true},
	}},
	{Name: "secret_findings", Model: (*SecretFinding)(nil), Indexes: []Index{
		{Name: "idx_secret_findings_session", Columns: []string{"session_id"}},
		{Name: "idx_secret_findings_rule", Columns: []string{"rule_name"}},
	}},
	{Name: "starred_sessions", Model: (*StarredSession)(nil)},
	{Name: "excluded_sessions", Model: (*ExcludedSession)(nil)},
	{Name: "session_aliases", Model: (*SessionAlias)(nil)},
	{Name: "pinned_messages", Model: (*PinnedMessage)(nil), Indexes: []Index{
		{Name: "idx_pinned_session", Columns: []string{"session_id"}},
		{Name: "idx_pinned_ordinal", Columns: []string{"session_id", "ordinal"}, Unique: true},
		{Name: "idx_pinned_created", Columns: []string{"created_at"}},
	}},
	{Name: "insights", Model: (*Insight)(nil), Indexes: []Index{
		{Name: "idx_insights_lookup", Columns: []string{"type", "date_from", "date_to", "project"}},
		{Name: "idx_insights_cache", Columns: []string{"cache_key", "created_at"}},
	}},
}

var modelRegistry = schema.NewTables(schema.NewNopQueryGen().Dialect())

// CommonTables returns a copy so adapters cannot mutate the canonical
// registry or one another's index definitions.
func CommonTables() []Table {
	tables := make([]Table, len(commonTables))
	for i := range commonTables {
		tables[i] = commonTables[i]
		tables[i].Indexes = slices.Clone(commonTables[i].Indexes)
		for j := range tables[i].Indexes {
			tables[i].Indexes[j].Columns = slices.Clone(tables[i].Indexes[j].Columns)
		}
	}
	return tables
}

// ModelColumns returns sorted Bun column names for a canonical model.
func ModelColumns(model any) []string {
	table := modelRegistry.Get(reflect.TypeOf(model))
	columns := make([]string, 0, len(table.Fields))
	for _, field := range table.Fields {
		columns = append(columns, field.Name)
	}
	slices.Sort(columns)
	return columns
}
