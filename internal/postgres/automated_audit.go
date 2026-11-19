package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type automatedAuditPGProgress struct {
	RowsPrefetched int
	RowsFullText   int
}

const fullAutomationCandidatesPG = `SELECT
	s.id, s.%s::text,
	s.session_kind,
	s.first_message,
	s.user_message_count,
	s.is_automated,
	s.prompt_evidence_discarded,
	(
		SELECT m.content
		FROM messages m
		WHERE m.session_id = s.id
		  AND m.role = 'user'
		  AND m.source_subtype <> 'tool_result'
		  AND m.is_system = false
		  AND btrim(m.content) <> ''
		ORDER BY m.ordinal
		LIMIT 1
	) AS first_user_message
 FROM sessions s`

func backfillIsAutomatedPGWithProgress(
	ctx context.Context, pg *sql.DB,
) (automatedAuditPGProgress, error) {
	var progress automatedAuditPGProgress
	if err := repairParentlessWorkersPG(ctx, pg); err != nil {
		return progress, err
	}
	current := db.ClassifierHash()
	var stored string
	errHash := pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		db.ClassifierHashKey,
	).Scan(&stored)
	if errHash != nil && !errors.Is(errHash, sql.ErrNoRows) {
		return progress, fmt.Errorf(
			"probing PG classifier hash: %w", errHash,
		)
	}

	column, err := sessionVersionColumnPG(ctx, pg)
	if err != nil {
		return progress, err
	}
	tracking, err := prepareAutomationAuditPG(ctx, pg, column)
	if err != nil {
		return progress, err
	}
	classifier := db.SnapshotAutomationClassifier()
	var verdicts []automationVerdictPG
	if tracking {
		if errors.Is(errHash, sql.ErrNoRows) {
			if _, err := pg.ExecContext(ctx, `UPDATE sessions SET automation_audit_hash = NULL WHERE automation_audit_hash IS NOT NULL`); err != nil {
				return progress, err
			}
		}
		ids, err := automationCandidateIDsPG(ctx, pg, current)
		if err != nil {
			return progress, err
		}
		if err := pgQueryChunked(ids, func(chunk []string) error {
			batch, err := auditAutomatedMatchingHashPG(ctx, pg, classifier, &progress, column, chunk)
			if err != nil {
				return err
			}
			return writeAutomationVerdictsPG(ctx, pg, batch, column, current)
		}); err != nil {
			return progress, err
		}
	} else if stored == current {
		verdicts, err = auditAutomatedMatchingHashPG(ctx, pg, classifier, &progress, column, nil)
	} else {
		verdicts, err = auditAutomatedFullPG(ctx, pg, classifier, &progress, column)
	}
	if err != nil {
		return progress, err
	}
	if err := writeAutomationVerdictsPG(ctx, pg, verdicts, column, ""); err != nil {
		return progress, err
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

const repairParentlessWorkersSQL = `UPDATE sessions
		SET relationship_type = 'subagent', updated_at = NOW()
		WHERE ` + db.ParentlessWorkerSQL

func repairParentlessWorkersPG(ctx context.Context, pg *sql.DB) error {
	if _, err := pg.ExecContext(ctx, repairParentlessWorkersSQL); err != nil {
		return fmt.Errorf("repairing PG parentless workers: %w", err)
	}
	return nil
}

func auditAutomatedFullPG(
	ctx context.Context,
	pg *sql.DB,
	classifier db.AutomationClassifier,
	progress *automatedAuditPGProgress,
	column string,
) (verdicts []automationVerdictPG, err error) {
	rows, err := pg.QueryContext(ctx, fmt.Sprintf(fullAutomationCandidatesPG, column))
	if err != nil {
		return nil, fmt.Errorf(
			"querying PG automated backfill candidates: %w", err,
		)
	}
	defer rows.Close()
	verdicts, count, err := scanFullAutomationCandidatesPG(
		rows, classifier, false,
	)
	if closeErr := rows.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	progress.RowsFullText += count
	return verdicts, err
}

func auditAutomatedMatchingHashPG(
	ctx context.Context,
	pg *sql.DB,
	classifier db.AutomationClassifier,
	progress *automatedAuditPGProgress,
	column string, ids []string,
) (verdicts []automationVerdictPG, err error) {
	// Fetch bytea prefixes because the classifier's evidence limit is bytes,
	// not characters. This also avoids affected PostgreSQL minors rejecting
	// valid multibyte text when text substring operates on compressed TOAST.
	filter := ""
	pb := &paramBuilder{}
	pb.add(db.AutomationEvidencePrefixBytes)
	if ids != nil {
		filter = " WHERE s.id IN " + pgInPlaceholders(ids, pb)
	}
	rows, err := pg.QueryContext(ctx,
		`SELECT
			s.id, s.`+column+`::text,
			s.session_kind,
			s.user_message_count,
			s.is_automated,
			s.prompt_evidence_discarded,
			CASE WHEN s.user_message_count <= 1
				THEN substring(
					convert_to(first_user.content, 'UTF8') FROM 1 FOR $1
				)
			END AS first_user_prefix,
			CASE WHEN s.user_message_count <= 1
				THEN octet_length(first_user.content)
			END AS first_user_length,
			CASE
				WHEN s.user_message_count <= 1
				 AND s.first_message IS NOT NULL
				THEN substring(
					convert_to(s.first_message, 'UTF8') FROM 1 FOR $1
				)
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
			  AND s.user_message_count <= 1
			  AND m.role = 'user'
			  AND m.source_subtype <> 'tool_result'
			  AND m.is_system = false
			  AND btrim(m.content) <> ''
			ORDER BY m.ordinal
			LIMIT 1
		 ) first_user ON s.user_message_count <= 1`+filter,
		pb.args...,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"querying bounded PG automated audit candidates: %w", err,
		)
	}
	defer rows.Close()

	var unresolved []string
	for rows.Next() {
		var (
			id, version             string
			sessionKind             string
			userMessageCount        int
			rowAutomated            bool
			promptEvidenceDiscarded bool
			firstUserPrefix         []byte
			firstUserLength         sql.NullInt64
			firstMessagePrefix      []byte
			firstMessageLength      sql.NullInt64
		)
		if err := rows.Scan(
			&id, &version,
			&sessionKind,
			&userMessageCount,
			&rowAutomated,
			&promptEvidenceDiscarded,
			&firstUserPrefix,
			&firstUserLength,
			&firstMessagePrefix,
			&firstMessageLength,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf(
				"scanning bounded PG automated audit candidate: %w", err,
			)
		}
		progress.RowsPrefetched++
		// SQLite keeps the stored verdict when usage-only storage discarded prompt evidence.
		if promptEvidenceDiscarded {
			if ids != nil {
				verdicts = append(verdicts, automationVerdictPG{id, version, rowAutomated})
			}
			continue
		}
		if db.IsAutomatedSessionMetadata(sessionKind) {
			if ids != nil || !rowAutomated {
				verdicts = append(verdicts, automationVerdictPG{id, version, true})
			}
			continue
		}

		want, conclusive := classifier.VerdictFromEvidence(
			userMessageCount,
			automationEvidencePG(firstUserPrefix, firstUserLength),
			automationEvidencePG(firstMessagePrefix, firstMessageLength),
		)
		if !conclusive {
			unresolved = append(unresolved, id)
			continue
		}
		if ids != nil || want != rowAutomated {
			verdicts = append(verdicts, automationVerdictPG{id, version, want})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
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
		batchVerdicts, count, err := func() ([]automationVerdictPG, int, error) {
			fullRows, err := pg.QueryContext(
				ctx,
				fmt.Sprintf(fullAutomationCandidatesPG, column)+
					" WHERE s.id IN ("+strings.Join(placeholders, ",")+")",
				pb.args...,
			)
			if err != nil {
				return nil, 0, fmt.Errorf(
					"querying unresolved PG automated audit candidates: %w", err,
				)
			}
			defer fullRows.Close()
			batchVerdicts, count, scanErr := scanFullAutomationCandidatesPG(fullRows, classifier, ids != nil)
			if closeErr := fullRows.Close(); scanErr == nil && closeErr != nil {
				scanErr = closeErr
			}
			if scanErr != nil {
				return nil, 0, scanErr
			}
			return batchVerdicts, count, nil
		}()
		if err != nil {
			return nil, err
		}
		progress.RowsFullText += count
		verdicts = append(verdicts, batchVerdicts...)
	}
	return verdicts, nil
}

func scanFullAutomationCandidatesPG(
	rows *sql.Rows,
	classifier db.AutomationClassifier,
	includeUnchanged bool,
) (verdicts []automationVerdictPG, count int, err error) {
	for rows.Next() {
		var (
			id, version             string
			sessionKind             string
			firstMessage            sql.NullString
			firstUser               sql.NullString
			userCount               int
			rowAutomated            bool
			promptEvidenceDiscarded bool
		)
		if err := rows.Scan(
			&id, &version, &sessionKind,
			&firstMessage, &userCount, &rowAutomated, &promptEvidenceDiscarded, &firstUser,
		); err != nil {
			return nil, count, fmt.Errorf(
				"scanning PG automated audit candidate: %w", err,
			)
		}
		count++
		// SQLite keeps the stored verdict when usage-only storage discarded prompt evidence.
		if promptEvidenceDiscarded {
			if includeUnchanged {
				verdicts = append(verdicts, automationVerdictPG{id, version, rowAutomated})
			}
			continue
		}
		want := db.IsAutomatedSessionMetadata(sessionKind)
		want = want || classifier.IsAutomatedFromTextCandidates(
			userCount, firstUser, firstMessage,
		)
		if includeUnchanged || want != rowAutomated {
			verdicts = append(verdicts, automationVerdictPG{id, version, want})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, count, err
	}
	return verdicts, count, nil
}

func automationEvidencePG(
	prefix []byte,
	fullByteLength sql.NullInt64,
) db.AutomationTextEvidence {
	return db.AutomationTextEvidence{
		Prefix:         prefix,
		FullByteLength: fullByteLength.Int64,
		Valid:          fullByteLength.Valid,
	}
}

// Each verdict is tied to the committed row that supplied its evidence.
type automationVerdictPG struct {
	id, version string
	automated   bool
}

func writeAutomationVerdictsPG(ctx context.Context, pg *sql.DB, verdicts []automationVerdictPG, column, hash string) error {
	for start := 0; start < len(verdicts); start += 500 {
		pb := &paramBuilder{}
		setStamp, changed := "", " AND s.is_automated IS DISTINCT FROM v.automated"
		if hash != "" {
			setStamp, changed = ", automation_audit_hash = "+pb.add(hash), ""
		}
		var values []string
		for _, v := range verdicts[start:min(start+500, len(verdicts))] {
			values = append(values, "("+pb.add(v.id)+"::text,"+pb.add(v.version)+"::text,"+pb.add(v.automated)+"::boolean)")
		}
		_, err := pg.ExecContext(ctx, `UPDATE sessions s SET is_automated = v.automated`+setStamp+`
		 FROM (VALUES `+strings.Join(values, ",")+`) v(id, version, automated)
		 WHERE s.id = v.id AND s.`+column+`::text = v.version`+changed, pb.args...)
		if err != nil {
			return fmt.Errorf("writing PG automation verdicts: %w", err)
		}
	}
	return nil
}

const automationCandidateIDsSQL = `SELECT id FROM sessions WHERE automation_audit_hash IS NULL
 UNION ALL SELECT id FROM sessions WHERE automation_audit_hash < $1
 UNION ALL SELECT id FROM sessions WHERE automation_audit_hash > $1`

func automationCandidateIDsPG(ctx context.Context, pg *sql.DB, hash string) ([]string, error) {
	rows, err := pg.QueryContext(ctx, automationCandidateIDsSQL, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}
