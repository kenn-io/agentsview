package db

// DialogueEligibilitySQL requires supported native provenance and actual
// dialogue. Persisted layouts have already passed the body writer's validation.
// Null layouts retain their original transcript but cannot prove dialogue.
func DialogueEligibilitySQL(alias string, dialect QueryDialect) string {
	layout := alias + ".content_layout"
	var version string
	switch dialect.name {
	case "postgres":
		version = "(" + layout + "::jsonb ->> 'version') = '1'"
	case "duckdb":
		version = "json_extract_string(" + layout + ", '$.version') = '1'"
	case "clickhouse":
		version = "JSONExtractInt(ifNull(" + layout + ", ''), 'version') = 1"
	default:
		version = "CASE WHEN json_valid(" + layout + ") THEN json_extract(" + layout + ", '$.version') END = 1"
	}
	return "(" + version + " AND " + alias + ".content <> '')"
}

// NormalizeContentSearchSources keeps the public token/vector modes restricted
// to dialogue while substring and regex can select canonical work fields.
func NormalizeContentSearchSources(f ContentSearchFilter) ([]string, error) {
	if f.Mode == "fts" || f.Mode == "terms" || f.Mode == "semantic" || f.Mode == "hybrid" {
		for _, source := range f.Sources {
			if source != "messages" {
				return nil, searchInputErrorf("search: %s searches messages only (got source %q)", f.Mode, source)
			}
		}
		return []string{"messages"}, nil
	}
	if len(f.Sources) == 0 {
		return []string{"messages", "thinking", "tool_input", "tool_result"}, nil
	}
	for _, source := range f.Sources {
		if source != "messages" && source != "thinking" && source != "tool_input" && source != "tool_result" {
			return nil, searchInputErrorf("search: unknown source %q", source)
		}
	}
	return f.Sources, nil
}
