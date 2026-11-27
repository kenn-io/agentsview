package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"go.kenn.io/agentsview/internal/db"
)

// errPushNewSessionBatch reports that a chunk's batched write of new sessions
// failed or met a session it could not create. pushBatch then rewrites the
// chunk one statement at a time, which handles each session's errors.
var errPushNewSessionBatch = errors.New("batched write of new sessions")

// newSessionBatchFlushBytes bounds the statement text and arguments a batch
// holds in memory before it is sent. Sending early stays inside the chunk's
// transaction.
const newSessionBatchFlushBytes = 8 << 20

// pgBatchExecer queues statements on the transaction's connection and sends
// them together in one pipelined round trip.
type pgBatchExecer struct {
	conn  *sql.Conn
	batch *pgx.Batch
	bytes int
	tags  []pgconn.CommandTag
}

// ExecContext queues the statement, and sends the batch once it holds
// newSessionBatchFlushBytes; statement errors surface when the batch is sent.
// The result reports zero affected rows because the statement may not have
// run. Only writes for sessions PostgreSQL has no row for are queued, and the
// writers read affected rows only from DELETEs on tables that reference
// sessions, where such a session has nothing to delete, so zero is also what
// those statements report once they run.
func (e *pgBatchExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	e.batch.Queue(query, args...)
	e.bytes += len(query)
	for _, arg := range args {
		switch v := arg.(type) {
		case string:
			e.bytes += len(v)
		case []byte:
			e.bytes += len(v)
		default:
			e.bytes += 8
		}
	}
	if e.bytes >= newSessionBatchFlushBytes {
		if err := e.send(ctx); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(0), nil
}

// position returns the index the next queued statement's tag will have.
func (e *pgBatchExecer) position() int {
	return len(e.tags) + e.batch.Len()
}

// send runs the queued statements in one round trip and records their tags.
func (e *pgBatchExecer) send(ctx context.Context) error {
	if e.batch.Len() == 0 {
		return nil
	}
	queued := e.batch
	e.batch, e.bytes = &pgx.Batch{}, 0
	return e.conn.Raw(func(driverConn any) (err error) {
		// A panic escaping Raw would make database/sql close the
		// connection while the transaction still holds it, which blocks
		// forever; report it as an error instead.
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("sending pg batch: panic: %v", r)
			}
		}()
		conn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected pg driver connection %T", driverConn)
		}
		results := conn.Conn().SendBatch(ctx, queued)
		for range queued.Len() {
			tag, err := results.Exec()
			if err != nil {
				_ = results.Close()
				return err
			}
			e.tags = append(e.tags, tag)
		}
		return results.Close()
	})
}

// newSessionWrite is a session the batch created.
type newSessionWrite struct {
	session  db.Session
	upsert   int
	messages int
}

// newSessionBatch writes a chunk's sessions that PostgreSQL has no row for
// with pipelined statements. A new session has no child rows and no pins to
// preserve, so the only reads it needs are made once for the whole chunk.
// Sessions stay in chunk order: the caller sends the queued statements before
// writing any other session one statement at a time, because a session's
// upsert can read rows an earlier session wrote, such as a Codex thread's
// trash state. Every error is wrapped in errPushNewSessionBatch, and the
// caller then rolls back the chunk.
type newSessionBatch struct {
	sync                 *Sync
	exec                 *pgBatchExecer
	created              map[string]bool
	published            map[string]map[publishedCommitLink]struct{}
	markerID             string
	legacyMarkerMachines []string
	externalParents      map[string]string
	writes               []newSessionWrite
}

// prepareNewSessionBatch chooses the chunk's new sessions. It returns nil
// when there are none or the archive is usage-only, which keeps every session
// on the sequential path.
func (s *Sync) prepareNewSessionBatch(
	ctx context.Context, conn *sql.Conn, tx *sql.Tx, batch []db.Session,
	comparisons *pushMessageComparison,
	markerID string, legacyMarkerMachines []string, externalParents map[string]string,
) (*newSessionBatch, error) {
	if s.local.ArchiveContent().UsageOnly() {
		return nil, nil
	}
	created, err := s.chooseNewSessions(ctx, tx, batch, comparisons, markerID, legacyMarkerMachines)
	if err != nil || len(created) == 0 {
		return nil, wrapNewSessionBatchErr(err)
	}
	ids := make([]string, 0, len(created))
	for id := range created {
		ids = append(ids, id)
	}
	published, err := publishedCommitLinks(ctx, tx, ids)
	if err != nil {
		return nil, wrapNewSessionBatchErr(fmt.Errorf("reading published commit links: %w", err))
	}
	if s.beforeNewSessionBatch != nil {
		s.beforeNewSessionBatch()
	}
	return &newSessionBatch{
		sync: s, exec: &pgBatchExecer{conn: conn, batch: &pgx.Batch{}},
		created: created, published: published, markerID: markerID,
		legacyMarkerMachines: legacyMarkerMachines, externalParents: externalParents,
	}, nil
}

// has reports whether sess is written by the batch.
func (b *newSessionBatch) has(sess db.Session) bool {
	return b.created[sess.ID]
}

// queue adds sess's writes.
func (b *newSessionBatch) queue(ctx context.Context, sess db.Session) error {
	write, err := b.sync.queueNewSession(ctx, b.exec, sess, b.markerID, b.legacyMarkerMachines, b.externalParents[sess.ID], b.published[sess.ID])
	if err != nil {
		return wrapNewSessionBatchErr(fmt.Errorf("session %s: %w", sess.ID, err))
	}
	b.writes = append(b.writes, write)
	return nil
}

// send runs the queued statements.
func (b *newSessionBatch) send(ctx context.Context) error {
	if b == nil {
		return nil
	}
	return wrapNewSessionBatchErr(b.exec.send(ctx))
}

// finish sends the remaining statements and confirms that the batch wrote
// only new sessions: each insert-only upsert created its row, so no other
// push created it after the sessions were chosen, and no exclusion applies to
// any of the session's tombstone IDs, which the sequential path rechecks after
// its upsert. The exclusion check rides in the last batch. finish returns the
// sessions the batch wrote.
func (b *newSessionBatch) finish(ctx context.Context) ([]newSessionWrite, error) {
	if b == nil || len(b.writes) == 0 {
		return nil, b.send(ctx)
	}
	sessions := make([]db.Session, len(b.writes))
	for i, write := range b.writes {
		sessions[i] = write.session
	}
	excludedQuery, excludedArgs := pgExcludedSessionIDsQuery(slices.Values(sessions), b.markerID, b.sync.machine, b.legacyMarkerMachines)
	excluded := b.exec.position()
	if _, err := b.exec.ExecContext(ctx, excludedQuery, excludedArgs...); err != nil {
		return nil, wrapNewSessionBatchErr(err)
	}
	if err := b.send(ctx); err != nil {
		return nil, err
	}
	for _, write := range b.writes {
		if b.exec.tags[write.upsert].RowsAffected() != 1 {
			return nil, wrapNewSessionBatchErr(fmt.Errorf("session %s was not created", write.session.ID))
		}
	}
	if b.exec.tags[excluded].RowsAffected() != 0 {
		return nil, wrapNewSessionBatchErr(errors.New("an exclusion applies to a batched session"))
	}
	return b.writes, nil
}

func wrapNewSessionBatchErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errPushNewSessionBatch, err)
}

// chooseNewSessions returns the IDs of the batch's sessions that PostgreSQL
// has no row for and no exclusion applies to. The comparison preload, when
// present, already read the friction columns of every existing session, so
// incremental pushes need no extra query.
func (s *Sync) chooseNewSessions(
	ctx context.Context, tx *sql.Tx, batch []db.Session,
	comparisons *pushMessageComparison,
	markerID string, legacyMarkerMachines []string,
) (map[string]bool, error) {
	var existing map[string]bool
	if comparisons != nil {
		existing = make(map[string]bool, len(comparisons.Friction))
		for id := range comparisons.Friction {
			existing[id] = true
		}
	} else {
		ids := make([]string, len(batch))
		for i, sess := range batch {
			ids[i] = sess.ID
		}
		var err error
		existing, err = existingPGSessionIDs(ctx, tx, ids)
		if err != nil {
			return nil, err
		}
	}
	candidates := slices.DeleteFunc(slices.Clone(batch), func(sess db.Session) bool { return existing[sess.ID] })
	excluded, err := readPGExcludedSessionIDs(ctx, tx, slices.Values(candidates), markerID, s.machine, legacyMarkerMachines)
	if err != nil {
		return nil, err
	}
	created := make(map[string]bool, len(candidates))
	for _, sess := range candidates {
		if !slices.ContainsFunc(pgSessionTombstoneIDs(sess), func(id string) bool {
			_, ok := excluded[id]
			return ok
		}) {
			created[sess.ID] = true
		}
	}
	return created, nil
}

func existingPGSessionIDs(ctx context.Context, tx *sql.Tx, ids []string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("reading existing sessions: %w", err)
	}
	defer rows.Close()
	existing := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning existing session: %w", err)
		}
		existing[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading existing sessions: %w", err)
	}
	return existing, nil
}

// queueNewSession queues one new session's writes: the session row, its
// aliases, usage events, transcript, findings, commit links, image text,
// plan usage and recorded total.
func (s *Sync) queueNewSession(
	ctx context.Context, exec *pgBatchExecer, sess db.Session,
	markerID string, legacyMarkerMachines []string, externalParent string,
	publishedLinks map[publishedCommitLink]struct{},
) (newSessionWrite, error) {
	imageRows, err := s.local.SessionImageTextRows(ctx, sess.ID)
	if err != nil {
		return newSessionWrite{}, fmt.Errorf("reading local image text: %w", err)
	}
	mirrored := sess
	mirrored.DataVersion = db.MirrorDataVersion(sess.DataVersion, len(imageRows) > 0)
	query, args, err := pgSessionUpsert(mirrored, markerID, legacyMarkerMachines, pgSessionWriteOptions{
		ExternalParent: externalParent,
		Machine:        s.machine, ArchiveID: s.archiveID, DatabaseGeneration: s.databaseGeneration,
		InsertOnly: true,
	})
	if err != nil {
		return newSessionWrite{}, err
	}
	write := newSessionWrite{session: sess, upsert: exec.position()}
	if _, err := exec.ExecContext(ctx, query, args...); err != nil {
		return newSessionWrite{}, err
	}
	if err := replacePGSessionAliases(ctx, exec, sess); err != nil {
		return newSessionWrite{}, err
	}
	count, messagesChanged, err := s.queueNewSessionTranscript(ctx, exec, sess.ID)
	if err != nil {
		return newSessionWrite{}, err
	}
	write.messages = count
	findingsChanged, err := s.pushSecretFindings(ctx, exec, sess.ID)
	if err != nil {
		return newSessionWrite{}, err
	}
	linkRows, err := s.local.SessionCommitLinkRows(ctx, sess.ID)
	if err != nil {
		return newSessionWrite{}, err
	}
	// Write the links even when the preload matched: commit links can exist
	// without their session, and another push can change them after the
	// preload. The comparison decides only whether the session changed.
	linksChanged := !commitLinksMatch(publishedLinks, linkRows)
	if err := writeCommitLinks(ctx, exec, sess.ID, linkRows); err != nil {
		return newSessionWrite{}, err
	}
	imageTextChanged, err := replaceImageText(ctx, exec, sess.ID, imageRows, nil)
	if err != nil {
		return newSessionWrite{}, err
	}
	frictionChanged, err := s.pushFrictionFindings(ctx, exec, sess.ID)
	if err != nil {
		return newSessionWrite{}, err
	}
	planUsageChanged, err := s.replacePlanUsage(ctx, exec, sess.ID)
	if err != nil {
		return newSessionWrite{}, err
	}
	sessionTotalChanged, err := s.replaceSessionUsageTotal(ctx, exec, sess.ID)
	if err != nil {
		return newSessionWrite{}, err
	}
	// The sequential path publishes these changes to read-mode watchers by
	// advancing updated_at; keep the same statement so session triggers see
	// the same writes.
	if messagesChanged || findingsChanged || linksChanged || imageTextChanged ||
		frictionChanged || planUsageChanged || sessionTotalChanged {
		if _, err := exec.ExecContext(ctx, `UPDATE sessions SET updated_at = NOW() WHERE id = $1`, sess.ID); err != nil {
			return newSessionWrite{}, err
		}
	}
	return write, nil
}

// queueNewSessionTranscript queues a new session's usage events and its whole
// transcript. Usage events and transcript pages come from one archive
// snapshot, as on the sequential path.
func (s *Sync) queueNewSessionTranscript(
	ctx context.Context, exec *pgBatchExecer, sessionID string,
) (count int, changed bool, err error) {
	snapshot, err := s.local.BeginMessageSnapshot(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	if s.afterMessageSnapshot != nil {
		s.afterMessageSnapshot()
	}
	usageChanged, err := replaceUsageEvents(ctx, exec, snapshot, sessionID)
	if err != nil {
		return 0, false, err
	}
	count, changed, err = s.copySessionMessages(ctx, exec, sessionID, snapshot, 0)
	if err != nil {
		return 0, false, err
	}
	return count, changed || usageChanged, nil
}
