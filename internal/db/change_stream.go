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

// A change stream follows two trigger-maintained counters. The conversation
// publication revision covers message and session-state changes; the usage
// change revision covers token, cost, and session-column changes, including
// usage written after its message was published. The cursor carries both,
// bound to one database generation.
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
		position.Conversation >= 0 && position.Usage >= 0
	return position, valid
}

// ChangeStreamHead identifies the archive and its newest change position.
type ChangeStreamHead struct {
	ArchiveID  string
	DatabaseID string
	Head       string
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

// ChangeStreamSession is a session's current state. A deleted or trashed
// session carries only its ID.
type ChangeStreamSession struct {
	SessionID      string                   `json:"session_id"`
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

// ChangeStreamMessage is a message's current state without its text. A
// deleted message carries no model, project, or usage.
type ChangeStreamMessage struct {
	SessionID string                   `json:"session_id"`
	MessageID string                   `json:"message_id"`
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
	}.encode()
	if err != nil {
		return ChangeStreamHead{}, err
	}
	return ChangeStreamHead{ArchiveID: heads.archiveID, DatabaseID: heads.databaseID, Head: head}, tx.Commit()
}

type changeStreamMessageRow struct {
	message ChangeStreamMessage
	ordinal int64
}

// ReadChangeStream returns the changes published after cursor in one read
// snapshot, at most MaxSessionLimit message and session-state changes plus at
// most MaxSessionLimit usage changes. An empty cursor, one from another
// database generation, or one this archive did not issue restarts from the
// beginning. Every session a change touches is reported with its current
// state, so a message change also refreshes its session's totals.
func (db *DB) ReadChangeStream(ctx context.Context, cursor string) (ChangeStreamBatch, error) {
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
		position.Conversation > heads.conversation || position.Usage > heads.usage {
		batch.Reset = true
		position = changeStreamPosition{
			Version: changeStreamPositionVersion, Kind: changeStreamPositionKind,
			DatabaseID: heads.databaseID,
		}
	} else {
		batch.From = cursor
	}

	sessionIDs := map[string]struct{}{}
	messages, conversationMore, err := readChangeStreamConversationTx(ctx, tx, position.Conversation, sessionIDs)
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
	usageLast, usageMore, err := readChangeStreamUsageTx(ctx, tx, position.Usage, sessionIDs)
	if err != nil {
		return ChangeStreamBatch{}, err
	}
	if usageMore {
		position.Usage = usageLast
	} else {
		position.Usage = heads.usage
	}
	batch.More = conversationMore || usageMore

	ids := make([]string, 0, len(sessionIDs))
	for id := range sessionIDs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	sessions, tallies, err := db.readChangeStreamSessionsTx(ctx, tx, heads.archiveID, ids)
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
		if !ok || message.Deleted || sessions[i].Deleted {
			message.Model = ""
		} else {
			message.Project = sessions[i].Project
			message.Usage = tallies[changeStreamMessageKey{message.SessionID, row.ordinal}].usage()
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

type changeStreamMessageKey struct {
	sessionID string
	ordinal   int64
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
	key        changeStreamMessageKey
	attributed bool
	cost       money.Money
}

// readChangeStreamSessionsTx reads the current state of each session and the
// usage of each of its messages, with session export's usage rules.
func (db *DB) readChangeStreamSessionsTx(
	ctx context.Context, tx *sql.Tx, archiveID string, ids []string,
) ([]ChangeStreamSession, map[changeStreamMessageKey]*changeStreamTally, error) {
	sessions := make([]ChangeStreamSession, len(ids))
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		sessions[i] = ChangeStreamSession{SessionID: id, Deleted: true, Models: []string{}}
		index[id] = i
	}
	tallies := map[changeStreamMessageKey]*changeStreamTally{}
	if len(ids) == 0 {
		return sessions, tallies, nil
	}
	observations := map[string]export.ProjectIdentityObservation{}
	var live []string
	for start := 0; start < len(ids); start += maxSQLVars {
		chunk := ids[start:min(start+maxSQLVars, len(ids))]
		err := readChangeStreamSessionRowsTx(ctx, tx, chunk, func(session ChangeStreamSession, obs export.ProjectIdentityObservation) {
			sessions[index[session.SessionID]] = session
			observations[session.SessionID] = obs
			live = append(live, session.SessionID)
		})
		if err != nil {
			return nil, nil, err
		}
	}
	if len(live) == 0 {
		return sessions, tallies, nil
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
	scope := export.IdentityScope{ArchiveID: archiveID, ArchiveSalt: salt}
	for _, id := range live {
		obs, ok := snapshots[id]
		if !ok {
			obs = observations[id]
		}
		sessions[index[id]].Project = new(export.ResolveProjectReferenceFromObservation(obs, scope))
	}

	pricing, err := db.loadPricingMapFrom(ctx, tx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading pricing: %w", err)
	}
	resolver := export.NewPricingResolver(pricing)
	for start := 0; start < len(live); start += MaxSessionLimit {
		chunk := live[start:min(start+MaxSessionLimit, len(live))]
		accum := make(map[string]*sessionExportUsageAccum, len(chunk))
		for _, id := range chunk {
			accum[id] = newSessionExportUsageAccum()
		}
		contributions := map[string][]changeStreamCreditCost{}
		err := forEachSessionUsageCredit(ctx, tx, chunk, resolver, func(credit sessionUsageCredit) error {
			a, ok := accum[credit.owner]
			if !ok {
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
			key := changeStreamMessageKey{credit.owner, credit.row.messageOrdinal.Int64}
			contributions[credit.owner] = append(contributions[credit.owner],
				changeStreamCreditCost{key: key, attributed: attributed, cost: credit.cost})
			if !attributed {
				return nil
			}
			tally := tallies[key]
			if tally == nil {
				tally = &changeStreamTally{allPriced: true}
				tallies[key] = tally
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
			return nil, nil, err
		}
		for _, id := range chunk {
			a, ok := accum[id]
			if !ok {
				continue
			}
			if a.authoritativeCost != nil {
				if err := allocateChangeStreamReportedCost(ctx, *a.authoritativeCost, contributions[id], tallies); err != nil {
					return nil, nil, err
				}
			}
			usage := a.modelUsage()
			session := &sessions[index[id]]
			session.Models = usage.Models
			session.Usage = &ChangeStreamUsage{
				InputTokens:         usage.InputTokens,
				OutputTokens:        usage.OutputTokens,
				CacheCreationTokens: usage.CacheCreationInputTokens,
				CacheReadTokens:     usage.CacheReadInputTokens,
				CostMicrodollars:    usage.Cost.Microdollars,
				Priced:              usage.HasCost,
			}
		}
	}
	return sessions, tallies, nil
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
	tallies map[changeStreamMessageKey]*changeStreamTally,
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
		if tally := tallies[credit.key]; credit.attributed && tally != nil {
			tally.cost, tally.allPriced = money.Money{}, true
		}
	}
	for i, credit := range credits {
		tally, ok := tallies[credit.key]
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
