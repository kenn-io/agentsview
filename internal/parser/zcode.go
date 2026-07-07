package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ZCodeDBName = "db.sqlite"

const zcodeDBName = ZCodeDBName

type zcodeProviderFactory struct {
	def AgentDef
}

func newZcodeProviderFactory(def AgentDef) ProviderFactory {
	return zcodeProviderFactory{def: cloneAgentDef(def)}
}

func (f zcodeProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f zcodeProviderFactory) Capabilities() Capabilities {
	return zcodeProviderCapabilities()
}

func (f zcodeProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	cfg.Roots = normalizeZCodeRoots(cfg.Roots)
	spec := zcodeProviderSpec()
	return &dbBackedProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   spec.caps,
			Config: cfg,
		},
		spec:    spec,
		sources: newDBBackedSourceSet(spec, cfg.Roots),
	}
}

func zcodeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: dbBackedSourceCapabilities(CapabilityNotApplicable),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
		},
	}
}

func zcodeProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentZCode,
		dbName: zcodeDBName,
		findDB: zcodeDBPath,
		listMeta: func(dbPath string) ([]dbBackedSessionMeta, error) {
			return listZCodeSessionMeta(dbPath)
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			result, err := parseZCodeSession(dbPath, sessionID, machine)
			if err != nil || result == nil {
				return nil, err
			}
			return []ParseResult{*result}, nil
		},
		caps: zcodeProviderCapabilities(),
	}
}

func normalizeZCodeRoots(roots []string) []string {
	cleaned := cleanJSONLRoots(roots)
	out := make([]string, 0, len(cleaned))
	seen := make(map[string]struct{}, len(cleaned))
	for _, root := range cleaned {
		normalized := normalizeZCodeRoot(root)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func normalizeZCodeRoot(root string) string {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return ""
	}
	if filepath.Base(root) == "db" {
		return root
	}
	return filepath.Join(root, "db")
}

func zcodeDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, zcodeDBName)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

func zcodeVirtualPathParts(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, zcodeDBName)
}

func ZCodeSQLiteVirtualPath(dbPath, sessionID string) string {
	return VirtualSourcePath(dbPath, sessionID)
}

func ZCodeSQLiteSourceMtime(path string) (int64, error) {
	dbPath, sessionID, ok := zcodeVirtualPathParts(path)
	if !ok {
		return 0, fmt.Errorf("not a zcode sqlite virtual path: %s", path)
	}
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	row, err := loadZCodeSessionRow(db, sessionID)
	if err != nil {
		return 0, err
	}
	return zcodeSessionFileMtime(dbPath, db, row), nil
}

func listZCodeSessionMeta(dbPath string) ([]dbBackedSessionMeta, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT id,
		       project_id,
		       workspace_id,
		       COALESCE(directory, ''),
		       COALESCE(title, ''),
		       COALESCE(time_created, ''),
		       COALESCE(time_updated, '')
		  FROM session
		 ORDER BY COALESCE(time_updated, time_created), id
	`)
	if err != nil {
		return nil, fmt.Errorf("listing zcode sessions: %w", err)
	}
	defer rows.Close()

	var metas []dbBackedSessionMeta
	for rows.Next() {
		row, err := scanZCodeSessionRow(rows)
		if err != nil {
			return nil, err
		}
		if row.id == "" {
			continue
		}
		metas = append(metas, dbBackedSessionMeta{
			SessionID:   row.id,
			VirtualPath: ZCodeSQLiteVirtualPath(dbPath, row.id),
			FileMtime:   zcodeSessionFileMtime(dbPath, db, row),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].SessionID < metas[j].SessionID
	})
	return metas, nil
}

func parseZCodeSession(dbPath, sessionID, machine string) (*ParseResult, error) {
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	row, err := loadZCodeSessionRow(db, sessionID)
	if err != nil {
		return nil, err
	}
	result, err := buildZCodeParseResult(dbPath, machine, row, db)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func openZCodeDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=0&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening zcode db %s: %w", dbPath, err)
	}
	return db, nil
}

type zcodeSessionRow struct {
	id          string
	projectID   sql.NullString
	workspaceID sql.NullString
	directory   sql.NullString
	title       sql.NullString
	timeCreated sql.NullString
	timeUpdated sql.NullString
}

type zcodeUsageRow struct {
	sessionID                string
	turnID                   sql.NullString
	providerID               sql.NullString
	modelID                  sql.NullString
	status                   sql.NullString
	inputTokens              sql.NullInt64
	outputTokens             sql.NullInt64
	reasoningTokens          sql.NullInt64
	cacheCreationInputTokens sql.NullInt64
	cacheReadInputTokens     sql.NullInt64
	computedTotalTokens      sql.NullInt64
	startedAt                sql.NullString
	completedAt              sql.NullString
	durationMS               sql.NullInt64
	toolCallCount            sql.NullInt64
}

func loadZCodeSessionRow(db *sql.DB, sessionID string) (zcodeSessionRow, error) {
	row := db.QueryRow(`
		SELECT id,
		       project_id,
		       workspace_id,
		       COALESCE(directory, ''),
		       COALESCE(title, ''),
		       COALESCE(time_created, ''),
		       COALESCE(time_updated, '')
		  FROM session
		 WHERE id = ?
	`, sessionID)

	var out zcodeSessionRow
	err := row.Scan(
		&out.id, &out.projectID, &out.workspaceID,
		&out.directory, &out.title, &out.timeCreated, &out.timeUpdated,
	)
	if err != nil {
		return zcodeSessionRow{}, fmt.Errorf("loading zcode session %s: %w", sessionID, err)
	}
	return out, nil
}

func scanZCodeSessionRow(rows *sql.Rows) (zcodeSessionRow, error) {
	var out zcodeSessionRow
	if err := rows.Scan(
		&out.id, &out.projectID, &out.workspaceID,
		&out.directory, &out.title, &out.timeCreated, &out.timeUpdated,
	); err != nil {
		return zcodeSessionRow{}, fmt.Errorf("scanning zcode session meta: %w", err)
	}
	return out, nil
}

func buildZCodeParseResult(
	dbPath, machine string,
	row zcodeSessionRow,
	db *sql.DB,
) (ParseResult, error) {
	startedAt := zcodeParseTime(row.timeCreated.String)
	endedAt := zcodeParseTime(row.timeUpdated.String)
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	directory := row.directory.String
	project := ExtractProjectFromCwd(directory)
	if project == "" {
		switch {
		case row.projectID.Valid:
			project = "project-" + strings.TrimSpace(row.projectID.String)
		case row.workspaceID.Valid:
			project = "workspace-" + strings.TrimSpace(row.workspaceID.String)
		default:
			project = "unknown"
		}
	}

	title := strings.TrimSpace(row.title.String)
	firstMessage := title
	if firstMessage != "" {
		firstMessage = truncate(strings.ReplaceAll(firstMessage, "\n", " "), 300)
	}

	sess := ParsedSession{
		ID:           "zcode:" + row.id,
		Project:      project,
		Machine:      machine,
		Agent:        AgentZCode,
		Cwd:          directory,
		FirstMessage: firstMessage,
		SessionName:  title,
		StartedAt:    startedAt,
		EndedAt:      endedAt,
		MessageCount: 0,
		File: FileInfo{
			Path: ZCodeSQLiteVirtualPath(dbPath, row.id),
		},
	}
	if info, err := os.Stat(dbPath); err == nil {
		sess.File.Size = info.Size()
	}
	sess.File.Mtime = zcodeSessionFileMtime(dbPath, db, row)

	usageEvents, err := listZCodeUsageEvents(db, row.id, startedAt, endedAt)
	if err != nil {
		return ParseResult{}, err
	}
	applyUsageEventTokenTotals(&sess, usageEvents)

	return ParseResult{
		Session:     sess,
		UsageEvents: usageEvents,
	}, nil
}

func listZCodeUsageEvents(
	db *sql.DB,
	sessionID string,
	startedAt, endedAt time.Time,
) ([]ParsedUsageEvent, error) {
	rows, err := db.Query(`
		SELECT session_id,
		       CAST(turn_id AS TEXT),
		       provider_id,
		       COALESCE(model_id, ''),
		       COALESCE(status, ''),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(reasoning_tokens, 0),
		       COALESCE(cache_creation_input_tokens, 0),
		       COALESCE(cache_read_input_tokens, 0),
		       COALESCE(computed_total_tokens, 0),
		       COALESCE(started_at, ''),
		       COALESCE(completed_at, ''),
		       COALESCE(duration_ms, 0),
		       COALESCE(tool_call_count, 0)
		  FROM model_usage
		 WHERE session_id = ?
		 ORDER BY
		       COALESCE(turn_id, -1),
		       COALESCE(started_at, ''),
		       COALESCE(completed_at, ''),
		       COALESCE(provider_id, -1),
		       COALESCE(model_id, ''),
		       COALESCE(status, ''),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(reasoning_tokens, 0),
		       COALESCE(cache_creation_input_tokens, 0),
		       COALESCE(cache_read_input_tokens, 0),
		       COALESCE(computed_total_tokens, 0),
		       COALESCE(duration_ms, 0),
		       COALESCE(tool_call_count, 0)
	`, sessionID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: model_usage") {
			return nil, nil
		}
		return nil, fmt.Errorf("listing zcode usage rows for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var events []ParsedUsageEvent
	for rows.Next() {
		row, err := scanZCodeUsageRow(rows)
		if err != nil {
			return nil, err
		}
		if row.sessionID == "" {
			continue
		}
		fullSessionID := "zcode:" + row.sessionID
		ev := ParsedUsageEvent{
			SessionID:                fullSessionID,
			Source:                   "session",
			Model:                    row.modelID.String,
			InputTokens:              int(row.inputTokens.Int64),
			OutputTokens:             int(row.outputTokens.Int64),
			CacheCreationInputTokens: int(row.cacheCreationInputTokens.Int64),
			CacheReadInputTokens:     int(row.cacheReadInputTokens.Int64),
			ReasoningTokens:          int(row.reasoningTokens.Int64),
			OccurredAt:               timeString(zcodeUsageTime(row.completedAt.String), zcodeUsageTime(row.startedAt.String)),
			DedupKey:                 zcodeUsageDedupKey(fullSessionID, row),
		}
		if row.turnID.Valid {
			if parsed, err := strconv.Atoi(strings.TrimSpace(row.turnID.String)); err == nil {
				ev.MessageOrdinal = &parsed
			}
		}
		if ev.OccurredAt == "" {
			ev.OccurredAt = timeString(endedAt, startedAt)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func scanZCodeUsageRow(rows *sql.Rows) (zcodeUsageRow, error) {
	var out zcodeUsageRow
	if err := rows.Scan(
		&out.sessionID, &out.turnID, &out.providerID,
		&out.modelID, &out.status, &out.inputTokens, &out.outputTokens,
		&out.reasoningTokens, &out.cacheCreationInputTokens,
		&out.cacheReadInputTokens, &out.computedTotalTokens,
		&out.startedAt, &out.completedAt, &out.durationMS,
		&out.toolCallCount,
	); err != nil {
		return zcodeUsageRow{}, fmt.Errorf("scanning zcode usage row: %w", err)
	}
	return out, nil
}

func zcodeUsageDedupKey(fullSessionID string, row zcodeUsageRow) string {
	parts := []string{
		"session:" + fullSessionID,
		"turn=" + row.turnID.String,
		"provider=" + row.providerID.String,
		"model=" + row.modelID.String,
		"status=" + row.status.String,
		"started_at=" + row.startedAt.String,
		"completed_at=" + row.completedAt.String,
		"duration_ms=" + zcodeNullInt64String(row.durationMS),
		"tool_call_count=" + zcodeNullInt64String(row.toolCallCount),
		"input_tokens=" + zcodeNullInt64String(row.inputTokens),
		"output_tokens=" + zcodeNullInt64String(row.outputTokens),
		"reasoning_tokens=" + zcodeNullInt64String(row.reasoningTokens),
		"cache_creation_input_tokens=" + zcodeNullInt64String(row.cacheCreationInputTokens),
		"cache_read_input_tokens=" + zcodeNullInt64String(row.cacheReadInputTokens),
		"computed_total_tokens=" + zcodeNullInt64String(row.computedTotalTokens),
	}
	return strings.Join(parts, "|")
}

func zcodeNullInt64String(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

func zcodeParseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts := parseTimestamp(raw); !ts.IsZero() {
		return ts
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch {
		case n >= 1_000_000_000_000_000_000:
			return time.Unix(0, n).UTC()
		case n >= 1_000_000_000_000:
			return time.UnixMilli(n).UTC()
		case n >= 1_000_000_000:
			return time.Unix(n, 0).UTC()
		default:
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}

func zcodeUsageTime(raw string) time.Time {
	return zcodeParseTime(raw)
}

func zcodeSessionFileMtime(dbPath string, db *sql.DB, row zcodeSessionRow) int64 {
	maxMtime := zcodeTimeUnixNano(zcodeRowUpdatedAt(row))
	if usageMtime, err := zcodeMaxUsageMtime(db, row.id); err == nil {
		maxMtime = max(maxMtime, usageMtime)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if info, err := os.Stat(path); err == nil {
			maxMtime = max(maxMtime, info.ModTime().UnixNano())
		}
	}
	return maxMtime
}

func zcodeMaxUsageMtime(db *sql.DB, sessionID string) (int64, error) {
	rows, err := db.Query(`
		SELECT CAST(COALESCE(completed_at, '') AS TEXT),
		       CAST(COALESCE(started_at, '') AS TEXT)
		  FROM model_usage
		 WHERE session_id = ?
	`, sessionID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table: model_usage") {
			return 0, nil
		}
		return 0, fmt.Errorf("listing zcode usage mtimes: %w", err)
	}
	defer rows.Close()

	var maxMtime int64
	for rows.Next() {
		var completedAt, startedAt string
		if err := rows.Scan(&completedAt, &startedAt); err != nil {
			return 0, fmt.Errorf("scanning zcode usage mtime: %w", err)
		}
		maxMtime = max(maxMtime, zcodeTimeUnixNano(zcodeUsageTime(completedAt)))
		maxMtime = max(maxMtime, zcodeTimeUnixNano(zcodeUsageTime(startedAt)))
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return maxMtime, nil
}

func zcodeTimeUnixNano(ts time.Time) int64 {
	if ts.IsZero() {
		return 0
	}
	return ts.UnixNano()
}

func zcodeRowUpdatedAt(row zcodeSessionRow) time.Time {
	if ts := zcodeParseTime(row.timeUpdated.String); !ts.IsZero() {
		return ts
	}
	if ts := zcodeParseTime(row.timeCreated.String); !ts.IsZero() {
		return ts
	}
	return time.Time{}
}
