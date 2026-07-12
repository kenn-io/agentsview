package duckdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

const projectIdentityPublicationStateKey = "duckdb_project_identity_publication_revision_v2"

func (s *Sync) projectIdentityStateKey(databaseGeneration string) string {
	key := projectIdentityPublicationStateKey + ":" + databaseGeneration
	if !s.isFiltered() {
		return s.syncStateKey(key)
	}
	projects := append([]string(nil), s.projects...)
	excludes := append([]string(nil), s.excludeProjects...)
	sort.Strings(projects)
	sort.Strings(excludes)
	projects = slices.Compact(projects)
	excludes = slices.Compact(excludes)

	sum := sha256.New()
	writeField := func(value string) {
		_, _ = fmt.Fprintf(sum, "%d:", len(value))
		_, _ = sum.Write([]byte(value))
	}
	writeField("include")
	writeField(strconv.Itoa(len(projects)))
	for _, project := range projects {
		writeField(project)
	}
	writeField("exclude")
	writeField(strconv.Itoa(len(excludes)))
	for _, project := range excludes {
		writeField(project)
	}
	return s.syncStateKey(
		key + ":project-filter:" + hex.EncodeToString(sum.Sum(nil)[:8]),
	)
}

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

func (s *Sync) syncProjectIdentityObservations(
	ctx context.Context, force bool,
) error {
	revision, err := s.local.ProjectIdentityPublicationRevision(ctx)
	if err != nil {
		return err
	}
	databaseGeneration, err := s.local.GetDatabaseID(ctx)
	if err != nil {
		return fmt.Errorf("loading source database generation: %w", err)
	}
	revisionValue := strconv.FormatInt(revision, 10)
	stateKey := s.projectIdentityStateKey(databaseGeneration)
	publishedRevisionValue, err := s.local.GetSyncState(stateKey)
	if err != nil {
		return fmt.Errorf("reading duckdb project identity publication revision: %w", err)
	}
	archiveID, err := s.local.GetArchiveID(ctx)
	if err != nil {
		return fmt.Errorf("loading source archive id: %w", err)
	}
	fullPublication := force || publishedRevisionValue == ""
	var publishedRevision int64
	if !fullPublication {
		publishedRevision, err = strconv.ParseInt(publishedRevisionValue, 10, 64)
		if err != nil || publishedRevision < 0 || publishedRevision > revision {
			fullPublication = true
		} else if publishedRevision == revision {
			var archivePresent bool
			if err := s.duck.QueryRowContext(ctx, `
			SELECT count(*) > 0 FROM source_archives
			WHERE source_archive_id = ?`, archiveID).Scan(&archivePresent); err != nil {
				return fmt.Errorf("checking duckdb project identity publication: %w", err)
			}
			if archivePresent {
				return nil
			}
			fullPublication = true
		}
	}

	var observations []export.ProjectIdentityObservation
	var snapshots []export.ProjectIdentityObservation
	var delta db.ProjectIdentityPublicationDelta
	if fullPublication {
		observations, err = s.local.ListProjectIdentityObservations(ctx, nil)
		if err != nil {
			return fmt.Errorf("loading project identity observations: %w", err)
		}
		if len(s.projects) > 0 || len(s.excludeProjects) > 0 {
			out := observations[:0]
			for _, obs := range observations {
				if projectInSyncScope(obs.Project, s.projects, s.excludeProjects) {
					out = append(out, obs)
				}
			}
			observations = out
		}
		snapshots, err = s.local.ListSessionProjectIdentitySnapshots(ctx)
		if err != nil {
			return fmt.Errorf("loading session project identity snapshots: %w", err)
		}
		if len(s.projects) > 0 || len(s.excludeProjects) > 0 {
			out := snapshots[:0]
			for _, snapshot := range snapshots {
				if projectInSyncScope(
					snapshot.Project, s.projects, s.excludeProjects,
				) {
					out = append(out, snapshot)
				}
			}
			snapshots = out
		}
	} else {
		delta, err = s.local.LoadProjectIdentityPublicationDelta(
			ctx, publishedRevision, revision, s.projects, s.excludeProjects,
		)
		if err != nil {
			return err
		}
		observations = delta.Observations
		snapshots = delta.Snapshots
	}
	archiveSalt, err := s.local.GetArchiveSalt(ctx)
	if err != nil {
		return fmt.Errorf("loading source archive salt: %w", err)
	}
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb project identity sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := upsertSourceArchiveScope(
		func(stmt string, args ...any) error {
			return s.execMutation(ctx, tx, stmt, args...)
		},
		func(stmt string, args ...any) *sql.Row {
			return tx.QueryRowContext(ctx, stmt, args...)
		},
		archiveID, archiveSalt,
	); err != nil {
		return err
	}
	execDelta := func(stmt string, args ...any) error {
		return s.execMutation(ctx, tx, stmt, args...)
	}
	if fullPublication {
		if err := deleteProjectIdentityScope(
			execDelta, archiveID, s.projects, s.excludeProjects,
		); err != nil {
			return err
		}
	} else if err := deleteProjectIdentityDelta(
		execDelta, archiveID, databaseGeneration,
		delta.ObservationDeletes, delta.SnapshotDeletes,
	); err != nil {
		return err
	}
	for _, obs := range observations {
		obs.SourceArchiveID = archiveID
		obs.SourceArchiveSalt = archiveSalt
		obs = export.SanitizeStoredProjectIdentityObservation(obs)
		if err := upsertProjectIdentityObservation(
			func(stmt string, args ...any) error {
				return s.execMutation(ctx, tx, stmt, args...)
			},
			func(stmt string, args ...any) *sql.Row {
				return tx.QueryRowContext(ctx, stmt, args...)
			},
			obs, "",
		); err != nil {
			return fmt.Errorf(
				"syncing duckdb project identity observation %s/%s/%s: %w",
				obs.Project, obs.Machine, obs.RootPath, err,
			)
		}
	}
	for i := range snapshots {
		snapshots[i] = export.SanitizeStoredProjectIdentityObservation(snapshots[i])
	}
	if err := upsertSessionProjectIdentitySnapshots(
		func(stmt string, args ...any) error {
			return s.execMutation(ctx, tx, stmt, args...)
		},
		archiveID, databaseGeneration, snapshots,
	); err != nil {
		return fmt.Errorf("syncing duckdb session project identity snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb project identity sync: %w", err)
	}
	if err := s.local.SetSyncState(stateKey, revisionValue); err != nil {
		return fmt.Errorf("recording duckdb project identity publication revision: %w", err)
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
	ctx context.Context,
	exec duckMutationExecutor,
	target duckQueryer,
	sess db.Session,
	full bool,
) (int, error) {
	if err := s.upsertSession(ctx, exec, sess); err != nil {
		return 0, err
	}
	msgs, err := s.local.GetAllMessages(ctx, sess.ID)
	if err != nil {
		return 0, fmt.Errorf("reading local messages for %s: %w", sess.ID, err)
	}

	if !full {
		action, err := s.duckMessagePushAction(ctx, target, sess.ID, msgs)
		if err != nil {
			return 0, err
		}
		switch action.kind {
		case duckMessagePushSkip:
			if err := s.replaceSessionSecretFindings(ctx, exec, sess.ID); err != nil {
				return 0, err
			}
			if err := s.replaceSessionPinnedMessages(ctx, exec, sess.ID); err != nil {
				return 0, err
			}
			return 0, nil
		case duckMessagePushAppend:
			suffix := messagesAfterDuckOrdinal(msgs, action.maxOrdinal)
			if len(suffix) == 0 {
				return 0, fmt.Errorf(
					"duckdb append %s: no local suffix after ordinal %d",
					sess.ID, action.maxOrdinal,
				)
			}
			if err := s.replaceUsageEvents(ctx, exec, sess.ID); err != nil {
				return 0, err
			}
			if err := insertMessages(ctx, exec, suffix); err != nil {
				return 0, err
			}
			if err := insertToolCalls(ctx, exec, suffix); err != nil {
				return 0, err
			}
			if err := insertToolResultEvents(ctx, exec, suffix); err != nil {
				return 0, err
			}
			if err := s.replaceSessionSecretFindings(ctx, exec, sess.ID); err != nil {
				return 0, err
			}
			if err := s.replaceSessionPinnedMessages(ctx, exec, sess.ID); err != nil {
				return 0, err
			}
			return len(suffix), nil
		case duckMessagePushReplace:
		default:
			return 0, fmt.Errorf("unknown duckdb message push action %d", action.kind)
		}
	}

	if err := s.replaceSessionDependents(ctx, exec, sess.ID); err != nil {
		return 0, err
	}
	if err := s.replaceUsageEvents(ctx, exec, sess.ID); err != nil {
		return 0, err
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
) ([]string, map[string]bool, error) {
	localIDs := make(map[string]bool, len(localSessions))
	for _, sess := range localSessions {
		localIDs[sess.ID] = true
	}
	mirrorIDs := make(map[string]bool)
	rows, err := tx.QueryContext(ctx,
		`SELECT id, project FROM sessions WHERE machine = ?`,
		machine,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("listing duckdb sessions for deletion reconciliation: %w", err)
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, nil, fmt.Errorf("scanning duckdb session for deletion reconciliation: %w", err)
		}
		mirrorIDs[id] = true
		if !projectInSyncScope(project, projects, excludeProjects) {
			continue
		}
		if !localIDs[id] {
			stale = append(stale, id)
			delete(mirrorIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Strings(stale)
	for _, id := range stale {
		if err := s.deleteMirrorSession(ctx, tx, id); err != nil {
			return nil, nil, err
		}
	}
	return stale, mirrorIDs, nil
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

const (
	duckRemoteMutationCoalesceMaxBytes = 2 << 20
	duckRemoteInsertCoalesceMaxRows    = 256
	duckRemoteInsertCoalesceMaxBytes   = 1 << 20
)

type duckRemoteMutationBatch struct {
	statements              []string
	renderedValid           bool
	renderedStatementsCache []duckRemoteRenderedStatement
	renderedStatementBytes  int
}

type duckRemoteRenderedStatement struct {
	sql        string
	start, end int
}

func (b *duckRemoteMutationBatch) ExecContext(
	_ context.Context, stmt string, args ...any,
) (sql.Result, error) {
	sqlText, err := duckSQLWithArgs(stmt, args...)
	if err != nil {
		return nil, err
	}
	b.statements = append(b.statements, sqlText)
	b.invalidateRendered()
	return duckNoopResult{}, nil
}

func (b *duckRemoteMutationBatch) Len() int {
	return len(b.statements)
}

func (b *duckRemoteMutationBatch) appendBatch(other *duckRemoteMutationBatch) {
	if other == nil || other.Len() == 0 {
		return
	}
	oldLen := len(b.statements)
	preserveRendered := b.renderedValid
	rendered := b.renderedStatementsCache
	renderedBytes := b.renderedStatementBytes
	b.statements = append(b.statements, other.statements...)
	if !preserveRendered {
		b.invalidateRendered()
		return
	}
	for _, stmt := range other.rendered() {
		stmt.start += oldLen
		stmt.end += oldLen
		rendered = append(rendered, stmt)
		renderedBytes += len(stmt.sql)
	}
	b.renderedStatementsCache = rendered
	b.renderedStatementBytes = renderedBytes
	b.renderedValid = true
}

func (b *duckRemoteMutationBatch) invalidateRendered() {
	b.renderedValid = false
	b.renderedStatementsCache = nil
	b.renderedStatementBytes = 0
}

type duckNoopResult struct{}

func (duckNoopResult) LastInsertId() (int64, error) { return 0, nil }
func (duckNoopResult) RowsAffected() (int64, error) { return 0, nil }

func (s *Sync) execRemoteMutationBatch(
	ctx context.Context, label string, batch *duckRemoteMutationBatch,
) error {
	return execDuckRemoteMutationBatch(
		ctx, s.execRemoteSQLRetry, label, batch, true,
	)
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
	nextBytes := next.transactionBytes()
	if nextBytes > maxBytes {
		if err := execDuckRemoteMutationBatch(
			ctx, execCoalesced, label, current, true,
		); err != nil {
			return current, err
		}
		return &duckRemoteMutationBatch{}, fmt.Errorf(
			"duckdb remote mutation batch exceeds remote mutation coalesce budget: %d > %d",
			nextBytes, maxBytes,
		)
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
	return execDuckRemoteMutationStatements(ctx, exec, label, batch.statements)
}

func execDuckRemoteMutationRenderedStatements(
	ctx context.Context,
	exec func(context.Context, string) error,
	label string,
	batch *duckRemoteMutationBatch,
) error {
	if batch.Len() == 0 {
		return nil
	}
	rendered := batch.rendered()
	statements := make([]string, 0, len(rendered))
	for _, stmt := range rendered {
		statements = append(statements, stmt.sql)
	}
	return execDuckRemoteMutationStatements(ctx, exec, label, statements)
}

func execDuckRemoteMutationStatements(
	ctx context.Context,
	exec func(context.Context, string) error,
	label string,
	statements []string,
) (err error) {
	if len(statements) == 0 {
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
	for i, stmt := range statements {
		if err := exec(ctx, stmt); err != nil {
			return fmt.Errorf(
				"execute %s statement %d/%d: %w",
				label, i+1, len(statements), err,
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

func execDuckRemoteMutationBatchOversizeWithStatementFallback(
	ctx context.Context,
	execCoalesced func(context.Context, string) error,
	execStatement func(context.Context, string) error,
	label string,
	batch *duckRemoteMutationBatch,
	maxBytes int,
) error {
	if maxBytes <= 0 {
		maxBytes = duckRemoteMutationCoalesceMaxBytes
	}
	if batch.transactionBytes() > maxBytes {
		return execDuckRemoteMutationRenderedStatements(
			ctx, execStatement, label, batch,
		)
	}
	return execDuckRemoteMutationBatchWithStatementFallback(
		ctx, execCoalesced, execStatement, label, batch,
	)
}

func (b *duckRemoteMutationBatch) transactionSQL() string {
	var sqlText strings.Builder
	statements := b.rendered()
	sqlText.WriteString("BEGIN TRANSACTION;\n")
	for _, stmt := range statements {
		sqlText.WriteString(stmt.sql)
		sqlText.WriteString(";\n")
	}
	sqlText.WriteString("COMMIT")
	return sqlText.String()
}

func (b *duckRemoteMutationBatch) transactionBytes() int {
	if b == nil || b.Len() == 0 {
		return 0
	}
	statements := b.rendered()
	return duckRemoteTransactionBytesFor(
		b.renderedStatementBytes, len(statements),
	)
}

func duckRemoteTransactionBytesFor(statementBytes, statementCount int) int {
	if statementCount == 0 {
		return 0
	}
	return len("BEGIN TRANSACTION;\n") +
		statementBytes +
		len(";\n")*statementCount +
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
	left := b.rendered()
	right := other.rendered()
	return duckRemoteTransactionBytesFor(
		b.renderedStatementBytes+other.renderedStatementBytes,
		len(left)+len(right),
	)
}

func (b *duckRemoteMutationBatch) rendered() []duckRemoteRenderedStatement {
	if b == nil || b.Len() == 0 {
		return nil
	}
	if b.renderedValid {
		return b.renderedStatementsCache
	}
	b.renderedStatementsCache = renderDuckRemoteMutationStatements(
		b.statements,
		duckRemoteInsertCoalesceMaxRows,
		duckRemoteInsertCoalesceMaxBytes,
	)
	b.renderedStatementBytes = 0
	for _, stmt := range b.renderedStatementsCache {
		b.renderedStatementBytes += len(stmt.sql)
	}
	b.renderedValid = true
	return b.renderedStatementsCache
}

func renderDuckRemoteMutationStatements(
	statements []string, maxRows int, maxBytes int,
) []duckRemoteRenderedStatement {
	if len(statements) == 0 {
		return nil
	}
	if maxRows <= 0 {
		maxRows = duckRemoteInsertCoalesceMaxRows
	}
	if maxBytes <= 0 {
		maxBytes = duckRemoteInsertCoalesceMaxBytes
	}
	out := make([]duckRemoteRenderedStatement, 0, len(statements))
	var pending duckRemoteInsertGroup
	hasPending := false
	flush := func() {
		if !hasPending {
			return
		}
		out = append(out, pending.RenderedStatement())
		pending = duckRemoteInsertGroup{}
		hasPending = false
	}
	for i, stmt := range statements {
		prefix, tuple, ok := splitDuckRemoteSimpleInsert(stmt)
		if !ok {
			flush()
			out = append(out, duckRemoteRenderedStatement{
				sql:   stmt,
				start: i,
				end:   i + 1,
			})
			continue
		}
		if !hasPending || pending.prefix != prefix {
			flush()
			pending = duckRemoteInsertGroup{prefix: prefix, start: i}
			hasPending = true
		}
		if pending.rows > 0 && (pending.rows+1 > maxRows ||
			pending.bytes+len(", ")+len(tuple) > maxBytes) {
			flush()
			pending = duckRemoteInsertGroup{prefix: prefix, start: i}
			hasPending = true
		}
		pending.Append(tuple, i+1)
	}
	flush()
	return out
}

type duckRemoteInsertGroup struct {
	prefix string
	tuples []string
	rows   int
	bytes  int
	start  int
	end    int
}

func (g *duckRemoteInsertGroup) Append(tuple string, end int) {
	if g.rows == 0 {
		g.bytes = len(g.prefix)
	} else {
		g.bytes += len(", ")
	}
	g.tuples = append(g.tuples, tuple)
	g.rows++
	g.bytes += len(tuple)
	g.end = end
}

func (g duckRemoteInsertGroup) RenderedStatement() duckRemoteRenderedStatement {
	return duckRemoteRenderedStatement{
		sql:   g.prefix + strings.Join(g.tuples, ", "),
		start: g.start,
		end:   g.end,
	}
}

func splitDuckRemoteSimpleInsert(stmt string) (string, string, bool) {
	trimmed := strings.TrimSpace(stmt)
	if !duckASCIIHasPrefixFold(trimmed, "INSERT INTO ") {
		return "", "", false
	}
	valuesIndex := duckSQLValuesKeywordIndex(trimmed)
	if valuesIndex < 0 {
		return "", "", false
	}
	prefix := strings.TrimSpace(trimmed[:valuesIndex]) + " VALUES "
	rest := strings.TrimSpace(trimmed[valuesIndex+len(" VALUES "):])
	tuple, suffix, ok := duckSQLLeadingParenthesizedExpression(rest)
	if !ok || strings.TrimSpace(suffix) != "" {
		return "", "", false
	}
	return prefix, tuple, true
}

func duckSQLValuesKeywordIndex(sqlText string) int {
	const valuesKeyword = " VALUES "
	for i := len("INSERT INTO "); i+len(valuesKeyword) <= len(sqlText); i++ {
		if duckASCIIEqualFoldAt(sqlText, i, valuesKeyword) {
			return i
		}
	}
	return -1
}

func duckASCIIHasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return duckASCIIEqualFoldAt(s, 0, prefix)
}

func duckASCIIEqualFoldAt(s string, start int, pattern string) bool {
	if start < 0 || start+len(pattern) > len(s) {
		return false
	}
	for i := range len(pattern) {
		if duckASCIIFold(s[start+i]) != duckASCIIFold(pattern[i]) {
			return false
		}
	}
	return true
}

func duckASCIIFold(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - 'a' + 'A'
	}
	return b
}

func duckSQLLeadingParenthesizedExpression(sqlText string) (
	string, string, bool,
) {
	if !strings.HasPrefix(sqlText, "(") {
		return "", "", false
	}
	depth := 0
	for i := 0; i < len(sqlText); {
		switch sqlText[i] {
		case '$':
			delimiter, ok := duckDollarQuoteDelimiterAt(sqlText, i)
			if ok {
				next := strings.Index(sqlText[i+len(delimiter):], delimiter)
				if next < 0 {
					return "", "", false
				}
				i += len(delimiter) + next + len(delimiter)
				continue
			}
		case '\'':
			next, ok := duckSingleQuotedLiteralEnd(sqlText, i)
			if !ok {
				return "", "", false
			}
			i = next
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return sqlText[:i+1], sqlText[i+1:], true
			}
			if depth < 0 {
				return "", "", false
			}
		}
		i++
	}
	return "", "", false
}

func duckDollarQuoteDelimiterAt(sqlText string, start int) (string, bool) {
	if start >= len(sqlText) || sqlText[start] != '$' {
		return "", false
	}
	i := start + 1
	for i < len(sqlText) && duckDollarQuoteTagByte(sqlText[i]) {
		i++
	}
	if i >= len(sqlText) || sqlText[i] != '$' {
		return "", false
	}
	return sqlText[start : i+1], true
}

func duckDollarQuoteTagByte(b byte) bool {
	return b == '_' ||
		('0' <= b && b <= '9') ||
		('A' <= b && b <= 'Z') ||
		('a' <= b && b <= 'z')
}

func duckSingleQuotedLiteralEnd(sqlText string, start int) (int, bool) {
	for i := start + 1; i < len(sqlText); i++ {
		if sqlText[i] != '\'' {
			continue
		}
		if i+1 < len(sqlText) && sqlText[i+1] == '\'' {
			i++
			continue
		}
		return i + 1, true
	}
	return 0, false
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

func (s *Sync) replaceSessionSecretFindings(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	if err := s.execMutation(ctx, exec,
		`DELETE FROM secret_findings WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb secret_findings for %s: %w", sessionID, err)
	}
	return s.replaceSecretFindings(ctx, exec, sessionID)
}

func (s *Sync) replaceSessionPinnedMessages(
	ctx context.Context, exec duckMutationExecutor, sessionID string,
) error {
	if err := s.execMutation(ctx, exec,
		`DELETE FROM pinned_messages WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb pinned_messages for %s: %w", sessionID, err)
	}
	return s.replacePinnedMessages(ctx, exec, sessionID)
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
		if err := s.replaceSessionPinnedMessages(ctx, tx, sess.ID); err != nil {
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
