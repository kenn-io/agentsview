package duckdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func (s *Sync) syncModelPricing(ctx context.Context) error {
	prices, err := s.local.ListModelPricing(ctx)
	if err != nil {
		return err
	}
	if len(prices) == 0 {
		prices = duckFallbackPricingRows()
	}
	if len(prices) == 0 {
		return nil
	}

	existing, err := s.listDuckModelPricing(ctx)
	if err != nil {
		return err
	}
	_, prices = db.FilterChangedModelPricing(existing, prices)
	if len(prices) == 0 {
		return nil
	}

	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb pricing sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for i := 0; i < len(prices); i += duckPricingUpsertBatch {
		end := min(i+duckPricingUpsertBatch, len(prices))
		batch := prices[i:end]
		query, args := duckPricingUpsertStatement(batch)
		if err := s.execMutation(ctx, tx, query, args...); err != nil {
			return fmt.Errorf(
				"syncing duckdb pricing batch starting at %d: %w",
				i, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb pricing sync: %w", err)
	}
	return nil
}

const duckPricingUpsertBatch = 100

func duckPricingUpsertStatement(prices []db.ModelPricing) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing (
		model_pattern, input_per_mtok, output_per_mtok,
		cache_creation_per_mtok, cache_read_per_mtok, updated_at
	) VALUES `)
	args := make([]any, 0, len(prices)*6)
	for i, p := range prices {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?, ?, ?, ?, ?)")
		args = append(args,
			p.ModelPattern,
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
			p.UpdatedAt,
		)
	}
	b.WriteString(`
	ON CONFLICT(model_pattern) DO UPDATE SET
		input_per_mtok = excluded.input_per_mtok,
		output_per_mtok = excluded.output_per_mtok,
		cache_creation_per_mtok = excluded.cache_creation_per_mtok,
		cache_read_per_mtok = excluded.cache_read_per_mtok,
		updated_at = excluded.updated_at`)
	return b.String(), args
}

func (s *Sync) listDuckModelPricing(ctx context.Context) ([]db.ModelPricing, error) {
	rows, err := queryDuckDBContext(
		ctx, s.duck, s.connectionKind, s.quack,
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
		 FROM model_pricing`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb pricing: %w", err)
	}
	defer rows.Close()

	var out []db.ModelPricing
	for rows.Next() {
		var p db.ModelPricing
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb pricing: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb pricing: %w", err)
	}
	return out, nil
}

func (s *Sync) syncCursorUsageEvents(ctx context.Context) error {
	// Cursor admin rows are global and unattributed, so project-filtered pushes
	// cannot sync them honestly.
	if s.isFiltered() {
		return nil
	}

	events, err := s.local.GetCursorUsageEvents(ctx)
	if err != nil {
		return fmt.Errorf("loading local cursor usage events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb cursor usage sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := s.bulkInsertCursorUsageEvents(ctx, tx, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb cursor usage sync: %w", err)
	}
	return nil
}

func duckFallbackPricingRows() []db.ModelPricing {
	src := pricingpkg.FallbackPricing()
	out := make([]db.ModelPricing, len(src))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, p := range src {
		out[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
			UpdatedAt:            now,
		}
	}
	return out
}

func (s *Sync) replaceStarredSessions(
	ctx context.Context, tx *sql.Tx, sessions []db.Session,
) error {
	ids, err := s.local.ListStarredSessionIDs(ctx)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		allowed[sess.ID] = true
	}
	if s.isFiltered() {
		for _, sess := range sessions {
			if err := s.execMutation(ctx, tx,
				`DELETE FROM starred_sessions WHERE session_id = ?`, sess.ID,
			); err != nil {
				return fmt.Errorf("clearing duckdb starred session %s: %w", sess.ID, err)
			}
		}
	} else {
		if err := s.execMutation(ctx, tx, `
			DELETE FROM starred_sessions
			WHERE session_id IN (
				SELECT id FROM sessions WHERE machine = ?
			)`, s.machine); err != nil {
			return fmt.Errorf("clearing duckdb starred_sessions: %w", err)
		}
	}
	for _, id := range ids {
		if !allowed[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO starred_sessions (session_id, created_at)
			 VALUES (?, current_timestamp)`,
			id,
		); err != nil {
			return fmt.Errorf("syncing starred session %s: %w", id, err)
		}
	}
	return nil
}

func (s *Sync) pushSession(
	ctx context.Context, exec duckMutationExecutor, sess db.Session,
) (int, error) {
	if err := s.upsertSession(ctx, exec, sess); err != nil {
		return 0, err
	}
	if err := s.replaceSessionDependents(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	if err := s.replaceUsageEvents(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	msgs, err := s.local.GetAllMessages(ctx, sess.ID)
	if err != nil {
		return 0, fmt.Errorf("reading local messages for %s: %w", sess.ID, err)
	}
	if err := insertMessages(ctx, exec, msgs); err != nil {
		return 0, err
	}
	if err := s.replaceToolRows(ctx, exec, sess.ID, msgs); err != nil {
		return 0, err
	}
	if err := s.replaceSecretFindings(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	if err := s.replacePinnedMessages(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	return len(msgs), nil
}

func (s *Sync) replaceSessionDependents(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	for _, stmt := range []string{
		`DELETE FROM pinned_messages WHERE session_id = ?`,
		`DELETE FROM secret_findings WHERE session_id = ?`,
		`DELETE FROM usage_events WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
	} {
		if err := s.execMutation(ctx, exec, stmt, sessionID); err != nil {
			return fmt.Errorf("clearing duckdb session dependents: %w", err)
		}
	}
	return nil
}

func (s *Sync) deleteHardDeletedMirrorSessions(
	ctx context.Context, tx *sql.Tx, localSessions []db.Session,
	machine string, projects, excludeProjects []string,
) ([]string, error) {
	localIDs := make(map[string]bool, len(localSessions))
	for _, sess := range localSessions {
		localIDs[sess.ID] = true
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, project FROM sessions WHERE machine = ?`,
		machine,
	)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb sessions for deletion reconciliation: %w", err)
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, fmt.Errorf("scanning duckdb session for deletion reconciliation: %w", err)
		}
		if !projectInSyncScope(project, projects, excludeProjects) {
			continue
		}
		if !localIDs[id] {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(stale)
	for _, id := range stale {
		if err := s.deleteMirrorSession(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

func (s *Sync) deleteMirrorSession(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	for _, stmt := range []string{
		`DELETE FROM pinned_messages WHERE session_id = ?`,
		`DELETE FROM starred_sessions WHERE session_id = ?`,
		`DELETE FROM secret_findings WHERE session_id = ?`,
		`DELETE FROM tool_result_events WHERE session_id = ?`,
		`DELETE FROM tool_calls WHERE session_id = ?`,
		`DELETE FROM usage_events WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM sessions WHERE id = ?`,
	} {
		if err := s.execMutation(ctx, tx, stmt, sessionID); err != nil {
			return fmt.Errorf("deleting hard-deleted duckdb session %s: %w", sessionID, err)
		}
	}
	return nil
}

func projectInSyncScope(project string, projects, excludeProjects []string) bool {
	if len(projects) > 0 {
		found := slices.Contains(projects, project)
		if !found {
			return false
		}
	}
	return !slices.Contains(excludeProjects, project)
}

func (s *Sync) execMutation(
	ctx context.Context, exec duckMutationExecutor, stmt string, args ...any,
) error {
	if s.connectionKind != duckDBQuackClientConnection {
		_, err := exec.ExecContext(ctx, stmt, args...)
		return err
	}
	if _, ok := exec.(*duckRemoteMutationBatch); ok {
		_, err := exec.ExecContext(ctx, stmt, args...)
		return err
	}
	// Quack attachments can accept plain inserts, but DELETE, UPDATE, and
	// ON CONFLICT are planned against proxy storage and currently fail with
	// GetStorageInfo errors. Run those mutations on the server-side base DB.
	sqlText, err := duckSQLWithArgs(stmt, args...)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, "FROM "+quackAttachmentName+".query(?)", sqlText)
	return err
}

type duckMutationExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

const duckRemoteMutationCoalesceMaxBytes = 2 << 20

type duckRemoteMutationBatch struct {
	statements     []string
	statementBytes int
}

func (b *duckRemoteMutationBatch) ExecContext(
	_ context.Context, stmt string, args ...any,
) (sql.Result, error) {
	sqlText, err := duckSQLWithArgs(stmt, args...)
	if err != nil {
		return nil, err
	}
	b.statements = append(b.statements, sqlText)
	b.statementBytes += len(sqlText)
	return duckNoopResult{}, nil
}

func (b *duckRemoteMutationBatch) Len() int {
	return len(b.statements)
}

func (b *duckRemoteMutationBatch) appendBatch(other *duckRemoteMutationBatch) {
	if other == nil || other.Len() == 0 {
		return
	}
	b.statements = append(b.statements, other.statements...)
	b.statementBytes += other.statementBytes
}

type duckNoopResult struct{}

func (duckNoopResult) LastInsertId() (int64, error) { return 0, nil }
func (duckNoopResult) RowsAffected() (int64, error) { return 0, nil }

func (s *Sync) execRemoteMutationBatch(
	ctx context.Context, label string, batch *duckRemoteMutationBatch, coalesce bool,
) error {
	exec := s.execRemoteSQLNoRetry
	if coalesce {
		exec = s.execRemoteSQLRetry
	}
	return execDuckRemoteMutationBatch(ctx, exec, label, batch, coalesce)
}

func appendDuckRemoteMutationBatch(
	ctx context.Context,
	execCoalesced func(context.Context, string) error,
	label string,
	current *duckRemoteMutationBatch,
	next *duckRemoteMutationBatch,
	maxBytes int,
) (*duckRemoteMutationBatch, error) {
	if current == nil {
		current = &duckRemoteMutationBatch{}
	}
	if next == nil || next.Len() == 0 {
		return current, nil
	}
	if maxBytes <= 0 {
		maxBytes = duckRemoteMutationCoalesceMaxBytes
	}
	if next.transactionBytes() > maxBytes {
		if err := execDuckRemoteMutationBatch(
			ctx, execCoalesced, label, current, true,
		); err != nil {
			return current, err
		}
		if err := execDuckRemoteMutationBatchChunks(
			ctx, execCoalesced, label, next, maxBytes,
		); err != nil {
			return &duckRemoteMutationBatch{}, err
		}
		return &duckRemoteMutationBatch{}, nil
	}
	if current.Len() > 0 && current.combinedTransactionBytes(next) > maxBytes {
		if err := execDuckRemoteMutationBatch(ctx, execCoalesced, label, current, true); err != nil {
			return current, err
		}
		current = &duckRemoteMutationBatch{}
	}
	current.appendBatch(next)
	return current, nil
}

func execDuckRemoteMutationBatch(
	ctx context.Context,
	exec func(context.Context, string) error,
	label string,
	batch *duckRemoteMutationBatch,
	coalesce bool,
) (err error) {
	if batch.Len() == 0 {
		return nil
	}
	if coalesce {
		if err := exec(ctx, batch.transactionSQL()); err != nil {
			if isDuckRemoteMutationTimeoutError(err) {
				return err
			}
			if rollbackErr := exec(ctx, "ROLLBACK"); rollbackErr != nil {
				return fmt.Errorf("%w; rollback %s: %v", err, label, rollbackErr)
			}
			return err
		}
		return nil
	}
	if err := exec(ctx, "BEGIN TRANSACTION"); err != nil {
		return fmt.Errorf("begin %s: %w", label, err)
	}
	needsRollback := true
	defer func() {
		if !needsRollback {
			return
		}
		rollbackErr := exec(ctx, "ROLLBACK")
		if err != nil && rollbackErr != nil {
			err = fmt.Errorf("%w; rollback %s: %v", err, label, rollbackErr)
		}
	}()
	for i, stmt := range batch.statements {
		if err := exec(ctx, stmt); err != nil {
			return fmt.Errorf(
				"execute %s statement %d/%d: %w",
				label, i+1, batch.Len(), err,
			)
		}
	}
	if err := exec(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit %s: %w", label, err)
	}
	needsRollback = false
	return nil
}

func execDuckRemoteMutationBatchWithStatementFallback(
	ctx context.Context,
	execCoalesced func(context.Context, string) error,
	execStatement func(context.Context, string) error,
	label string,
	batch *duckRemoteMutationBatch,
) error {
	if err := execDuckRemoteMutationBatch(ctx, execCoalesced, label, batch, true); err != nil {
		if ctx.Err() != nil ||
			isStaleQuackConnectionError(err) ||
			isDuckRemoteMutationTimeoutError(err) {
			return err
		}
		return execDuckRemoteMutationBatch(ctx, execStatement, label, batch, false)
	}
	return nil
}

func execDuckRemoteMutationBatchChunks(
	ctx context.Context,
	exec func(context.Context, string) error,
	label string,
	batch *duckRemoteMutationBatch,
	maxBytes int,
) error {
	for _, chunk := range batch.chunks(maxBytes) {
		if err := execDuckRemoteMutationBatch(ctx, exec, label, chunk, true); err != nil {
			return err
		}
	}
	return nil
}

func execDuckRemoteMutationBatchChunksWithStatementFallback(
	ctx context.Context,
	execCoalesced func(context.Context, string) error,
	execStatement func(context.Context, string) error,
	label string,
	batch *duckRemoteMutationBatch,
	maxBytes int,
) error {
	for _, chunk := range batch.chunks(maxBytes) {
		if err := execDuckRemoteMutationBatchWithStatementFallback(
			ctx, execCoalesced, execStatement, label, chunk,
		); err != nil {
			return err
		}
	}
	return nil
}

func (b *duckRemoteMutationBatch) transactionSQL() string {
	var sqlText strings.Builder
	sqlText.WriteString("BEGIN TRANSACTION;\n")
	for _, stmt := range b.statements {
		sqlText.WriteString(stmt)
		sqlText.WriteString(";\n")
	}
	sqlText.WriteString("COMMIT")
	return sqlText.String()
}

func (b *duckRemoteMutationBatch) transactionBytes() int {
	if b == nil || b.Len() == 0 {
		return 0
	}
	return len("BEGIN TRANSACTION;\n") +
		b.statementBytes +
		len(";\n")*b.Len() +
		len("COMMIT")
}

func (b *duckRemoteMutationBatch) combinedTransactionBytes(
	other *duckRemoteMutationBatch,
) int {
	if b == nil || b.Len() == 0 {
		return other.transactionBytes()
	}
	if other == nil || other.Len() == 0 {
		return b.transactionBytes()
	}
	return len("BEGIN TRANSACTION;\n") +
		b.statementBytes + other.statementBytes +
		len(";\n")*(b.Len()+other.Len()) +
		len("COMMIT")
}

func (b *duckRemoteMutationBatch) chunks(maxBytes int) []*duckRemoteMutationBatch {
	if b == nil || b.Len() == 0 {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = duckRemoteMutationCoalesceMaxBytes
	}
	var chunks []*duckRemoteMutationBatch
	current := &duckRemoteMutationBatch{}
	for _, stmt := range b.statements {
		next := &duckRemoteMutationBatch{
			statements:     []string{stmt},
			statementBytes: len(stmt),
		}
		if current.Len() > 0 && current.combinedTransactionBytes(next) > maxBytes {
			chunks = append(chunks, current)
			current = &duckRemoteMutationBatch{}
		}
		current.appendBatch(next)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

func isDuckRemoteMutationTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout was reached")
}

func (s *Sync) execRemoteSQLRetry(ctx context.Context, sqlText string) error {
	if s.quack != nil {
		return s.quack.execRemote(ctx, sqlText, true)
	}
	return s.execRemoteSQLNoRetry(ctx, sqlText)
}

func (s *Sync) execRemoteSQLNoRetry(ctx context.Context, sqlText string) error {
	if s.quack != nil {
		return s.quack.execRemote(ctx, sqlText, false)
	}
	_, err := s.duck.ExecContext(ctx, "FROM "+quackAttachmentName+".query(?)", sqlText)
	return err
}

func duckSQLWithArgs(stmt string, args ...any) (string, error) {
	var b strings.Builder
	argIndex := 0
	for _, r := range stmt {
		if r != '?' {
			b.WriteRune(r)
			continue
		}
		if argIndex >= len(args) {
			return "", fmt.Errorf("duckdb remote statement missing argument")
		}
		lit, err := duckValueLiteral(args[argIndex])
		if err != nil {
			return "", err
		}
		b.WriteString(lit)
		argIndex++
	}
	if argIndex != len(args) {
		return "", fmt.Errorf("duckdb remote statement has unused argument")
	}
	return b.String(), nil
}

func duckValueLiteral(v any) (string, error) {
	switch value := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return duckRemoteStringLiteral(value)
	case *string:
		if value == nil {
			return "NULL", nil
		}
		return duckRemoteStringLiteral(*value)
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprint(value), nil
	case *int:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case *int64:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case *float64:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case bool:
		if value {
			return "TRUE", nil
		}
		return "FALSE", nil
	case time.Time:
		return "TIMESTAMP " + duckLiteral(
			value.UTC().Format("2006-01-02 15:04:05.999999"),
		), nil
	default:
		return "", fmt.Errorf("unsupported duckdb remote argument type %T", v)
	}
}

func duckRemoteStringLiteral(s string) (string, error) {
	s = strings.ReplaceAll(s, "\x00", "")
	for {
		var tagBytes [16]byte
		if _, err := rand.Read(tagBytes[:]); err != nil {
			return "", fmt.Errorf("generating duckdb string literal tag: %w", err)
		}
		tag := "agentsview_" + hex.EncodeToString(tagBytes[:])
		delimiter := "$" + tag + "$"
		if strings.Contains(s, delimiter) {
			continue
		}
		return delimiter + s + delimiter, nil
	}
}

func (s *Sync) upsertSession(
	ctx context.Context, exec duckMutationExecutor, sess db.Session,
) error {
	query := `
		INSERT INTO sessions (
			id, project, machine, agent,
			first_message, display_name, session_name, started_at, ended_at,
			message_count, user_message_count,
			file_path, file_size, file_mtime, file_inode, file_device,
			file_hash, local_modified_at, parent_session_id,
			relationship_type, total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens, is_automated,
			tool_failure_signal_count, tool_retry_count, edit_churn_count,
			consecutive_failure_max, outcome, outcome_confidence,
			ended_with_role, final_failure_streak, signals_pending_since,
			compaction_count, mid_task_compaction_count,
			context_pressure_max, health_score, health_grade,
			has_tool_calls, has_context_data,
			quality_signal_version, short_prompt_count, unstructured_start,
			missing_success_criteria_count, missing_verification_count,
			duplicate_prompt_count, no_code_context_count,
			runaway_tool_loop_count, data_version,
			cwd, git_branch, source_session_id, source_version, transcript_fidelity,
			parser_malformed_lines, is_truncated, deleted_at, created_at,
			termination_status, secret_leak_count, secrets_rules_version
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?
		)`
	query += `
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			agent = excluded.agent,
			first_message = excluded.first_message,
			display_name = excluded.display_name,
			session_name = excluded.session_name,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			message_count = excluded.message_count,
			user_message_count = excluded.user_message_count,
			file_path = excluded.file_path,
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			file_inode = excluded.file_inode,
			file_device = excluded.file_device,
			file_hash = excluded.file_hash,
			local_modified_at = excluded.local_modified_at,
			parent_session_id = excluded.parent_session_id,
			relationship_type = excluded.relationship_type,
			total_output_tokens = excluded.total_output_tokens,
			peak_context_tokens = excluded.peak_context_tokens,
			has_total_output_tokens = excluded.has_total_output_tokens,
			has_peak_context_tokens = excluded.has_peak_context_tokens,
			is_automated = excluded.is_automated,
			tool_failure_signal_count = excluded.tool_failure_signal_count,
			tool_retry_count = excluded.tool_retry_count,
			edit_churn_count = excluded.edit_churn_count,
			consecutive_failure_max = excluded.consecutive_failure_max,
			outcome = excluded.outcome,
			outcome_confidence = excluded.outcome_confidence,
			ended_with_role = excluded.ended_with_role,
			final_failure_streak = excluded.final_failure_streak,
			signals_pending_since = excluded.signals_pending_since,
			compaction_count = excluded.compaction_count,
			mid_task_compaction_count = excluded.mid_task_compaction_count,
			context_pressure_max = excluded.context_pressure_max,
			health_score = excluded.health_score,
			health_grade = excluded.health_grade,
			has_tool_calls = excluded.has_tool_calls,
			has_context_data = excluded.has_context_data,
			quality_signal_version = excluded.quality_signal_version,
			short_prompt_count = excluded.short_prompt_count,
			unstructured_start = excluded.unstructured_start,
			missing_success_criteria_count = excluded.missing_success_criteria_count,
			missing_verification_count = excluded.missing_verification_count,
			duplicate_prompt_count = excluded.duplicate_prompt_count,
			no_code_context_count = excluded.no_code_context_count,
			runaway_tool_loop_count = excluded.runaway_tool_loop_count,
			data_version = excluded.data_version,
			cwd = excluded.cwd,
			git_branch = excluded.git_branch,
			source_session_id = excluded.source_session_id,
			source_version = excluded.source_version,
			transcript_fidelity = excluded.transcript_fidelity,
			parser_malformed_lines = excluded.parser_malformed_lines,
			is_truncated = excluded.is_truncated,
			deleted_at = excluded.deleted_at,
			created_at = excluded.created_at,
			termination_status = excluded.termination_status,
			secret_leak_count = excluded.secret_leak_count,
			secrets_rules_version = excluded.secrets_rules_version`

	if err := s.execMutation(ctx, exec, query, sessionInsertArgs(sess, s.machine)...); err != nil {
		return fmt.Errorf("writing duckdb session %s: %w", sess.ID, err)
	}
	return nil
}

func sessionInsertArgs(sess db.Session, machine string) []any {
	return []any{
		sess.ID, sess.Project, machine, sess.Agent,
		nilString(sess.FirstMessage), nilString(sess.DisplayName),
		nilString(sess.SessionName),
		nilTime(sess.StartedAt), nilTime(sess.EndedAt),
		sess.MessageCount, sess.UserMessageCount,
		nilString(sess.FilePath), sess.FileSize, sess.FileMtime,
		sess.FileInode, sess.FileDevice, nilString(sess.FileHash),
		nilTime(sess.LocalModifiedAt), nilString(sess.ParentSessionID),
		sess.RelationshipType, sess.TotalOutputTokens,
		sess.PeakContextTokens, sess.HasTotalOutputTokens,
		sess.HasPeakContextTokens, sess.IsAutomated,
		sess.ToolFailureSignalCount, sess.ToolRetryCount,
		sess.EditChurnCount, sess.ConsecutiveFailureMax,
		sess.Outcome, sess.OutcomeConfidence,
		sess.EndedWithRole, sess.FinalFailureStreak,
		nilString(sess.SignalsPendingSince),
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax, sess.HealthScore,
		nilString(sess.HealthGrade), sess.HasToolCalls,
		sess.HasContextData,
		sess.QualitySignalVersion, sess.ShortPromptCount,
		sess.UnstructuredStart, sess.MissingSuccessCriteriaCount,
		sess.MissingVerificationCount, sess.DuplicatePromptCount,
		sess.NoCodeContextCount, sess.RunawayToolLoopCount,
		sess.DataVersion,
		sess.Cwd, sess.GitBranch, sess.SourceSessionID,
		sess.SourceVersion, sess.TranscriptFidelity, sess.ParserMalformedLines,
		sess.IsTruncated, nilTime(sess.DeletedAt),
		timeValue(sess.CreatedAt), nilString(sess.TerminationStatus),
		sess.SecretLeakCount, sess.SecretsRulesVersion,
	}
}

func insertMessages(
	ctx context.Context, exec duckMutationExecutor, msgs []db.Message,
) error {
	for _, m := range msgs {
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO messages (
				id, session_id, ordinal, role, content, thinking_text,
				timestamp, has_thinking, has_tool_use, content_length,
				is_system, model, token_usage, context_tokens, output_tokens,
				has_context_tokens, has_output_tokens, claude_message_id,
				claude_request_id, source_type, source_subtype, source_uuid,
				source_parent_uuid, is_sidechain, is_compact_boundary
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.SessionID, m.Ordinal, m.Role, m.Content,
			m.ThinkingText, timeValue(m.Timestamp),
			m.HasThinking, m.HasToolUse, m.ContentLength,
			m.IsSystem, m.Model, string(m.TokenUsage),
			m.ContextTokens, m.OutputTokens,
			m.HasContextTokens, m.HasOutputTokens,
			m.ClaudeMessageID, m.ClaudeRequestID,
			m.SourceType, m.SourceSubtype, m.SourceUUID,
			m.SourceParentUUID, m.IsSidechain, m.IsCompactBoundary,
		); err != nil {
			return fmt.Errorf("inserting duckdb message %s/%d: %w", m.SessionID, m.Ordinal, err)
		}
	}
	return nil
}

func (s *Sync) replaceToolRows(
	ctx context.Context, exec duckMutationExecutor, sessionID string, msgs []db.Message,
) error {
	if err := s.execMutation(ctx, exec,
		`DELETE FROM tool_result_events WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb tool_result_events for %s: %w", sessionID, err)
	}
	if err := s.execMutation(ctx, exec,
		`DELETE FROM tool_calls WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb tool_calls for %s: %w", sessionID, err)
	}
	if err := insertToolCalls(ctx, exec, msgs); err != nil {
		return err
	}
	if err := insertToolResultEvents(ctx, exec, msgs); err != nil {
		return err
	}
	return nil
}

func insertToolCalls(
	ctx context.Context, exec duckMutationExecutor, msgs []db.Message,
) error {
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			if _, err := exec.ExecContext(ctx, `
				INSERT INTO tool_calls (
					message_id, session_id, tool_name, category,
					call_index, tool_use_id, input_json, skill_name,
					result_content_length, result_content,
					subagent_session_id, file_path
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				m.ID, m.SessionID, tc.ToolName, tc.Category,
				i, tc.ToolUseID, nilEmpty(tc.InputJSON),
				nilEmpty(tc.SkillName), nilZero(tc.ResultContentLength),
				nilEmpty(tc.ResultContent), nilEmpty(tc.SubagentSessionID),
				nilEmpty(tc.FilePath),
			); err != nil {
				return fmt.Errorf("inserting duckdb tool_call %s/%d/%d: %w",
					m.SessionID, m.Ordinal, i, err)
			}
		}
	}
	return nil
}

func insertToolResultEvents(
	ctx context.Context, exec duckMutationExecutor, msgs []db.Message,
) error {
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			for _, ev := range tc.ResultEvents {
				if _, err := exec.ExecContext(ctx, `
					INSERT INTO tool_result_events (
						session_id, tool_call_message_ordinal, call_index,
						tool_use_id, agent_id, subagent_session_id,
						source, status, content, content_length,
						timestamp, event_index
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					m.SessionID, m.Ordinal, i,
					nilEmpty(ev.ToolUseID), nilEmpty(ev.AgentID),
					nilEmpty(ev.SubagentSessionID), ev.Source, ev.Status,
					ev.Content, ev.ContentLength, timeValue(ev.Timestamp),
					ev.EventIndex,
				); err != nil {
					return fmt.Errorf("inserting duckdb tool_result_event %s/%d/%d: %w",
						m.SessionID, m.Ordinal, i, err)
				}
			}
		}
	}
	return nil
}

func (s *Sync) replaceUsageEvents(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	events, err := s.local.GetUsageEvents(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := s.execMutation(ctx, exec,
		`DELETE FROM usage_events WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb usage_events for %s: %w", sessionID, err)
	}
	for _, ev := range events {
		if err := insertUsageEvent(ctx, exec, ev); err != nil {
			return fmt.Errorf("inserting duckdb usage_event %s: %w", sessionID, err)
		}
	}
	return nil
}

func insertUsageEvent(
	ctx context.Context, exec duckMutationExecutor, ev db.UsageEvent,
) error {
	ordinal, cost, occurredAt := usageEventNullableValues(ev)
	if _, err := exec.ExecContext(ctx, `
		INSERT INTO usage_events (
			id, session_id, message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.SessionID, ordinal, ev.Source, ev.Model,
		ev.InputTokens, ev.OutputTokens,
		ev.CacheCreationInputTokens, ev.CacheReadInputTokens,
		ev.ReasoningTokens, cost, ev.CostStatus,
		ev.CostSource, occurredAt, ev.DedupKey,
	); err != nil {
		return err
	}
	return nil
}

func (s *Sync) bulkInsertCursorUsageEvents(
	ctx context.Context, tx *sql.Tx, events []db.CursorUsageEvent,
) error {
	if len(events) == 0 {
		return nil
	}
	const cursorBatch = 100
	for i := 0; i < len(events); i += cursorBatch {
		end := min(i+cursorBatch, len(events))
		batch := events[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO cursor_usage_events (
			occurred_at, model, kind,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_cents, cursor_token_fee,
			user_id, user_email, is_headless, dedup_key
		) VALUES `)
		args := make([]any, 0, len(batch)*13)
		for j, ev := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			occurredAt, ok := parseTimestamp(ev.OccurredAt)
			if !ok {
				return fmt.Errorf("parsing cursor usage occurred_at %q", ev.OccurredAt)
			}
			args = append(args,
				occurredAt,
				db.SanitizeUTF8(ev.Model),
				db.SanitizeUTF8(ev.Kind),
				ev.InputTokens,
				ev.OutputTokens,
				ev.CacheWriteTokens,
				ev.CacheReadTokens,
				ev.ChargedCents,
				ev.CursorTokenFee,
				db.SanitizeUTF8(ev.UserID),
				db.SanitizeUTF8(ev.UserEmail),
				ev.IsHeadless,
				db.SanitizeUTF8(ev.DedupKey),
			)
		}
		b.WriteString(` ON CONFLICT DO NOTHING`)
		if err := s.execMutation(ctx, tx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting duckdb cursor_usage_events: %w", err)
		}
	}
	return nil
}

func usageEventNullableValues(ev db.UsageEvent) (any, any, any) {
	var ordinal any
	if ev.MessageOrdinal != nil {
		ordinal = *ev.MessageOrdinal
	}
	var cost any
	if ev.CostUSD != nil {
		cost = *ev.CostUSD
	}
	var occurredAt any
	if ev.OccurredAt != "" {
		occurredAt = timeValue(ev.OccurredAt)
	}
	return ordinal, cost, occurredAt
}

func (s *Sync) replaceSecretFindings(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	findings, err := s.local.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, f := range findings {
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO secret_findings (
				session_id, rule_name, confidence, location_kind,
				message_ordinal, call_index, event_index,
				match_start, match_end, match_index,
				redacted_match, rules_version, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, current_timestamp)`,
			f.SessionID, f.RuleName, f.Confidence, f.LocationKind,
			f.MessageOrdinal, f.CallIndex, f.EventIndex,
			f.MatchStart, f.MatchEnd, f.MatchIndex,
			f.RedactedMatch, f.RulesVersion,
		); err != nil {
			return fmt.Errorf("inserting duckdb secret_finding %s: %w", sessionID, err)
		}
	}
	return nil
}

func (s *Sync) replacePinnedMessages(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	pins, err := s.local.ListPinnedMessages(ctx, sessionID, "")
	if err != nil {
		return err
	}
	for _, p := range pins {
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO pinned_messages (
				id, session_id, message_id, ordinal, note, created_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			p.ID, p.SessionID, p.MessageID, p.Ordinal,
			p.Note, timeValue(p.CreatedAt),
		); err != nil {
			return fmt.Errorf("inserting duckdb pinned_message %s/%d: %w",
				sessionID, p.MessageID, err)
		}
	}
	return nil
}

func (s *Sync) replaceAllPinnedMessages(
	ctx context.Context, tx *sql.Tx, sessions []db.Session,
) error {
	if err := s.execMutation(ctx, tx, `
		DELETE FROM pinned_messages
		WHERE session_id IN (
			SELECT id FROM sessions WHERE machine = ?
		)`, s.machine); err != nil {
		return fmt.Errorf("clearing duckdb pinned_messages: %w", err)
	}
	for _, sess := range sessions {
		if err := s.replacePinnedMessages(ctx, tx, sess.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) replaceScopedPinnedMessages(
	ctx context.Context, tx *sql.Tx, sessions []db.Session,
) error {
	for _, sess := range sessions {
		if err := s.execMutation(ctx, tx,
			`DELETE FROM pinned_messages WHERE session_id = ?`, sess.ID,
		); err != nil {
			return fmt.Errorf("clearing duckdb pinned_messages for %s: %w", sess.ID, err)
		}
		if err := s.replacePinnedMessages(ctx, tx, sess.ID); err != nil {
			return err
		}
	}
	return nil
}

func nilString(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}

func nilEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nilZero(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nilTime(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return timeValue(*value)
}

func timeValue(value string) any {
	if value == "" {
		return nil
	}
	if t, ok := parseTimestamp(value); ok {
		return t
	}
	return value
}

func parseTimestamp(value string) (time.Time, bool) {
	candidates := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	value = strings.TrimSpace(value)
	for _, layout := range candidates {
		t, err := time.Parse(layout, value)
		if err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
