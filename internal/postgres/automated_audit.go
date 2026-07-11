package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type automatedAuditPGProgress struct {
	RowsPrefetched int
	RowsFullText   int
}

const fullAutomationCandidatesPG = `SELECT
	s.id,
	s.first_message,
	s.user_message_count,
	s.is_automated,
	(
		SELECT m.content
		FROM messages m
		WHERE m.session_id = s.id
		  AND m.role = 'user'
		  AND COALESCE(m.is_system, false) = false
		  AND btrim(m.content) <> ''
		ORDER BY m.ordinal
		LIMIT 1
	) AS first_user_message
 FROM sessions s`

func backfillIsAutomatedPGWithProgress(
	ctx context.Context, pg *sql.DB,
) (automatedAuditPGProgress, error) {
	var progress automatedAuditPGProgress
	current := db.ClassifierHash()
	var stored string
	err := pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		db.ClassifierHashKey,
	).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return progress, fmt.Errorf(
			"probing PG classifier hash: %w", err,
		)
	}

	var setIDs, clearIDs []string
	classifier := db.SnapshotAutomationClassifier()
	if stored == current {
		setIDs, clearIDs, err = auditAutomatedMatchingHashPG(
			ctx, pg, classifier, &progress,
		)
	} else {
		setIDs, clearIDs, err = auditAutomatedFullPG(
			ctx, pg, classifier, &progress,
		)
	}
	if err != nil {
		return progress, err
	}

	if err := batchUpdateAutomatedPG(ctx, pg, setIDs, true); err != nil {
		return progress, err
	}
	if err := batchUpdateAutomatedPG(ctx, pg, clearIDs, false); err != nil {
		return progress, err
	}
	if len(setIDs) > 0 || len(clearIDs) > 0 {
		log.Printf(
			"pg migration: recomputed is_automated"+
				" (set %d, cleared %d)",
			len(setIDs), len(clearIDs),
		)
	}

	if _, err := pg.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value`,
		db.ClassifierHashKey, current,
	); err != nil {
		return progress, fmt.Errorf(
			"storing PG classifier hash: %w", err,
		)
	}
	return progress, nil
}

func auditAutomatedFullPG(
	ctx context.Context,
	pg *sql.DB,
	classifier db.AutomationClassifier,
	progress *automatedAuditPGProgress,
) (setIDs, clearIDs []string, err error) {
	rows, err := pg.QueryContext(ctx, fullAutomationCandidatesPG)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying PG automated backfill candidates: %w", err,
		)
	}
	setIDs, clearIDs, count, err := scanFullAutomationCandidatesPG(
		rows, classifier,
	)
	if closeErr := rows.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	progress.RowsFullText += count
	return setIDs, clearIDs, err
}

func auditAutomatedMatchingHashPG(
	ctx context.Context,
	pg *sql.DB,
	classifier db.AutomationClassifier,
	progress *automatedAuditPGProgress,
) (setIDs, clearIDs []string, err error) {
	rows, err := pg.QueryContext(ctx,
		`SELECT
			s.id,
			s.user_message_count,
			s.is_automated,
			CASE WHEN s.user_message_count <= 1
				THEN left(first_user.content, $1)
			END AS first_user_prefix,
			CASE WHEN s.user_message_count <= 1
				THEN octet_length(first_user.content)
			END AS first_user_length,
			CASE
				WHEN s.user_message_count <= 1
				 AND s.first_message IS NOT NULL
				THEN left(s.first_message, $1)
			END AS first_message_prefix,
			CASE
				WHEN s.user_message_count <= 1
				 AND s.first_message IS NOT NULL
				THEN octet_length(s.first_message)
			END AS first_message_length
		 FROM sessions s
		 LEFT JOIN LATERAL (
			SELECT m.content
			FROM messages m
			WHERE m.session_id = s.id
			  AND m.role = 'user'
			  AND COALESCE(m.is_system, false) = false
			  AND btrim(m.content) <> ''
			ORDER BY m.ordinal
			LIMIT 1
		 ) first_user ON s.user_message_count <= 1`,
		db.AutomationEvidencePrefixBytes,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying bounded PG automated audit candidates: %w", err,
		)
	}

	var unresolved []string
	for rows.Next() {
		var (
			id                 string
			userMessageCount   int
			rowAutomated       bool
			firstUserPrefix    sql.NullString
			firstUserLength    sql.NullInt64
			firstMessagePrefix sql.NullString
			firstMessageLength sql.NullInt64
		)
		if err := rows.Scan(
			&id,
			&userMessageCount,
			&rowAutomated,
			&firstUserPrefix,
			&firstUserLength,
			&firstMessagePrefix,
			&firstMessageLength,
		); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf(
				"scanning bounded PG automated audit candidate: %w", err,
			)
		}
		progress.RowsPrefetched++

		want, conclusive := classifier.VerdictFromEvidence(
			userMessageCount,
			automationEvidencePG(firstUserPrefix, firstUserLength),
			automationEvidencePG(firstMessagePrefix, firstMessageLength),
		)
		if !conclusive {
			unresolved = append(unresolved, id)
			continue
		}
		setIDs, clearIDs = appendAutomationFlagChangePG(
			setIDs, clearIDs, id, rowAutomated, want,
		)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}

	const batchSize = 500
	for start := 0; start < len(unresolved); start += batchSize {
		end := min(start+batchSize, len(unresolved))
		batch := unresolved[start:end]
		pb := &paramBuilder{}
		placeholders := make([]string, len(batch))
		for i, id := range batch {
			placeholders[i] = pb.add(id)
		}
		fullRows, err := pg.QueryContext(
			ctx,
			fullAutomationCandidatesPG+
				" WHERE s.id IN ("+strings.Join(placeholders, ",")+")",
			pb.args...,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"querying unresolved PG automated audit candidates: %w", err,
			)
		}
		batchSet, batchClear, count, scanErr :=
			scanFullAutomationCandidatesPG(fullRows, classifier)
		if closeErr := fullRows.Close(); scanErr == nil && closeErr != nil {
			scanErr = closeErr
		}
		if scanErr != nil {
			return nil, nil, scanErr
		}
		progress.RowsFullText += count
		setIDs = append(setIDs, batchSet...)
		clearIDs = append(clearIDs, batchClear...)
	}
	return setIDs, clearIDs, nil
}

func scanFullAutomationCandidatesPG(
	rows *sql.Rows,
	classifier db.AutomationClassifier,
) (setIDs, clearIDs []string, count int, err error) {
	for rows.Next() {
		var (
			id           string
			firstMessage sql.NullString
			firstUser    sql.NullString
			userCount    int
			rowAutomated bool
		)
		if err := rows.Scan(
			&id, &firstMessage, &userCount, &rowAutomated, &firstUser,
		); err != nil {
			return nil, nil, count, fmt.Errorf(
				"scanning PG automated audit candidate: %w", err,
			)
		}
		count++
		want := classifier.IsAutomatedFromTextCandidates(
			userCount, firstUser, firstMessage,
		)
		setIDs, clearIDs = appendAutomationFlagChangePG(
			setIDs, clearIDs, id, rowAutomated, want,
		)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, count, err
	}
	return setIDs, clearIDs, count, nil
}

func automationEvidencePG(
	prefix sql.NullString,
	fullByteLength sql.NullInt64,
) db.AutomationTextEvidence {
	return db.AutomationTextEvidence{
		Prefix:         []byte(prefix.String),
		FullByteLength: fullByteLength.Int64,
		Valid:          fullByteLength.Valid,
	}
}

func appendAutomationFlagChangePG(
	setIDs, clearIDs []string,
	id string,
	rowAutomated, want bool,
) ([]string, []string) {
	if want && !rowAutomated {
		setIDs = append(setIDs, id)
	} else if !want && rowAutomated {
		clearIDs = append(clearIDs, id)
	}
	return setIDs, clearIDs
}
