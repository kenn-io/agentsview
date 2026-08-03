package bunmodel

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/uptrace/bun/schema"
)

// Index is one portable ordinary index owned by the common schema.
type Index struct {
	Name        string
	Columns     []string
	Expressions []string
	Unique      bool
}

// ForeignKey is one canonical relationship. DuckDB enforces the relationship
// but cannot express cascading actions; its read-only mirror writer deletes
// dependent rows explicitly before parent rows.
type ForeignKey struct {
	Columns           []string
	ReferencedTable   string
	ReferencedColumns []string
	OnDeleteCascade   bool
}

// Table registers one canonical model and its ordinary indexes.
type Table struct {
	Name        string
	Model       any
	Indexes     []Index
	ForeignKeys []ForeignKey
}

// ForeignKeyDefinition renders the portable Bun ForeignKey clause. Callers
// omit cascading actions only for engines, currently DuckDB, that reject the
// syntax and already own explicit child-first deletion.
func ForeignKeyDefinition(foreignKey ForeignKey, includeCascade bool) string {
	definition := fmt.Sprintf(
		"%s REFERENCES %s %s",
		quotedColumns(foreignKey.Columns),
		quotedIdentifier(foreignKey.ReferencedTable),
		quotedColumns(foreignKey.ReferencedColumns),
	)
	if includeCascade && foreignKey.OnDeleteCascade {
		definition += " ON DELETE CASCADE"
	}
	return definition
}

func quotedColumns(columns []string) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quotedIdentifier(column)
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

func quotedIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

var commonTables = []Table{
	{Name: "source_archives", Model: (*SourceArchive)(nil)},
	{Name: "sessions", Model: (*Session)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"source_archive_id"}, ReferencedTable: "source_archives", ReferencedColumns: []string{"source_archive_id"}},
	}, Indexes: []Index{
		{Name: "idx_sessions_ended", Columns: []string{"ended_at", "id"}},
		{Name: "idx_sessions_project", Columns: []string{"project"}},
		{Name: "idx_sessions_machine", Columns: []string{"machine"}},
		{Name: "idx_sessions_parent", Columns: []string{"parent_session_id"}},
		{Name: "idx_sessions_started", Columns: []string{"started_at"}},
		{Name: "idx_sessions_agent", Columns: []string{"agent"}},
		{Name: "idx_sessions_termination_status", Columns: []string{"termination_status"}},
	}},
	{Name: "messages", Model: (*Message)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id"}, ReferencedTable: "sessions", ReferencedColumns: []string{"id"}, OnDeleteCascade: true},
	}, Indexes: []Index{
		{Name: "idx_messages_session_role", Columns: []string{"session_id", "role"}},
		{Name: "idx_messages_timestamp", Columns: []string{"timestamp"}},
	}},
	{Name: "usage_events", Model: (*UsageEvent)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id"}, ReferencedTable: "sessions", ReferencedColumns: []string{"id"}, OnDeleteCascade: true},
	}, Indexes: []Index{
		{Name: "idx_usage_events_session", Columns: []string{"session_id"}},
		{Name: "idx_usage_events_occurred", Columns: []string{"occurred_at"}},
		{Name: "idx_usage_events_dedup", Expressions: []string{
			"(CASE WHEN dedup_key <> '' THEN session_id END)",
			"(CASE WHEN dedup_key <> '' THEN source END)",
			"(CASE WHEN dedup_key <> '' THEN dedup_key END)",
		}, Unique: true},
	}},
	{Name: "cursor_usage_events", Model: (*CursorUsageEvent)(nil), Indexes: []Index{
		{Name: "idx_cursor_usage_events_dedup", Expressions: []string{
			"(CASE WHEN dedup_key <> '' THEN dedup_key END)",
		}, Unique: true},
		{Name: "idx_cursor_usage_events_occurred", Columns: []string{"occurred_at"}},
		{Name: "idx_cursor_usage_events_model", Columns: []string{"model"}},
	}},
	{Name: "model_pricing", Model: (*ModelPricing)(nil)},
	{Name: "model_pricing_bands", Model: (*ModelPricingBand)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"model_pattern"}, ReferencedTable: "model_pricing", ReferencedColumns: []string{"model_pattern"}, OnDeleteCascade: true},
	}},
	{Name: "source_project_identity_observations", Model: (*SourceProjectIdentityObservation)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"source_archive_id"}, ReferencedTable: "source_archives", ReferencedColumns: []string{"source_archive_id"}, OnDeleteCascade: true},
	}, Indexes: []Index{
		{Name: "idx_source_project_identity_observations_project", Columns: []string{"project"}},
	}},
	{Name: "source_session_project_identity_snapshots", Model: (*SourceSessionProjectIdentitySnapshot)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"source_archive_id"}, ReferencedTable: "source_archives", ReferencedColumns: []string{"source_archive_id"}, OnDeleteCascade: true},
	}, Indexes: []Index{
		{Name: "idx_source_session_project_identity_snapshots_project", Columns: []string{"source_archive_id", "project"}},
	}},
	{Name: "source_worktree_project_mappings", Model: (*SourceWorktreeProjectMapping)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"source_archive_id"}, ReferencedTable: "source_archives", ReferencedColumns: []string{"source_archive_id"}, OnDeleteCascade: true},
	}},
	{Name: "tool_calls", Model: (*ToolCall)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id", "message_ordinal"}, ReferencedTable: "messages", ReferencedColumns: []string{"session_id", "ordinal"}, OnDeleteCascade: true},
	}, Indexes: []Index{
		{Name: "idx_tool_calls_dedup", Columns: []string{"session_id", "message_ordinal", "call_index"}, Unique: true},
		{Name: "idx_tool_calls_session", Columns: []string{"session_id"}},
		{Name: "idx_tool_calls_category", Columns: []string{"category"}},
		{Name: "idx_tool_calls_file_path", Columns: []string{"file_path"}},
	}},
	{Name: "tool_result_events", Model: (*ToolResultEvent)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id", "tool_call_message_ordinal", "call_index"}, ReferencedTable: "tool_calls", ReferencedColumns: []string{"session_id", "message_ordinal", "call_index"}, OnDeleteCascade: true},
	}, Indexes: []Index{
		{Name: "idx_tool_result_events_session", Columns: []string{"session_id"}},
		{Name: "idx_tool_result_events_dedup", Columns: []string{"session_id", "tool_call_message_ordinal", "call_index", "event_index"}, Unique: true},
	}},
	{Name: "secret_findings", Model: (*SecretFinding)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id"}, ReferencedTable: "sessions", ReferencedColumns: []string{"id"}, OnDeleteCascade: true},
	}, Indexes: []Index{
		{Name: "idx_secret_findings_session", Columns: []string{"session_id"}},
		{Name: "idx_secret_findings_rule", Columns: []string{"rule_name"}},
	}},
	{Name: "starred_sessions", Model: (*StarredSession)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id"}, ReferencedTable: "sessions", ReferencedColumns: []string{"id"}, OnDeleteCascade: true},
	}},
	{Name: "excluded_sessions", Model: (*ExcludedSession)(nil)},
	{Name: "session_aliases", Model: (*SessionAlias)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id"}, ReferencedTable: "sessions", ReferencedColumns: []string{"id"}, OnDeleteCascade: true},
	}},
	{Name: "pinned_messages", Model: (*PinnedMessage)(nil), ForeignKeys: []ForeignKey{
		{Columns: []string{"session_id", "ordinal"}, ReferencedTable: "messages", ReferencedColumns: []string{"session_id", "ordinal"}, OnDeleteCascade: true},
	}, Indexes: []Index{
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
			tables[i].Indexes[j].Expressions = slices.Clone(tables[i].Indexes[j].Expressions)
		}
		tables[i].ForeignKeys = slices.Clone(commonTables[i].ForeignKeys)
		for j := range tables[i].ForeignKeys {
			tables[i].ForeignKeys[j].Columns = slices.Clone(tables[i].ForeignKeys[j].Columns)
			tables[i].ForeignKeys[j].ReferencedColumns = slices.Clone(tables[i].ForeignKeys[j].ReferencedColumns)
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
