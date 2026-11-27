package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

type automatedAuditPGProgress struct {
	RowsPrefetched int
	RowsFullText   int
}

const fullAutomationCandidatesPG = `SELECT
	s.id, s.%s::text,
	s.session_kind,
	s.entrypoint,
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
	column, err := sessionVersionColumnPG(ctx, pg)
	if err != nil {
		return automatedAuditPGProgress{}, err
	}
	tracking, err := prepareAutomationAuditPG(ctx, pg, column)
	if err != nil {
		return automatedAuditPGProgress{}, err
	}
	progress, _, _, err := backfillIsAutomatedPGCore(ctx, pg, tracking, column, false)
	return progress, err
}

// backfillHostedAutomationPG reconciles automation verdicts and parentless
// headless worker relationships inside a hosted tenant provisioning/upgrade
// transaction, refreshes outcome and health signals for raw sessions whose
// automation status changed, and advances raw_corpus_state.corpus_revision
// when any row changes.
func backfillHostedAutomationPG(ctx context.Context, tx *sql.Tx) error {
	column, err := sessionVersionColumnPG(ctx, tx)
	if err != nil {
		return err
	}
	tracking := false
	if column == "xmin" {
		tracking, err = automationAuditSchemaCurrentPG(ctx, tx)
		if err != nil {
			return err
		}
	}
	_, changed, changedAutomationIDs, err := backfillIsAutomatedPGCore(ctx, tx, tracking, column, true)
	if err != nil {
		return err
	}
	if err := refreshHostedRawSignalsPG(ctx, tx, changedAutomationIDs); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_corpus_state(singleton, corpus_revision)
		 VALUES (1, 1)
		 ON CONFLICT (singleton) DO UPDATE
		 SET corpus_revision = raw_corpus_state.corpus_revision + 1`)
	return err
}

func refreshHostedRawSignalsPG(ctx context.Context, tx *sql.Tx, changedIDs []string) error {
	if len(changedIDs) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id, s.is_automated
		 FROM sessions s
		 JOIN raw_content_revisions r ON r.session_id = s.id
		 WHERE s.id = ANY($1)
		   AND s.provenance_kind = 'raw'
		   AND NOT s.prompt_evidence_discarded
		 ORDER BY s.id`,
		changedIDs,
	)
	if err != nil {
		return fmt.Errorf("loading changed hosted raw sessions for signal refresh: %w", err)
	}
	defer rows.Close()
	type changedRaw struct {
		id          string
		isAutomated bool
	}
	var targets []changedRaw
	for rows.Next() {
		var item changedRaw
		if err := rows.Scan(&item.id, &item.isAutomated); err != nil {
			return err
		}
		targets = append(targets, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	now := time.Now().UTC()
	for _, target := range targets {
		p, err := loadRawPayload(ctx, tx, target.id)
		if err != nil {
			return fmt.Errorf("loading raw payload for signal refresh %s: %w", target.id, err)
		}
		p.Session.IsAutomated = target.isAutomated
		p.Signals = ingest.RefreshSignalRecencyAt(p.Session, p.Messages, p.Signals, now)
		if _, err := publishRawRecency(ctx, tx, target.id, p.Signals); err != nil {
			return fmt.Errorf("publishing refreshed signals for %s: %w", target.id, err)
		}
	}
	return nil
}

func backfillIsAutomatedPGCore(
	ctx context.Context, pg storeExecutor, tracking bool, column string, hosted bool,
) (automatedAuditPGProgress, bool, []string, error) {
	var progress automatedAuditPGProgress
	current := db.ClassifierHash()
	var stored string
	errHash := pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		db.ClassifierHashKey,
	).Scan(&stored)
	if errHash != nil && !errors.Is(errHash, sql.ErrNoRows) {
		return progress, false, nil, fmt.Errorf(
			"probing PG classifier hash: %w", errHash,
		)
	}

	classifier := db.SnapshotAutomationClassifier()
	var verdicts []automationVerdictPG
	var candidateIDs []string
	var changedAutomationIDs []string
	var reconciled bool
	var err error
	fullReconcile := stored != current
	if tracking {
		if errors.Is(errHash, sql.ErrNoRows) {
			if _, err := pg.ExecContext(ctx, `UPDATE sessions SET automation_audit_hash = NULL WHERE automation_audit_hash IS NOT NULL`); err != nil {
				return progress, false, nil, err
			}
		}
		candidateIDs, err = automationCandidateIDsPG(ctx, pg, current)
		if err != nil {
			return progress, false, nil, err
		}
		if err := pgQueryChunked(candidateIDs, func(chunk []string) error {
			batch, err := auditAutomatedMatchingHashPG(ctx, pg, classifier, &progress, column, chunk)
			if err != nil {
				return err
			}
			ids, chunkReconciled, err := applyVerdictBatchPG(ctx, pg, batch, chunk, column, current, true, hosted)
			if err != nil {
				return err
			}
			changedAutomationIDs = append(changedAutomationIDs, ids...)
			reconciled = reconciled || chunkReconciled
			return nil
		}); err != nil {
			return progress, false, nil, err
		}
	} else if stored == current {
		verdicts, err = auditAutomatedMatchingHashPG(ctx, pg, classifier, &progress, column, nil)
	} else {
		verdicts, err = auditAutomatedFullPG(ctx, pg, classifier, &progress, column)
	}
	if err != nil {
		return progress, false, nil, err
	}
	for start := 0; start < len(verdicts); start += 500 {
		batch := verdicts[start:min(start+500, len(verdicts))]
		ids, _, err := applyVerdictBatchPG(ctx, pg, batch, nil, column, "", false, hosted)
		if err != nil {
			return progress, false, nil, err
		}
		changedAutomationIDs = append(changedAutomationIDs, ids...)
	}
	remainingReconciled, err := reconcileParentlessWorkersPG(ctx, pg, tracking, current, column, fullReconcile, hosted)
	if err != nil {
		return progress, false, nil, err
	}
	reconciled = reconciled || remainingReconciled

	if _, err := pg.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value`,
		db.ClassifierHashKey, current,
	); err != nil {
		return progress, false, nil, fmt.Errorf(
			"storing PG classifier hash: %w", err,
		)
	}
	return progress, len(changedAutomationIDs) > 0 || reconciled, changedAutomationIDs, nil
}

func applyVerdictBatchPG(
	ctx context.Context, pg storeExecutor, batch []automationVerdictPG, chunk []string,
	column, hash string, tracking, hosted bool,
) ([]string, bool, error) {
	var batchChanged []string
	for _, v := range batch {
		if v.changed {
			batchChanged = append(batchChanged, v.id)
		}
	}
	var reconciled bool
	err := withOptionalTxPG(ctx, pg, func(exec storeExecutor) error {
		if err := writeAutomationVerdictsPG(ctx, exec, batch, column, hash); err != nil {
			return err
		}
		if err := refreshSessionSignalsPG(ctx, exec, batchChanged, tracking, hash, hosted); err != nil {
			return err
		}
		if tracking {
			chunkReconciled, err := reconcileParentlessWorkersChunkPG(ctx, exec, chunk, hash, column, hosted)
			if err != nil {
				return err
			}
			reconciled = chunkReconciled
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return batchChanged, reconciled, nil
}

func withOptionalTxPG(ctx context.Context, pg storeExecutor, fn func(storeExecutor) error) error {
	dbConn, ok := pg.(*sql.DB)
	if !ok {
		return fn(pg)
	}
	tx, err := dbConn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting PG automation update transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing PG automation update batch: %w", err)
	}
	return nil
}

func refreshSessionSignalsPG(
	ctx context.Context, pg storeExecutor, changedIDs []string,
	tracking bool, hash string, hosted bool,
) error {
	if len(changedIDs) == 0 {
		return nil
	}
	now := time.Now().UTC()
	type refreshUpdate struct {
		id          string
		isAutomated bool
		res         db.SessionSignalRefreshResult
	}
	return pgQueryChunked(changedIDs, func(chunk []string) error {
		pb := &paramBuilder{}
		where := `WHERE s.id = ANY(` + pb.add(chunk) + `::text[])
		  AND NOT s.prompt_evidence_discarded
		  AND NOT (
		      s.outcome = '' AND s.outcome_confidence = ''
		      AND s.health_score IS NULL AND s.health_grade IS NULL
		      AND s.signals_pending_since IS NULL
		  )
		  AND (
		      s.quality_signal_version > 0
		      OR s.outcome <> 'unknown'
		      OR s.outcome_confidence <> 'low'
		      OR s.health_score IS NOT NULL
		      OR s.health_grade IS NOT NULL
		      OR s.signals_pending_since IS NOT NULL
		  )`
		if hosted {
			where += ` AND s.provenance_kind <> 'raw'`
		}
		if tracking {
			where += ` AND s.automation_audit_hash = ` + pb.add(hash)
		}
		rows, err := pg.QueryContext(ctx,
			`SELECT s.id, s.is_automated, s.message_count, s.ended_at,
			        s.ended_with_role, s.final_failure_streak,
			        s.signals_pending_since, s.has_tool_calls,
			        s.tool_failure_signal_count, s.tool_retry_count,
			        s.edit_churn_count, s.consecutive_failure_max,
			        s.has_context_data, s.compaction_count,
			        s.mid_task_compaction_count, s.context_pressure_max,
			        s.short_prompt_count, s.unstructured_start,
			        s.missing_success_criteria_count,
			        s.missing_verification_count, s.duplicate_prompt_count,
			        s.no_code_context_count, s.runaway_tool_loop_count,
			        s.outcome, s.outcome_confidence,
			        s.health_score, s.health_grade,
			        COALESCE(last_msg.role, '') AS last_role,
			        COALESCE(last_msg.content, '') AS last_content
			 FROM sessions s
			 LEFT JOIN LATERAL (
			     SELECT m.role, m.content
			     FROM messages m
			     WHERE m.session_id = s.id
			       AND m.is_system = false
			       AND m.source_subtype <> 'tool_result'
			     ORDER BY m.ordinal DESC
			     LIMIT 1
			 ) last_msg ON true
			 `+where,
			pb.args...,
		)
		if err != nil {
			return fmt.Errorf("loading changed PG sessions for signal refresh: %w", err)
		}
		defer rows.Close()
		var updates []refreshUpdate
		for rows.Next() {
			var (
				id, curOutcome, curConfidence string
				lastRole, lastContent         string
				curScore                      *int
				curGrade                      *string
				in                            db.SessionSignalRefreshInput
			)
			if err := rows.Scan(
				&id, &in.IsAutomated, &in.MessageCount, &in.EndedAt,
				&in.EndedWithRole, &in.FinalFailureStreak,
				&in.SignalsPendingSince, &in.HasToolCalls,
				&in.ToolFailureSignalCount, &in.ToolRetryCount,
				&in.EditChurnCount, &in.ConsecutiveFailureMax,
				&in.HasContextData, &in.CompactionCount,
				&in.MidTaskCompactionCount, &in.ContextPressureMax,
				&in.ShortPromptCount, &in.UnstructuredStart,
				&in.MissingSuccessCriteriaCount,
				&in.MissingVerificationCount, &in.DuplicatePromptCount,
				&in.NoCodeContextCount, &in.RunawayToolLoopCount,
				&curOutcome, &curConfidence,
				&curScore, &curGrade,
				&lastRole, &lastContent,
			); err != nil {
				return fmt.Errorf("scanning changed PG session for signal refresh: %w", err)
			}
			if next, changed := in.RefreshIfChanged(
				lastRole, lastContent, curOutcome, curConfidence, curScore, curGrade, now,
			); changed {
				updates = append(updates, refreshUpdate{
					id:          id,
					isAutomated: in.IsAutomated,
					res:         next,
				})
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("reading changed PG sessions for signal refresh: %w", err)
		}
		for _, u := range updates {
			args := []any{
				u.id, u.res.Outcome, u.res.OutcomeConfidence,
				u.res.SignalsPendingSince, u.res.HealthScore,
				u.res.HealthGrade, u.isAutomated,
			}
			guard := ` AND is_automated = $7`
			if tracking {
				guard += ` AND automation_audit_hash = $8`
				args = append(args, hash)
			}
			if _, err := pg.ExecContext(ctx,
				`UPDATE sessions
				 SET outcome = $2,
				     outcome_confidence = $3,
				     signals_pending_since = $4,
				     health_score = $5,
				     health_grade = $6
				 WHERE id = $1`+guard+`
				   AND (outcome, outcome_confidence, signals_pending_since, health_score, health_grade)
				       IS DISTINCT FROM ($2::text, $3::text, $4::text, $5::int, $6::text)`,
				args...,
			); err != nil {
				return fmt.Errorf("refreshing PG signals for session %s: %w", u.id, err)
			}
		}
		return nil
	})
}

// clearableHeadlessWorkerPG mirrors the SQLite rule: an automated parentless
// session counts as Automated only when its subagent role was inferred from
// headless shape and nothing ever spawned or launched it. A launcher link
// counts only once it applies, which spawn_linked records; a link still
// waiting for its parent leaves the row Automated, as in SQLite. Rows pushed
// by older binaries carry no parser relationship or spawn record; they fall
// back to entrypoint/kind matching, treat any launcher link as a spawn, and
// treat Grok rows and native agent-* IDs (including origin-prefixed imports
// and hosted raw-projected rows via source_session_id) as explicit children,
// as SQLite's legacy backfill does.
const clearableHeadlessWorkerPG = `is_automated
		  AND COALESCE(parent_session_id, '') = ''
		  AND NOT spawn_linked
		  AND CASE WHEN parser_relationship_type IS NULL
			THEN (entrypoint = 'sdk-cli' OR (session_kind = 'non-interactive' AND agent <> 'grok'))
				AND COALESCE(external_parent_session_id, '') = ''
				AND COALESCE(parser_parent_session_id, '') = ''
				AND substr(id, strpos(id, '~') + 1, 6) <> 'agent-'
				AND substr(COALESCE(source_session_id, ''), strpos(COALESCE(source_session_id, ''), '~') + 1, 6) <> 'agent-'
			ELSE relationship_inferred END
		  AND NOT EXISTS (
			SELECT 1 FROM tool_calls tc
			WHERE tc.subagent_session_id = sessions.id
			  AND tc.session_id IS DISTINCT FROM tc.subagent_session_id
		  )`

var hostedRawSpawnExistsPG = `EXISTS (
			SELECT 1` + hostedLinkFromSQL + `
			  AND e.kind IN ('call', 'event')
			  AND target.session_id = sessions.id
			  AND owner.session_id IS DISTINCT FROM sessions.id
		  )`

var hostedRawSpawnGuardPG = `
		  AND NOT (provenance_kind = 'raw' AND ` + hostedRawSpawnExistsPG + `)`

type parentlessWorkerStepPG struct {
	query, action string
	bounded       bool
}

func clearParentlessWorkersSQLPG(clearable string) string {
	return `UPDATE sessions SET relationship_type = '',
			parser_relationship_type = COALESCE(parser_relationship_type, 'subagent'),
			relationship_inferred = CASE WHEN parser_relationship_type IS NULL THEN TRUE ELSE relationship_inferred END,
			updated_at = NOW()
		WHERE relationship_type = 'subagent' AND ` + clearable
}

func parentlessWorkerStepsPG(hosted bool) []parentlessWorkerStepPG {
	clearable := clearableHeadlessWorkerPG
	if hosted {
		clearable += hostedRawSpawnGuardPG
	}
	return []parentlessWorkerStepPG{
		{
			`UPDATE sessions SET relationship_type = 'subagent', updated_at = NOW()
		WHERE ` + db.ParentlessWorkerSQL + ` AND NOT (` + clearable + `)`,
			"promoting PG parentless workers",
			true,
		},
		{
			clearParentlessWorkersSQLPG(clearable),
			"clearing PG automated parentless workers",
			false,
		},
		{
			`UPDATE sessions SET relationship_type = 'subagent', updated_at = NOW()
		WHERE COALESCE(relationship_type, '') = ''
		  AND COALESCE(parent_session_id, '') = ''
		  AND parser_relationship_type = 'subagent'
		  AND NOT (` + clearable + `)`,
			"restoring PG parentless workers",
			false,
		},
	}
}

func reconcileStampedPG(
	ctx context.Context, exec storeExecutor, hosted bool, column, hash string,
	include func(parentlessWorkerStepPG) bool, extra string, args ...any,
) (bool, error) {
	var verdicts []automationVerdictPG
	returning := ` RETURNING id, ` + column + `::text, is_automated`
	queryArgs := append([]any{hash}, args...)
	for _, step := range parentlessWorkerStepsPG(hosted) {
		if !include(step) {
			continue
		}
		changed, err := queryAutomationVerdictsPG(
			ctx, exec, step.query+` AND automation_audit_hash = $1`+extra+returning, queryArgs...,
		)
		if err != nil {
			return false, fmt.Errorf("%s: %w", step.action, err)
		}
		verdicts = append(verdicts, changed...)
	}
	if err := writeAutomationVerdictsPG(ctx, exec, verdicts, column, hash); err != nil {
		return false, err
	}
	return len(verdicts) > 0, nil
}

// reconcileParentlessWorkersChunkPG reconciles parentless-worker relationships
// for one audited chunk inside the same transaction that wrote the chunk's
// automation verdicts, refreshed signals, and audit hash stamp.
func reconcileParentlessWorkersChunkPG(
	ctx context.Context, exec storeExecutor, chunk []string, hash, column string, hosted bool,
) (bool, error) {
	if len(chunk) == 0 {
		return false, nil
	}
	return reconcileStampedPG(
		ctx, exec, hosted, column, hash,
		func(parentlessWorkerStepPG) bool { return true },
		` AND id = ANY($2)`, chunk,
	)
}

// reconcileParentlessWorkersPG applies remaining relationship changes after
// chunk verdicts. With audit tracking, every candidate chunk has already been
// reconciled atomically inside applyVerdictBatchPG, so this only covers
// bounded/full-reconcile steps on already-stamped rows inside one transaction.
func reconcileParentlessWorkersPG(
	ctx context.Context, pg storeExecutor, tracking bool, hash, column string, fullReconcile bool, hosted bool,
) (bool, error) {
	if !tracking {
		var reconciled bool
		for _, step := range parentlessWorkerStepsPG(hosted) {
			res, err := pg.ExecContext(ctx, step.query)
			if err != nil {
				return false, fmt.Errorf("%s: %w", step.action, err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				reconciled = true
			}
		}
		return reconciled, nil
	}
	var reconciled bool
	err := withOptionalTxPG(ctx, pg, func(exec storeExecutor) error {
		var err error
		reconciled, err = reconcileStampedPG(
			ctx, exec, hosted, column, hash,
			func(step parentlessWorkerStepPG) bool { return step.bounded || fullReconcile },
			"",
		)
		return err
	})
	return reconciled, err
}

// reconcileRawSpawnLinks updates materialized raw sessions in the affected
// groups or reachable through affected public/spawn aliases whose effective
// relationship depends on resolved hosted spawn links. Scoping the candidate
// set to affected groups and aliases keeps sync work bounded by the batch.
func reconcileRawSpawnLinks(ctx context.Context, tx *sql.Tx, groups, affectedAliases []string) (bool, error) {
	if len(groups) == 0 && len(affectedAliases) == 0 {
		return false, nil
	}
	if groups == nil {
		groups = []string{}
	}
	if affectedAliases == nil {
		affectedAliases = []string{}
	}
	candidateIDs, err := queryStrings(ctx, tx,
		`SELECT DISTINCT s.id
		 FROM sessions s
		 WHERE s.provenance_kind = 'raw'
		   AND (
		       s.raw_group_id = ANY($1::text[])
		       OR s.raw_group_id IN (
		           SELECT a.group_id
		           FROM raw_session_public_aliases a
		           WHERE a.alias_id = ANY($2::text[])
		       )
		   )
		 ORDER BY s.id`,
		groups, affectedAliases,
	)
	if err != nil {
		return false, fmt.Errorf("loading affected raw sessions for spawn link reconciliation: %w", err)
	}
	if len(candidateIDs) == 0 {
		return false, nil
	}
	var changed bool
	for _, query := range []string{
		`UPDATE sessions SET relationship_type = 'subagent', updated_at = NOW()
		WHERE id = ANY($1::text[])
		  AND provenance_kind = 'raw'
		  AND COALESCE(relationship_type, '') = ''
		  AND COALESCE(parent_session_id, '') = ''
		  AND parser_relationship_type = 'subagent'
		  AND ` + hostedRawSpawnExistsPG,
		clearParentlessWorkersSQLPG(clearableHeadlessWorkerPG+hostedRawSpawnGuardPG) + `
		  AND id = ANY($1::text[])
		  AND provenance_kind = 'raw'`,
	} {
		res, err := tx.ExecContext(ctx, query, candidateIDs)
		if err != nil {
			return false, fmt.Errorf("reconciling hosted raw spawn links: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			changed = true
		}
	}
	return changed, nil
}

func queryAutomationVerdictsPG(ctx context.Context, pg storeExecutor, query string, args ...any) ([]automationVerdictPG, error) {
	rows, err := pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var verdicts []automationVerdictPG
	for rows.Next() {
		var v automationVerdictPG
		if err := rows.Scan(&v.id, &v.version, &v.automated); err != nil {
			return nil, err
		}
		verdicts = append(verdicts, v)
	}
	return verdicts, errors.Join(rows.Err(), rows.Close())
}

func auditAutomatedFullPG(
	ctx context.Context,
	pg storeExecutor,
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
	pg storeExecutor,
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
			s.entrypoint,
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
			entrypoint              string
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
			&entrypoint,
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
		if db.IsAutomatedSessionMetadata(sessionKind, entrypoint) {
			if ids != nil || !rowAutomated {
				verdicts = append(verdicts, automationVerdictPG{id, version, true, !rowAutomated})
			}
			continue
		}
		// SQLite keeps the stored verdict when usage-only storage discarded prompt evidence.
		if promptEvidenceDiscarded {
			if ids != nil {
				verdicts = append(verdicts, automationVerdictPG{id, version, rowAutomated, false})
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
			verdicts = append(verdicts, automationVerdictPG{id, version, want, want != rowAutomated})
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
			entrypoint              string
			firstMessage            sql.NullString
			firstUser               sql.NullString
			userCount               int
			rowAutomated            bool
			promptEvidenceDiscarded bool
		)
		if err := rows.Scan(
			&id, &version, &sessionKind, &entrypoint,
			&firstMessage, &userCount, &rowAutomated, &promptEvidenceDiscarded, &firstUser,
		); err != nil {
			return nil, count, fmt.Errorf(
				"scanning PG automated audit candidate: %w", err,
			)
		}
		count++
		if db.IsAutomatedSessionMetadata(sessionKind, entrypoint) {
			if includeUnchanged || !rowAutomated {
				verdicts = append(verdicts, automationVerdictPG{id, version, true, !rowAutomated})
			}
			continue
		}
		// SQLite keeps the stored verdict when usage-only storage discarded prompt evidence.
		if promptEvidenceDiscarded {
			if includeUnchanged {
				verdicts = append(verdicts, automationVerdictPG{id, version, rowAutomated, false})
			}
			continue
		}
		want := classifier.IsAutomatedFromTextCandidates(
			userCount, firstUser, firstMessage,
		)
		if includeUnchanged || want != rowAutomated {
			verdicts = append(verdicts, automationVerdictPG{id, version, want, want != rowAutomated})
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
	changed     bool
}

func writeAutomationVerdictsPG(ctx context.Context, pg storeExecutor, verdicts []automationVerdictPG, column, hash string) error {
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

func automationCandidateIDsPG(ctx context.Context, pg storeExecutor, hash string) ([]string, error) {
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
