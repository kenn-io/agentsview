package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const legacyTraeTargetMapTable = "temp._legacy_trae_target_map"

func legacyTraeRawSessionID(oldID, sourceSessionID string) string {
	rawID := strings.TrimSpace(sourceSessionID)
	if rawID != "" {
		rawID = strings.TrimPrefix(rawID, "trae:")
		rawID = strings.TrimPrefix(rawID, "trae:")
		if rawID != "" {
			return rawID
		}
	}
	_, strippedID := legacyTraeSessionPrefixAndStrippedID(oldID)
	rawID = strings.TrimPrefix(strippedID, "trae:")
	rawID = strings.TrimPrefix(rawID, "trae:")
	return strings.TrimSpace(rawID)
}

func legacyTraeSiblingSessionIDs(
	oldID, sourceSessionID string,
) (workspaceID, globalID string) {
	rawID := legacyTraeRawSessionID(oldID, sourceSessionID)
	if rawID == "" {
		return "", ""
	}
	hostPrefix, _ := legacyTraeSessionPrefixAndStrippedID(oldID)
	return hostPrefix + "trae:workspaceStorage:" + rawID,
		hostPrefix + "trae:globalStorage:" + rawID
}

func resolveLegacyTraeNamespacedID(
	oldID, filePath, sourceSessionID string,
	exists func(string) (bool, error),
) (string, error) {
	workspaceID, globalID := legacyTraeSiblingSessionIDs(oldID, sourceSessionID)
	if workspaceID == "" || globalID == "" {
		return "", nil
	}

	namespace := traeSessionNamespaceFromPath(filePath)
	switch namespace {
	case "workspaceStorage":
		ok, err := exists(workspaceID)
		if err != nil {
			return "", err
		}
		if ok {
			return workspaceID, nil
		}
		return "", nil
	case "globalStorage":
		ok, err := exists(globalID)
		if err != nil {
			return "", err
		}
		if ok {
			return globalID, nil
		}
		return "", nil
	}

	workspaceExists, err := exists(workspaceID)
	if err != nil {
		return "", err
	}
	globalExists, err := exists(globalID)
	if err != nil {
		return "", err
	}
	switch {
	case workspaceExists && !globalExists:
		return workspaceID, nil
	case globalExists && !workspaceExists:
		return globalID, nil
	default:
		return "", nil
	}
}

func rebuildLegacyTraeTargetMapTx(
	ctx context.Context,
	tx *sql.Tx,
	sourceSessionsTable, targetSessionsTable string,
) error {
	if _, err := tx.ExecContext(ctx,
		`DROP TABLE IF EXISTS `+legacyTraeTargetMapTable,
	); err != nil {
		return fmt.Errorf("dropping legacy trae target map: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE `+legacyTraeTargetMapTable+` (
			legacy_id TEXT PRIMARY KEY,
			namespaced_id TEXT NOT NULL
		)`,
	); err != nil {
		return fmt.Errorf("creating legacy trae target map: %w", err)
	}

	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, file_path, source_session_id
		FROM %s
		WHERE agent = 'trae'
		  AND (id LIKE 'trae:%%' OR id LIKE '%%~trae:%%')
		  AND id NOT LIKE 'trae:workspaceStorage:%%'
		  AND id NOT LIKE 'trae:globalStorage:%%'
		  AND id NOT LIKE '%%~trae:workspaceStorage:%%'
		  AND id NOT LIKE '%%~trae:globalStorage:%%'`,
		sourceSessionsTable,
	))
	if err != nil {
		return fmt.Errorf("querying legacy trae target map rows: %w", err)
	}
	defer rows.Close()

	exists := func(id string) (bool, error) {
		var ok bool
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(
				`SELECT EXISTS(SELECT 1 FROM %s WHERE id = ?)`,
				targetSessionsTable,
			),
			id,
		).Scan(&ok); err != nil {
			return false, err
		}
		return ok, nil
	}

	for rows.Next() {
		var (
			oldID           string
			filePath        sql.NullString
			sourceSessionID sql.NullString
		)
		if err := rows.Scan(&oldID, &filePath, &sourceSessionID); err != nil {
			return fmt.Errorf("scanning legacy trae target map row: %w", err)
		}
		namespacedID, err := resolveLegacyTraeNamespacedID(
			oldID,
			filePath.String,
			sourceSessionID.String,
			exists,
		)
		if err != nil {
			return fmt.Errorf("resolving legacy trae target for %s: %w", oldID, err)
		}
		if namespacedID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+legacyTraeTargetMapTable+` (legacy_id, namespaced_id)
			 VALUES (?, ?)`,
			oldID, namespacedID,
		); err != nil {
			return fmt.Errorf(
				"inserting legacy trae target map row %s -> %s: %w",
				oldID, namespacedID, err,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating legacy trae target map rows: %w", err)
	}
	return nil
}
