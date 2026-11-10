package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// A change stream follows three trigger-maintained counters. The conversation
// publication revision covers message and session-state changes. The usage
// change revision covers a session's token, cost, and column changes. The
// conversation usage revision covers a message whose own usage changed without
// a conversation revision, such as token usage a later sync adds to unchanged
// text. The cursor carries all three, bound to one database generation.
const (
	changeStreamPositionVersion = 1
	changeStreamPositionKind    = "changes"
)

type changeStreamPosition struct {
	Version      int    `json:"version"`
	Kind         string `json:"kind"`
	DatabaseID   string `json:"database_id"`
	Conversation int64  `json:"conversation,string"`
	Usage        int64  `json:"usage,string"`
	MessageUsage int64  `json:"message_usage,string"`
}

func (p changeStreamPosition) encode() (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// decodeChangeStreamPosition reports false for any cursor this archive did
// not issue in this format; the caller restarts from the beginning.
func decodeChangeStreamPosition(cursor string) (changeStreamPosition, bool) {
	var position changeStreamPosition
	if cursor == "" {
		return position, false
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return position, false
	}
	if err := json.Unmarshal(data, &position, json.RejectUnknownMembers(true)); err != nil {
		return position, false
	}
	valid := position.Version == changeStreamPositionVersion &&
		position.Kind == changeStreamPositionKind && position.DatabaseID != "" &&
		position.Conversation >= 0 && position.Usage >= 0 && position.MessageUsage >= 0
	return position, valid
}

// ChangeStreamHead identifies the archive and its newest change position.
// LocalMachineID is this installation's machine identity, or empty when the
// archive has none recorded.
type ChangeStreamHead struct {
	ArchiveID      string
	DatabaseID     string
	Head           string
	LocalMachineID string
}

// ChangeStreamUsage is a content-free token and cost total. Cost sums the
// rows that have a price; Priced is true only when every counted row has one,
// so a session without usage reports zeros and Priced false.
type ChangeStreamUsage struct {
	InputTokens         int   `json:"input_tokens"`
	OutputTokens        int   `json:"output_tokens"`
	CacheCreationTokens int   `json:"cache_creation_tokens"`
	CacheReadTokens     int   `json:"cache_read_tokens"`
	CostMicrodollars    int64 `json:"cost_microdollars"`
	Priced              bool  `json:"priced"`
}

// ChangeStreamSession is a session's current state. MachineID identifies the
// machine the session originated on, in the form per-machine reporting
// snapshots use. It is nil when that is unknown: the archive no longer holds
// the session's record, or the session is stored under this installation's
// "local" key and no installation identity is recorded. A deleted or trashed
// session carries only its ID and MachineID.
type ChangeStreamSession struct {
	SessionID      string                   `json:"session_id"`
	MachineID      *string                  `json:"machine_id"`
	Deleted        bool                     `json:"deleted"`
	Agent          string                   `json:"agent"`
	Automation     string                   `json:"automation"`
	Project        *export.ProjectReference `json:"project"`
	Models         []string                 `json:"models"`
	StartedAt      *string                  `json:"started_at"`
	LastActivityAt *string                  `json:"last_activity_at"`
	MessageCount   int                      `json:"message_count"`
	Usage          *ChangeStreamUsage       `json:"usage"`
}

// ChangeStreamMessage is a message's current state without its text.
// MachineID is its session's. A deleted message carries no model, project, or
// usage.
type ChangeStreamMessage struct {
	SessionID string                   `json:"session_id"`
	MessageID string                   `json:"message_id"`
	MachineID *string                  `json:"machine_id"`
	Revision  string                   `json:"revision"`
	Deleted   bool                     `json:"deleted"`
	Role      string                   `json:"role"`
	Timestamp *string                  `json:"timestamp"`
	Model     string                   `json:"model"`
	Project   *export.ProjectReference `json:"project"`
	Usage     *ChangeStreamUsage       `json:"usage"`
}

// ChangeStreamBatch is one bounded read of the changes after a cursor. From
// is the cursor that was read from, or empty when Reset restarts the stream
// from the beginning of the archive. More reports that changes remain past To.
type ChangeStreamBatch struct {
	ArchiveID  string
	DatabaseID string
	From       string
	To         string
	Reset      bool
	More       bool
	Sessions   []ChangeStreamSession
	Messages   []ChangeStreamMessage
}

type changeStreamRevisions struct {
	archiveID    string
	databaseID   string
	conversation int64
	usage        int64
	messageUsage int64
}

func changeStreamRevisionsTx(ctx context.Context, tx *sql.Tx) (changeStreamRevisions, error) {
	var heads changeStreamRevisions
	var err error
	heads.archiveID, err = sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveIDKey, ErrArchiveIDMissing, "archive id")
	if err != nil {
		return heads, err
	}
	heads.databaseID, err = sessionExportMetadataValue(ctx, tx, archiveMetadataDatabaseIDKey, ErrDatabaseIDMissing, "database id")
	if err != nil {
		return heads, err
	}
	heads.conversation, err = archiveJournalRevision(ctx, tx, conversationRevisionKey, "conversation publication")
	if err != nil {
		return heads, err
	}
	heads.usage, err = archiveJournalRevision(ctx, tx, archiveMetadataUsageChangeRevisionKey, "usage change")
	if err != nil {
		return heads, err
	}
	heads.messageUsage, err = archiveJournalRevision(ctx, tx, conversationUsageRevisionKey, "conversation usage")
	return heads, err
}

// ReadChangeStreamHead returns the archive identity and the cursor of its
// newest change.
func (db *DB) ReadChangeStreamHead(ctx context.Context) (ChangeStreamHead, error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ChangeStreamHead{}, err
	}
	defer func() { _ = tx.Rollback() }()
	heads, err := changeStreamRevisionsTx(ctx, tx)
	if err != nil {
		return ChangeStreamHead{}, err
	}
	head, err := changeStreamPosition{
		Version: changeStreamPositionVersion, Kind: changeStreamPositionKind,
		DatabaseID: heads.databaseID, Conversation: heads.conversation, Usage: heads.usage,
		MessageUsage: heads.messageUsage,
	}.encode()
	if err != nil {
		return ChangeStreamHead{}, err
	}
	machines, err := loadMachineIdentities(ctx, tx)
	if err != nil {
		return ChangeStreamHead{}, fmt.Errorf("reading machine identities: %w", err)
	}
	return ChangeStreamHead{
		ArchiveID: heads.archiveID, DatabaseID: heads.databaseID, Head: head, LocalMachineID: machines.local,
	}, tx.Commit()
}

type changeStreamMessageRow struct {
	message ChangeStreamMessage
	ordinal int64
}

// ReadChangeStream returns the changes published after cursor in one read
// snapshot: at most MaxSessionLimit message and session-state changes, at most
// MaxSessionLimit session usage changes, and at most MaxSessionLimit messages
// whose own usage changed without a conversation revision. An empty cursor, one from another
// database generation, or one this archive did not issue restarts from the
// beginning. Every session a change touches is reported with its current
// state, so a message change also refreshes its session's totals. A non-nil
// cache carries session usage from one read to the next; pass the same cache
// for every read of one stream.
func (db *DB) ReadChangeStream(
	ctx context.Context, cursor string, cache *ChangeStreamUsageCache,
) (ChangeStreamBatch, error) {
	// A replay page loads and prices usage for up to a page of sessions, so it
	// waits in the bulk-read lane and leaves a reader free for quick reads.
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	defer release()
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	defer func() { _ = tx.Rollback() }()
	heads, err := changeStreamRevisionsTx(ctx, tx)
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	batch := ChangeStreamBatch{
		ArchiveID: heads.archiveID, DatabaseID: heads.databaseID,
		Sessions: []ChangeStreamSession{}, Messages: []ChangeStreamMessage{},
	}
	position, ok := decodeChangeStreamPosition(cursor)
	if !ok || position.DatabaseID != heads.databaseID ||
		position.Conversation > heads.conversation || position.Usage > heads.usage ||
		position.MessageUsage > heads.messageUsage {
		batch.Reset = true
		// The replay sends every message with its current usage, so no
		// earlier usage change needs its own re-send.
		position = changeStreamPosition{
			Version: changeStreamPositionVersion, Kind: changeStreamPositionKind,
			DatabaseID: heads.databaseID, MessageUsage: heads.messageUsage,
		}
	} else {
		batch.From = cursor
	}

	sessionIDs := map[string]struct{}{}
	// A message whose conversation revision is past the cursor is sent by the
	// conversation pages, now or in a later event, with its current usage.
	resent, messageUsageLast, messageUsageMore, err := readChangeStreamMessageUsageTx(
		ctx, tx, position.MessageUsage, position.Conversation, sessionIDs)
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	var messages changeStreamConversationPage
	conversationMore := false
	if messageUsageMore {
		// Hold the conversation position until the usage backlog drains. A
		// message sent now by the conversation pages would pass the next usage
		// page's revision filter and be sent again with the same state.
		position.MessageUsage = messageUsageLast
	} else {
		position.MessageUsage = heads.messageUsage
		messages, conversationMore, err = readChangeStreamConversationTx(ctx, tx, position.Conversation, sessionIDs)
		if err != nil {
			return ChangeStreamBatch{}, err
		}
		if conversationMore {
			position.Conversation, err = strconv.ParseInt(messages.lastRevision, 10, 64)
			if err != nil {
				return ChangeStreamBatch{}, err
			}
		} else {
			position.Conversation = heads.conversation
		}
	}
	usageLast, usageMore, err := readChangeStreamUsageTx(ctx, tx, position.Usage, sessionIDs)
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	if usageMore {
		position.Usage = usageLast
	} else {
		position.Usage = heads.usage
	}
	batch.More = conversationMore || usageMore || messageUsageMore
	messages.rows = append(messages.rows, resent...)

	ids := make([]string, 0, len(sessionIDs))
	for id := range sessionIDs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	sessions, usage, err := db.readChangeStreamSessionsTx(ctx, tx, heads, ids, cache)
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	byID := make(map[string]int, len(sessions))
	for i := range sessions {
		byID[sessions[i].SessionID] = i
	}
	for _, row := range messages.rows {
		message := row.message
		i, ok := byID[message.SessionID]
		if ok {
			message.MachineID = sessions[i].MachineID
		}
		if !ok || message.Deleted || sessions[i].Deleted {
			message.Model = ""
		} else {
			message.Project = sessions[i].Project
			if session := usage[message.SessionID]; session != nil {
				message.Usage = session.messages[row.ordinal].usage()
			}
		}
		batch.Messages = append(batch.Messages, message)
	}
	batch.Sessions = sessions
	batch.To, err = position.encode()
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	return batch, tx.Commit()
}

type changeStreamConversationPage struct {
	rows         []changeStreamMessageRow
	lastRevision string
}

// readChangeStreamConversationTx reads message and session-state changes in
// publication order, as the conversation export does.
func readChangeStreamConversationTx(
	ctx context.Context, tx *sql.Tx, after int64, sessionIDs map[string]struct{},
) (changeStreamConversationPage, bool, error) {
	var page changeStreamConversationPage
	rows, err := tx.QueryContext(ctx, `SELECT 'message',c.session_id,c.message_id,c.revision,c.role,NULLIF(c.timestamp,''),c.deleted,c.ordinal,
		 CASE WHEN c.removed=0 THEN COALESCE(m.model,'') ELSE '' END
		 FROM conversation_messages c LEFT JOIN messages m ON m.session_id=c.session_id AND m.ordinal=c.ordinal
		 WHERE c.revision > ?
		UNION ALL SELECT 'session',session_id,'',revision,'',NULL,deleted,0,''
		 FROM conversation_session_changes WHERE revision > ?
		ORDER BY 4 LIMIT ?`, after, after, MaxSessionLimit+1)
	if err != nil {
		return page, false, fmt.Errorf("reading conversation changes: %w", err)
	}
	defer rows.Close()
	count := 0
	more := false
	for rows.Next() {
		if count == MaxSessionLimit {
			more = true
			break
		}
		count++
		var kind string
		var revision int64
		var row changeStreamMessageRow
		if err := rows.Scan(&kind, &row.message.SessionID, &row.message.MessageID, &revision,
			&row.message.Role, &row.message.Timestamp, &row.message.Deleted, &row.ordinal, &row.message.Model); err != nil {
			return page, false, err
		}
		page.lastRevision = strconv.FormatInt(revision, 10)
		sessionIDs[row.message.SessionID] = struct{}{}
		if kind == "message" {
			row.message.Revision = page.lastRevision
			page.rows = append(page.rows, row)
		}
	}
	return page, more, errors.Join(rows.Err(), rows.Close())
}

// readChangeStreamMessageUsageTx reads live messages whose own usage changed
// after the cursor, in conversation usage revision order. Each marked row has
// its own revision, so a page can end between any two of them.
func readChangeStreamMessageUsageTx(
	ctx context.Context, tx *sql.Tx, after, conversationAfter int64, sessionIDs map[string]struct{},
) ([]changeStreamMessageRow, int64, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.session_id,c.message_id,c.revision,c.role,NULLIF(c.timestamp,''),c.deleted,c.ordinal,
		 COALESCE(m.model,''),c.usage_revision
		 FROM conversation_messages c LEFT JOIN messages m ON m.session_id=c.session_id AND m.ordinal=c.ordinal
		 WHERE c.usage_revision > ? AND c.removed=0 AND c.revision <= ?
		 ORDER BY c.usage_revision LIMIT ?`, after, conversationAfter, MaxSessionLimit+1)
	if err != nil {
		return nil, 0, false, fmt.Errorf("reading message usage changes: %w", err)
	}
	defer rows.Close()
	var out []changeStreamMessageRow
	var last int64
	more := false
	for rows.Next() {
		if len(out) == MaxSessionLimit {
			more = true
			break
		}
		var row changeStreamMessageRow
		var revision int64
		if err := rows.Scan(&row.message.SessionID, &row.message.MessageID, &revision, &row.message.Role,
			&row.message.Timestamp, &row.message.Deleted, &row.ordinal, &row.message.Model, &last); err != nil {
			return nil, 0, false, err
		}
		row.message.Revision = strconv.FormatInt(revision, 10)
		sessionIDs[row.message.SessionID] = struct{}{}
		out = append(out, row)
	}
	return out, last, more, errors.Join(rows.Err(), rows.Close())
}

// readChangeStreamUsageTx reads sessions whose usage or session columns
// changed, in usage revision order.
func readChangeStreamUsageTx(
	ctx context.Context, tx *sql.Tx, after int64, sessionIDs map[string]struct{},
) (int64, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id,revision FROM usage_session_changes
		WHERE revision > ? AND session_id != '' ORDER BY revision LIMIT ?`, after, MaxSessionLimit+1)
	if err != nil {
		return 0, false, fmt.Errorf("reading usage changes: %w", err)
	}
	defer rows.Close()
	var last int64
	count := 0
	more := false
	for rows.Next() {
		if count == MaxSessionLimit {
			more = true
			break
		}
		count++
		var id string
		if err := rows.Scan(&id, &last); err != nil {
			return 0, false, err
		}
		sessionIDs[id] = struct{}{}
	}
	return last, more, errors.Join(rows.Err(), rows.Close())
}

type changeStreamTally struct {
	total     ChangeStreamUsage
	cost      money.Money
	allPriced bool
}

func (t *changeStreamTally) usage() *ChangeStreamUsage {
	if t == nil {
		return nil
	}
	usage := t.total
	usage.CostMicrodollars = t.cost.Microdollars
	usage.Priced = t.allPriced
	return &usage
}

type changeStreamCreditCost struct {
	ordinal    int64
	attributed bool
	cost       money.Money
}

// ChangeStreamUsageCache keeps the session usage one change stream read
// computed, so the next read of the same stream can reuse it. A replay sends a
// long session over many pages, and each page would otherwise reload and
// reprice every usage row of that session. The cache holds only the sessions
// of the latest read, which is enough for a session that spans consecutive
// pages. The zero value is ready to use. A cache serves one stream and is not
// safe for concurrent use.
type ChangeStreamUsageCache struct {
	databaseID   string
	usage        int64
	messageUsage int64
	sessions     map[string]*changeStreamSessionUsage
}

// changeStreamSessionUsage is one session's totals and the usage of each of
// its messages, by ordinal.
type changeStreamSessionUsage struct {
	models   []string
	total    ChangeStreamUsage
	messages map[int64]*changeStreamTally
}

// reusable reports whether the cached usage still matches the archive. Every
// write that can change a session's computed usage advances the usage change
// revision: usage rows, pricing, and session columns, including the transcript
// revision that each message write advances. Claude snapshot peers can move
// usage between sessions, so any change anywhere discards the whole cache.
func (c *ChangeStreamUsageCache) reusable(heads changeStreamRevisions) bool {
	return c != nil && c.sessions != nil && c.databaseID == heads.databaseID &&
		c.usage == heads.usage && c.messageUsage == heads.messageUsage
}

// readChangeStreamSessionsTx reads the current state of each session and the
// usage of each of its messages, with session export's usage rules.
func (db *DB) readChangeStreamSessionsTx(
	ctx context.Context, tx *sql.Tx, heads changeStreamRevisions, ids []string, cache *ChangeStreamUsageCache,
) ([]ChangeStreamSession, map[string]*changeStreamSessionUsage, error) {
	sessions := make([]ChangeStreamSession, len(ids))
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		sessions[i] = ChangeStreamSession{SessionID: id, Deleted: true, Models: []string{}}
		index[id] = i
	}
	usage := map[string]*changeStreamSessionUsage{}
	if len(ids) == 0 {
		return sessions, usage, nil
	}
	machines, err := loadMachineIdentities(ctx, tx)
	if err != nil {
		return nil, nil, fmt.Errorf("reading machine identities: %w", err)
	}
	observations := map[string]export.ProjectIdentityObservation{}
	var live []string
	for start := 0; start < len(ids); start += maxSQLVars {
		chunk := ids[start:min(start+maxSQLVars, len(ids))]
		// Trashed sessions keep their record, so they keep their origin too.
		err := readChangeStreamSessionMachinesTx(ctx, tx, chunk, func(id, machine string) {
			if origin := machines.origin(machine, heads.archiveID); origin != "" {
				sessions[index[id]].MachineID = &origin
			}
		})
		if err != nil {
			return nil, nil, err
		}
		err = readChangeStreamSessionRowsTx(ctx, tx, chunk, func(session ChangeStreamSession, obs export.ProjectIdentityObservation) {
			session.MachineID = sessions[index[session.SessionID]].MachineID
			sessions[index[session.SessionID]] = session
			observations[session.SessionID] = obs
			live = append(live, session.SessionID)
		})
		if err != nil {
			return nil, nil, err
		}
	}
	if len(live) == 0 {
		return sessions, usage, nil
	}

	snapshots, err := db.listSessionProjectIdentitySnapshotsFrom(ctx, tx, live)
	if err != nil {
		return nil, nil, err
	}
	salt, err := sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveSaltKey, ErrArchiveSaltMissing, "archive salt")
	if err != nil {
		return nil, nil, err
	}
	salt, err = validateArchiveSalt(salt)
	if err != nil {
		return nil, nil, err
	}
	scope := export.IdentityScope{ArchiveID: heads.archiveID, ArchiveSalt: salt}
	for _, id := range live {
		obs, ok := snapshots[id]
		if !ok {
			obs = observations[id]
		}
		sessions[index[id]].Project = new(export.ResolveProjectReferenceFromObservation(obs, scope))
	}

	var misses []string
	if cache.reusable(heads) {
		for _, id := range live {
			if cached, ok := cache.sessions[id]; ok {
				usage[id] = cached
			} else {
				misses = append(misses, id)
			}
		}
	} else {
		misses = live
	}
	if err := db.computeChangeStreamUsageTx(ctx, tx, misses, usage); err != nil {
		return nil, nil, err
	}
	for _, id := range live {
		computed, ok := usage[id]
		if !ok {
			return nil, nil, fmt.Errorf("change stream usage missing for session %s", id)
		}
		session := &sessions[index[id]]
		session.Models = computed.models
		total := computed.total
		session.Usage = &total
	}
	if cache != nil {
		*cache = ChangeStreamUsageCache{
			databaseID: heads.databaseID, usage: heads.usage, messageUsage: heads.messageUsage, sessions: usage,
		}
	}
	return sessions, usage, nil
}

// computeChangeStreamUsageTx computes the usage of each live session in ids
// and stores it in usage.
func (db *DB) computeChangeStreamUsageTx(
	ctx context.Context, tx *sql.Tx, ids []string, usage map[string]*changeStreamSessionUsage,
) error {
	if len(ids) == 0 {
		return nil
	}
	pricing, err := db.loadPricingMapFrom(ctx, tx)
	if err != nil {
		return fmt.Errorf("loading pricing: %w", err)
	}
	resolver := export.NewPricingResolver(pricing)
	for start := 0; start < len(ids); start += MaxSessionLimit {
		chunk := ids[start:min(start+MaxSessionLimit, len(ids))]
		accum := make(map[string]*sessionExportUsageAccum, len(chunk))
		for _, id := range chunk {
			accum[id] = newSessionExportUsageAccum()
			usage[id] = &changeStreamSessionUsage{messages: map[int64]*changeStreamTally{}}
		}
		contributions := map[string][]changeStreamCreditCost{}
		err := forEachSessionUsageCredit(ctx, tx, chunk, resolver, func(credit sessionUsageCredit) error {
			a, ok := accum[credit.owner]
			session, found := usage[credit.owner]
			if !ok || !found {
				return nil
			}
			if err := a.add(credit); err != nil {
				return err
			}
			if !credit.contributes {
				return nil
			}
			// A row credited to another session's totals has no message there.
			attributed := credit.row.messageOrdinal.Valid && credit.row.sessionID == credit.owner
			ordinal := credit.row.messageOrdinal.Int64
			contributions[credit.owner] = append(contributions[credit.owner],
				changeStreamCreditCost{ordinal: ordinal, attributed: attributed, cost: credit.cost})
			if !attributed {
				return nil
			}
			tallies := session.messages
			tally := tallies[ordinal]
			if tally == nil {
				tally = &changeStreamTally{allPriced: true}
				tallies[ordinal] = tally
			}
			tally.total.InputTokens += credit.inputTokens
			tally.total.OutputTokens += credit.outputTokens
			tally.total.CacheCreationTokens += credit.cacheCreationInputTokens
			tally.total.CacheReadTokens += credit.cacheReadInputTokens
			if !credit.priced {
				tally.allPriced = false
				return nil
			}
			var err error
			tally.cost, err = money.Add(tally.cost, credit.cost)
			return err
		})
		if err != nil {
			return err
		}
		for _, id := range chunk {
			a, ok := accum[id]
			session, found := usage[id]
			if !ok || !found {
				continue
			}
			if a.authoritativeCost != nil {
				if err := allocateChangeStreamReportedCost(ctx, *a.authoritativeCost, contributions[id], session.messages); err != nil {
					return err
				}
			}
			totals := a.modelUsage()
			session.models = totals.Models
			session.total = ChangeStreamUsage{
				InputTokens:         totals.InputTokens,
				OutputTokens:        totals.OutputTokens,
				CacheCreationTokens: totals.CacheCreationInputTokens,
				CacheReadTokens:     totals.CacheReadInputTokens,
				CostMicrodollars:    totals.Cost.Microdollars,
				Priced:              totals.HasCost,
			}
		}
	}
	return nil
}

// readChangeStreamSessionMachinesTx reads the stored machine key of each
// session in ids that the archive still holds, trashed or not.
func readChangeStreamSessionMachinesTx(
	ctx context.Context, tx *sql.Tx, ids []string, fn func(id, machine string),
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,machine FROM sessions WHERE id IN (`+placeholderList(len(ids))+`)`,
		stringArgs(ids)...)
	if err != nil {
		return fmt.Errorf("reading changed session machines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, machine string
		if err := rows.Scan(&id, &machine); err != nil {
			return err
		}
		fn(id, machine)
	}
	return errors.Join(rows.Err(), rows.Close())
}

// readChangeStreamSessionRowsTx reads the sessions in ids that exist and are
// not in the trash.
func readChangeStreamSessionRowsTx(
	ctx context.Context, tx *sql.Tx, ids []string,
	fn func(ChangeStreamSession, export.ProjectIdentityObservation),
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,agent,is_automated,relationship_type='subagent',project,machine,
		NULLIF(started_at,''),`+sessionExportLastActivityExprFor("sessions")+`,message_count
		FROM sessions WHERE deleted_at IS NULL AND id IN (`+placeholderList(len(ids))+`)`, stringArgs(ids)...)
	if err != nil {
		return fmt.Errorf("reading changed sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var session ChangeStreamSession
		var meta activity.SessionMeta
		var obs export.ProjectIdentityObservation
		var startedAt sql.NullString
		var lastActivity string
		if err := rows.Scan(&session.SessionID, &meta.Agent, &meta.IsAutomated, &meta.IsSubagent, &obs.Project, &obs.Machine,
			&startedAt, &lastActivity, &session.MessageCount); err != nil {
			return err
		}
		session.Agent = meta.Agent
		session.Automation = meta.ActivityCategory()
		session.Models = []string{}
		session.StartedAt = nullStringPtr(startedAt)
		session.LastActivityAt = &lastActivity
		fn(session, obs)
	}
	return errors.Join(rows.Err(), rows.Close())
}

// allocateChangeStreamReportedCost spreads a reported session total over its
// usage rows by their computed cost, as the session usage breakdown does.
func allocateChangeStreamReportedCost(
	ctx context.Context, total money.Money, credits []changeStreamCreditCost,
	tallies map[int64]*changeStreamTally,
) error {
	weights := make([]money.Money, len(credits))
	for i, credit := range credits {
		weights[i] = credit.cost
	}
	costs, err := export.AllocateCostByWeightContext(ctx, total, weights)
	if err != nil {
		return err
	}
	for _, credit := range credits {
		if tally := tallies[credit.ordinal]; credit.attributed && tally != nil {
			tally.cost, tally.allPriced = money.Money{}, true
		}
	}
	for i, credit := range credits {
		tally, ok := tallies[credit.ordinal]
		if !credit.attributed || !ok {
			continue
		}
		tally.cost, err = money.Add(tally.cost, costs[i])
		if err != nil {
			return err
		}
	}
	return nil
}

func stringArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}
