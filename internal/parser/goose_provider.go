package parser

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// GooseDBName is the SQLite filename inside Goose's sessions directory.
const GooseDBName = "sessions.db"

func newGooseProviderFactory(def AgentDef) ProviderFactory {
	return dbBackedProviderFactory{
		def:            cloneAgentDef(def),
		spec:           gooseProviderSpec,
		normalizeRoots: normalizeGooseRoots,
		tracker: &sqliteChangeTracker{
			agent:  AgentGoose,
			open:   openGooseDB,
			schema: gooseCursorSchema,
		},
	}
}

func gooseProviderCapabilities() Capabilities {
	source := dbBackedSourceCapabilities(CapabilityNotApplicable)
	// Watcher events use the bounded producer-row cursor below. Stored source
	// hints would enumerate every archived virtual member for each WAL event.
	source.StoredSourceHints = CapabilityUnsupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}

func gooseProviderSpec(stableSnapshot bool) dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentGoose,
		dbName: GooseDBName,
		findDB: gooseDBPath,
		streamMeta: func(
			ctx context.Context,
			dbPath string,
			yield func(dbBackedSessionMeta) error,
		) error {
			return forEachGooseSessionMeta(ctx, dbPath, stableSnapshot, yield)
		},
		metaForID: func(
			ctx context.Context, dbPath, sessionID string,
		) (dbBackedSessionMeta, bool, error) {
			return gooseSessionMeta(ctx, dbPath, sessionID, stableSnapshot)
		},
		parse: func(
			ctx context.Context, dbPath, sessionID, machine string,
		) ([]ParseResult, error) {
			result, err := parseGooseSession(ctx, dbPath, sessionID, machine, stableSnapshot)
			if err != nil || result == nil {
				return nil, err
			}
			return []ParseResult{*result}, nil
		},
		fingerprintHash: func(ctx context.Context, dbPath, sessionID string) (string, bool, error) {
			return gooseSessionFingerprint(ctx, dbPath, sessionID, stableSnapshot)
		},
		caps: gooseProviderCapabilities(),
	}
}

func normalizeGooseRoots(roots []string) []string {
	cleaned := cleanJSONLRoots(roots)
	out := make([]string, 0, len(cleaned))
	seen := make(map[string]struct{}, len(cleaned))
	for _, root := range cleaned {
		normalized := normalizeGooseRoot(root)
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

// ResolveGoosePathRoot expands GOOSE_PATH_ROOT using Goose's producer-defined
// data layout. Unlike goose_dirs, the environment variable is never a direct
// sessions directory, even when its basename is "data" or "sessions".
func ResolveGoosePathRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	return filepath.Join(root, "data", "sessions")
}

func normalizeGooseRoot(root string) string {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return ""
	}
	if filepath.Base(root) == GooseDBName {
		return filepath.Dir(root)
	}
	candidates := []string{
		root,
		filepath.Join(root, "sessions"),
		filepath.Join(root, "data", "sessions"),
	}
	for _, candidate := range candidates {
		if IsRegularFile(filepath.Join(candidate, GooseDBName)) {
			return candidate
		}
	}
	switch filepath.Base(root) {
	case "sessions":
		return root
	case "data":
		return filepath.Join(root, "sessions")
	default:
		// A goose_dirs entry may point at the path root when no more specific
		// existing path or conventional basename identifies its shape.
		return filepath.Join(root, "data", "sessions")
	}
}

func gooseDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, GooseDBName)
	if !IsRegularFile(path) {
		return ""
	}
	return path
}

// GooseSQLiteVirtualPath identifies one Goose session inside sessions.db.
func GooseSQLiteVirtualPath(dbPath, sessionID string) string {
	return VirtualSourcePath(dbPath, sessionID)
}

func openGooseDB(dbPath string, stableSnapshot bool) (*sql.DB, error) {
	immutable := "0"
	if stableSnapshot {
		immutable = "1"
	}
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=" + immutable + "&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening goose sessions database %s: %w", dbPath, err)
	}
	return db, nil
}

func gooseCursorSchema(
	ctx context.Context, db *sql.DB,
) (int, []sqliteCursorTable, error) {
	version, err := gooseSchemaVersion(ctx, db)
	if err != nil {
		return 0, nil, err
	}
	hasUsage, err := gooseTableExists(ctx, db, "usage_ledger")
	if err != nil {
		return 0, nil, err
	}
	tables := []sqliteCursorTable{
		{
			name: "sessions", rowID: "rowid", sessionID: "id",
			identity: "CAST(id AS TEXT)",
		},
		{
			name: "messages", rowID: "id", sessionID: "session_id",
			identity: "session_id || char(31) || COALESCE(message_id, '') || char(31) || CAST(created_timestamp AS TEXT)",
		},
	}
	if hasUsage {
		tables = append(tables, sqliteCursorTable{
			name: "usage_ledger", rowID: "id", sessionID: "session_id",
			identity: "session_id || char(31) || COALESCE(model, '') || char(31) || CAST(created_timestamp AS TEXT)",
		})
	}
	return version, tables, nil
}
