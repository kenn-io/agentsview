package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// CodexAncestry supplies candidates for an edge from an already verified page.
type CodexAncestry func(context.Context, string, string) ([]string, error)

type acceptedRow struct {
	root, manifestID, configuredRoot, originalRoot, originalPath, sourceKey, boundRoot, boundKey string
	canonical                                                                                    []byte
}

func acceptedSource(identity rawsync.AuthIdentity, row acceptedRow, provider, nativeID string) (rawsync.CanonicalManifest, rawsync.Entry, string, error) {
	manifest, err := rawsync.ParseCanonicalManifest(identity, row.manifestID, row.canonical, rawsync.DefaultManifestLimits())
	if err != nil {
		return rawsync.CanonicalManifest{}, rawsync.Entry{}, "", err
	}
	if manifest.Manifest.ConfiguredRootID != row.configuredRoot || manifest.Manifest.SourceKey != row.sourceKey || string(manifest.Manifest.Provider) != provider {
		return rawsync.CanonicalManifest{}, rawsync.Entry{}, "", errors.New("snapshot source mismatch")
	}
	bound, primary, err := snapshotPrimaryPath(row.sourceKey, row.originalPath, row.originalRoot, nativeID, manifest.Manifest.Entries)
	return manifest, bound, primary, err
}

func acceptedCodexSources(ctx context.Context, conn *sql.DB, identity rawsync.AuthIdentity, key string) (_ []acceptedRow, retErr error) {
	sourceKey := parser.CodexSourceKey(parser.AgentCodex, key)
	rows, err := conn.QueryContext(ctx, `SELECT s.root_id,s.manifest_id,r.configured_root_id,r.original_path,s.original_path,s.canonical_json,COALESCE(b.root_id,''),COALESCE(b.source_key,'') FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id LEFT JOIN raw_archive_sessions b ON b.device_id=r.device_id AND b.provider=r.provider AND b.parser_id=? WHERE r.device_id=? AND r.provider=? AND s.source_key=? ORDER BY s.manifest_id`, "codex:"+key, identity.DeviceID, "codex", sourceKey)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var result []acceptedRow
	for rows.Next() {
		row := acceptedRow{sourceKey: sourceKey}
		if err := rows.Scan(&row.root, &row.manifestID, &row.configuredRoot, &row.originalRoot, &row.originalPath, &row.canonical, &row.boundRoot, &row.boundKey); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func pickAncestorSource(identity rawsync.AuthIdentity, parent rawsync.Entry, key string, rows []acceptedRow) (rawsync.CanonicalManifest, bool, error) {
	var selected rawsync.CanonicalManifest
	found := false
	for _, row := range rows {
		bound := row.boundRoot != "" || row.boundKey != ""
		if bound && (row.root != row.boundRoot || row.sourceKey != row.boundKey) {
			continue
		}
		manifest, primary, _, err := acceptedSource(identity, row, "codex", "codex:"+key)
		if err != nil {
			return rawsync.CanonicalManifest{}, false, err
		}
		if primary.Length != parent.Length || !slices.Equal(primary.Objects, parent.Objects) {
			if bound {
				return rawsync.CanonicalManifest{}, false, errors.New("saved transcript does not match snapshot")
			}
			continue
		}
		if found {
			return rawsync.CanonicalManifest{}, false, fmt.Errorf("ambiguous Codex rollout %s", strings.TrimPrefix(key, parser.CodexThreadIDFromSessionKey(key)+"_"))
		}
		selected, found = manifest, true
	}
	return selected, found, nil
}

func codexAncestorEntries(manifest rawsync.CanonicalManifest, rollout string, byPath map[string]rawsync.Entry) ([]string, error) {
	var wanted []rawsync.Entry
	for _, entry := range manifest.Manifest.Entries {
		key := parser.CodexSessionUUIDFromFilename(path.Base(entry.Path))
		if key != "" && strings.TrimPrefix(key, parser.CodexThreadIDFromSessionKey(key)+"_") == rollout {
			wanted = append(wanted, entry)
		}
	}
	scratch := make(map[string]rawsync.Entry)
	names := codexCompanions(wanted, nil, scratch)
	for i, name := range names {
		token := "\x00" + manifest.ManifestID + "/" + name
		if _, taken := byPath[token]; taken {
			return nil, fmt.Errorf("ambiguous Codex rollout %s", rollout)
		}
		byPath[token], names[i] = scratch[name], token
	}
	return names, nil
}

func codexAncestry(conn *sql.DB, identity rawsync.AuthIdentity, manifest rawsync.CanonicalManifest, byPath map[string]rawsync.Entry) CodexAncestry {
	reached := slices.Clone(manifest.Manifest.Entries)
	return func(ctx context.Context, from, rollout string) ([]string, error) {
		parent, ok := byPath[from]
		key := parser.CodexSessionUUIDFromFilename(path.Base(parent.Path))
		if !ok || key == "" || !slices.ContainsFunc(reached, func(entry rawsync.Entry) bool {
			return parser.CodexSessionUUIDFromFilename(path.Base(entry.Path)) == key && entry.Length == parent.Length && slices.Equal(entry.Objects, parent.Objects)
		}) {
			return nil, nil
		}
		rows, err := acceptedCodexSources(ctx, conn, identity, key)
		if err != nil {
			return nil, err
		}
		source, found, err := pickAncestorSource(identity, parent, key, rows)
		if err != nil || !found {
			return nil, err
		}
		names, err := codexAncestorEntries(source, rollout, byPath)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			reached = append(reached, byPath[name])
		}
		return names, nil
	}
}
