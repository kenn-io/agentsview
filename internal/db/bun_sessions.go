package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

var bunSessionQueryDialect = QueryDialect{
	name:             "bun",
	placeholderStyle: placeholderQuestion,
	trueLiteral:      "TRUE",
	falseLiteral:     "FALSE",
	dateStartExpr: func(q func(string) string) string {
		return "COALESCE(" + bunNullableTimestamp(q("started_at")) +
			", " + q("created_at") + ")"
	},
	dateEndExpr: func(q func(string) string) string {
		outerID := q("id")
		if outerID == "id" {
			outerID = "session.id"
		}
		return "COALESCE(" + bunNullableTimestamp(q("ended_at")) +
			", (SELECT MAX(" + bunNullableTimestamp("m.timestamp") +
			") FROM messages m WHERE m.session_id = " + outerID +
			" AND " + bunNullableTimestamp("m.timestamp") + " IS NOT NULL), " +
			bunNullableTimestamp(q("started_at")) + ", " + q("created_at") + ")"
	},
	dateParam:     func(ph string) string { return ph },
	activityParam: func(ph string) string { return ph },
	cursorActivityExpr: "COALESCE(" + bunNullableTimestamp("ended_at") + ", " +
		bunNullableTimestamp("started_at") + ", created_at)",
	cursorParam:            func(ph string) string { return ph },
	castCursor:             func(ph string, _ valueKind) string { return ph },
	portableEmptyTimestamp: true,
	terminationExpr: "COALESCE(" + bunNullableTimestamp("ended_at") + ", " +
		bunNullableTimestamp("started_at") + ", created_at)",
	terminationKind:        timestampText,
	caseInsensitiveLike:    "LIKE",
	caseInsensitiveLikeEsc: `ESCAPE '\'`,
	regexPredicate: func(col, ph string) string {
		return col + " LIKE " + ph
	},
	sidebarChildRelationships:   []string{"subagent", "fork"},
	canonicalChildRelationships: []string{"subagent", "fork", "continuation"},
}

// PortableBunSessionQueryDialect returns the Bun-placeholder session dialect
// used by engines with native timestamp comparison semantics.
func PortableBunSessionQueryDialect() QueryDialect {
	return bunSessionQueryDialect
}

// SQLiteBunSessionQueryDialect preserves chronological comparisons for the
// shipped SQLite text timestamp representation while retaining Bun's portable
// question-mark placeholders and common non-time predicates.
func SQLiteBunSessionQueryDialect() QueryDialect {
	dialect := bunSessionQueryDialect
	sqlite := SQLiteQueryDialect()
	dialect.dateStartExpr = sqlite.dateStartExpr
	dialect.dateEndExpr = func(q func(string) string) string {
		outerID := q("id")
		if outerID == "id" {
			outerID = "session.id"
		}
		return "julianday(COALESCE(NULLIF(" + q("ended_at") +
			", ''), (SELECT m.timestamp FROM messages m" +
			" WHERE m.session_id = " + outerID +
			" AND m.timestamp != '' ORDER BY julianday(m.timestamp)" +
			" DESC, m.timestamp DESC LIMIT 1), NULLIF(" + q("started_at") +
			", ''), " + q("created_at") + "))"
	}
	dialect.dateParam = sqlite.dateParam
	dialect.timestampOrderExpr = func(column string) string {
		return "julianday(NULLIF(" + column + ", ''))"
	}
	dialect.castCursor = func(placeholder string, kind valueKind) string {
		if kind == kindTimestamp {
			return "julianday(" + placeholder + ")"
		}
		return placeholder
	}
	dialect.terminationExpr = sqlite.terminationExpr
	dialect.terminationKind = sqlite.terminationKind
	return dialect
}

func bunNullableTimestamp(column string) string {
	return "CASE WHEN CAST(" + column + " AS VARCHAR) = '' THEN NULL ELSE " +
		column + " END"
}

// EncodeCursor returns a base64-encoded, HMAC-signed cursor string.
func (s *BunStore) EncodeCursor(cursor SessionCursor) string {
	payload, _ := json.Marshal(cursor)
	s.cursorMu.RLock()
	mac := hmac.New(sha256.New, s.cursorSecret)
	s.cursorMu.RUnlock()
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// DecodeCursor verifies and decodes a session cursor. Unsigned legacy cursors
// remain readable, but their cached total is discarded.
func (s *BunStore) DecodeCursor(value string) (SessionCursor, error) {
	parts := strings.Split(value, ".")
	if len(parts) == 1 {
		payload, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return SessionCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}
		var cursor SessionCursor
		if err := json.Unmarshal(payload, &cursor); err != nil {
			return SessionCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}
		cursor.Total = 0
		return cursor, nil
	}
	if len(parts) != 2 {
		return SessionCursor{}, fmt.Errorf("%w: invalid format", ErrInvalidCursor)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return SessionCursor{}, fmt.Errorf("%w: invalid payload: %v", ErrInvalidCursor, err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return SessionCursor{}, fmt.Errorf("%w: invalid signature encoding: %v", ErrInvalidCursor, err)
	}
	s.cursorMu.RLock()
	mac := hmac.New(sha256.New, s.cursorSecret)
	s.cursorMu.RUnlock()
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return SessionCursor{}, fmt.Errorf("%w: signature mismatch", ErrInvalidCursor)
	}
	var cursor SessionCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return SessionCursor{}, fmt.Errorf("%w: invalid json: %v", ErrInvalidCursor, err)
	}
	return cursor, nil
}

// ListSessions returns a cursor-paginated list from canonical session rows.
// Count and page scans share one backend guard so handle replacement cannot
// split one logical read across mirror generations.
func (s *BunStore) ListSessions(
	ctx context.Context, filter SessionFilter,
) (SessionPage, error) {
	if filter.Limit <= 0 || filter.Limit > MaxSessionLimit {
		filter.Limit = DefaultSessionLimit
	}
	resolvedSort := ResolveSort(filter)
	var cursor SessionCursor
	var total int
	if filter.Cursor != "" {
		var err error
		cursor, err = s.DecodeCursor(filter.Cursor)
		if err != nil {
			return SessionPage{}, err
		}
		total = cursor.Total
	}

	var page SessionPage
	err := s.view(ctx, func(store bun.IDB) error {
		dialect := s.backend.SessionQueryDialect()
		where, args := BuildSessionFilterSQL(filter, dialect)
		base := store.NewSelect().Model((*bunmodel.Session)(nil))
		base = applyBunWhere(base, where, args)
		if total <= 0 {
			count, err := base.Clone().Count(ctx)
			if err != nil {
				return fmt.Errorf("counting sessions: %w", err)
			}
			total = count
		}
		query := base.Clone()
		if filter.Cursor != "" {
			values, err := CursorPredicateValues(cursor, resolvedSort)
			if err != nil {
				return err
			}
			builder := NewQueryBuilder(dialect, 0)
			query = query.Where(
				builder.CursorPredicate(resolvedSort, filter, values, cursor.ID),
				builder.Args()...,
			)
		}
		orderBuilder := NewQueryBuilder(dialect, 0)
		order := strings.TrimPrefix(
			orderBuilder.OrderByClause(resolvedSort, filter), "ORDER BY ",
		)
		var rows []bunmodel.Session
		if len(orderBuilder.Args()) == 0 {
			query = query.OrderExpr(order)
		} else {
			query = query.OrderExpr(order, orderBuilder.Args()...)
		}
		if err := query.Limit(filter.Limit+1).Scan(ctx, &rows); err != nil {
			return fmt.Errorf("querying sessions: %w", err)
		}
		page.Total = total
		page.Sessions = make([]Session, 0, min(len(rows), filter.Limit))
		for _, row := range rows[:min(len(rows), filter.Limit)] {
			page.Sessions = append(page.Sessions, baseSessionFromBunRow(row))
		}
		if len(rows) > filter.Limit {
			last := page.Sessions[len(page.Sessions)-1]
			page.NextCursor = s.EncodeCursor(
				NextSessionCursor(&last, resolvedSort, total, filter),
			)
		}
		return nil
	})
	if err != nil {
		return SessionPage{}, err
	}
	return page, nil
}

func visibleSessionFromBunRow(row bunmodel.Session) Session {
	session := sessionFromBunRow(row)
	if session.DisplayName == nil {
		session.DisplayName = session.SessionName
	}
	return session
}

func baseSessionFromBunRow(row bunmodel.Session) Session {
	session := visibleSessionFromBunRow(row)
	session.SessionName = nil
	session.FilePath = nil
	session.FileSize = nil
	session.FileMtime = nil
	session.FileInode = nil
	session.FileDevice = nil
	session.FileHash = nil
	session.LocalModifiedAt = nil
	return session
}

// GetSession returns a visible session and excludes trashed rows.
func (s *BunStore) GetSession(ctx context.Context, id string) (*Session, error) {
	return s.getSession(ctx, id, false)
}

// GetSessionFull includes trashed rows and canonical file metadata.
func (s *BunStore) GetSessionFull(ctx context.Context, id string) (*Session, error) {
	return s.getSession(ctx, id, true)
}

func (s *BunStore) getSession(
	ctx context.Context, id string, includeDeleted bool,
) (*Session, error) {
	var session *Session
	err := s.view(ctx, func(store bun.IDB) error {
		var err error
		session, err = s.getSessionFrom(ctx, store, id, includeDeleted)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("getting session %s: %w", id, err)
	}
	return session, nil
}

func (s *BunStore) getSessionFrom(
	ctx context.Context, store bun.IDB, id string, includeDeleted bool,
) (*Session, error) {
	var row bunmodel.Session
	query := store.NewSelect().Model(&row).Where("id = ?", id)
	if !includeDeleted {
		query = query.Where("deleted_at IS NULL")
	}
	if err := query.Scan(ctx); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if includeDeleted {
		session := visibleSessionFromBunRow(row)
		if hydrator, ok := s.backend.(bunSessionFullHydrator); ok {
			if err := hydrator.HydrateSessionFull(ctx, store, &session); err != nil {
				return nil, err
			}
		}
		return &session, nil
	}
	session := baseSessionFromBunRow(row)
	return &session, nil
}

const partialSessionCandidateBatchSize = 64

// FindSessionIDsByPartial performs literal, case-sensitive substring matching.
func (s *BunStore) FindSessionIDsByPartial(
	ctx context.Context, partial string, limit int,
) ([]string, error) {
	if partial == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	ids := make([]string, 0, min(limit, partialSessionCandidateBatchSize))
	err := s.view(ctx, func(store bun.IDB) error {
		activityExpr := bunSessionActivityExpr("session")
		pattern := "%" + EscapeLikePattern(partial) + "%"
		var cursorActivity any
		var cursorID string
		hasCursor := false
		for len(ids) < limit {
			var candidates []struct {
				ID       string `bun:"id"`
				Activity any    `bun:"activity"`
			}
			query := store.NewSelect().TableExpr("sessions AS session").
				ColumnExpr("session.id").
				ColumnExpr(activityExpr+" AS activity").
				Where("session.id LIKE ? ESCAPE '\\'", pattern).
				Where("session.deleted_at IS NULL")
			if hasCursor {
				query = query.Where(
					"("+activityExpr+" < ? OR ("+activityExpr+
						" = ? AND session.id < ?))",
					cursorActivity, cursorActivity, cursorID,
				)
			}
			if err := query.OrderExpr(activityExpr+" DESC").
				OrderExpr("session.id DESC").
				Limit(partialSessionCandidateBatchSize).
				Scan(ctx, &candidates); err != nil {
				return err
			}
			for _, candidate := range candidates {
				if strings.Contains(candidate.ID, partial) {
					ids = append(ids, candidate.ID)
					if len(ids) == limit {
						return nil
					}
				}
			}
			if len(candidates) < partialSessionCandidateBatchSize {
				return nil
			}
			last := candidates[len(candidates)-1]
			cursorActivity = last.Activity
			cursorID = last.ID
			hasCursor = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("finding sessions by partial id %q: %w", partial, err)
	}
	return ids, nil
}

// GetChildSessions returns non-deleted children ordered by start time.
func (s *BunStore) GetChildSessions(
	ctx context.Context, parentID string,
) ([]Session, error) {
	var rows []bunmodel.Session
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Model(&rows).
			Where("parent_session_id = ?", parentID).
			Where("deleted_at IS NULL").
			OrderExpr("COALESCE(" + bunNullableTimestamp("started_at") +
				", created_at) ASC").
			OrderExpr("id ASC").Scan(ctx)
	})
	if err != nil {
		return nil, fmt.Errorf("querying child sessions for %s: %w", parentID, err)
	}
	sessions := make([]Session, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, baseSessionFromBunRow(row))
	}
	return sessions, nil
}

// GetSidebarSessionIndex returns canonical roots and their descendants.
func (s *BunStore) GetSidebarSessionIndex(
	ctx context.Context, filter SessionFilter,
) (SidebarSessionIndex, error) {
	filter.IncludeChildren = true
	filter.IncludeOrphans = true
	var index SidebarSessionIndex
	err := s.view(ctx, func(store bun.IDB) error {
		var err error
		if filter.Limit > 0 || filter.Cursor != "" || filter.Starred {
			index, err = s.getSidebarSessionIndexPage(
				ctx, store, filter, s.backend.SessionQueryDialect(),
			)
		} else {
			index, err = s.getSidebarSessionIndexAll(
				ctx, store, filter, s.backend.SessionQueryDialect(),
			)
		}
		return err
	})
	if err != nil {
		return SidebarSessionIndex{}, err
	}
	return index, nil
}

func sidebarRootFilter(filter SessionFilter, dialect QueryDialect) (string, []any) {
	rootFilter := filter
	rootFilter.IncludeChildren = false
	rootFilter.Cursor = ""
	rootFilter.Starred = false
	where, args := BuildSessionBaseFilterSQL(rootFilter, dialect)
	where += " AND " + BuildCanonicalRootWhere(
		dialect, "session", filter.IncludeOrphans,
	)
	return where, args
}

func bunSessionActivityExpr(alias string) string {
	qualify := func(column string) string {
		if alias == "" {
			return column
		}
		return alias + "." + column
	}
	return "COALESCE(" + bunNullableTimestamp(qualify("ended_at")) + ", " +
		bunNullableTimestamp(qualify("started_at")) + ", " +
		qualify("created_at") + ")"
}

func bunSessionActivityOrderExpr(alias string, dialect QueryDialect) string {
	return dialect.timestampExpr(bunSessionActivityExpr(alias))
}

func sidebarRootTreeSQL(
	rootWhere, childAutomationWhere string, starred bool,
) string {
	return `WITH RECURSIVE root_candidates(id) AS (
			SELECT session.id
			FROM sessions AS session
			WHERE ` + rootWhere + `
		),
		tree(root_id, id) AS (
			SELECT id, id FROM root_candidates
			UNION
			SELECT t.root_id, child.id
			FROM sessions AS child
			JOIN tree AS t ON child.parent_session_id = t.id
			WHERE child.message_count > 0
			  AND child.deleted_at IS NULL` + childAutomationWhere + `
		)` + sidebarStarredRootCTE(starred)
}

func sidebarStarredRootCTE(enabled bool) string {
	if !enabled {
		return ""
	}
	return `,
		eligible_roots(id) AS (
			SELECT DISTINCT tree.root_id
			FROM tree
			JOIN starred_sessions AS starred ON starred.session_id = tree.id
		)`
}

func sidebarStarredRootJoin(enabled bool) string {
	if !enabled {
		return ""
	}
	return "JOIN eligible_roots AS eligible ON eligible.id = t.root_id"
}

func sidebarChildAutomationWhere(filter SessionFilter, dialect QueryDialect) string {
	predicate := automationScopePredicate(filter, dialect, "child")
	if predicate == "" {
		return ""
	}
	return " AND " + predicate
}

func (s *BunStore) getSidebarSessionIndexAll(
	ctx context.Context, store bun.IDB, filter SessionFilter, dialect QueryDialect,
) (SidebarSessionIndex, error) {
	rootWhere, rootArgs := sidebarRootFilter(filter, dialect)
	var total int
	if err := store.NewRaw(
		"SELECT COUNT(*) FROM sessions AS session WHERE "+rootWhere,
		rootArgs...,
	).Scan(ctx, &total); err != nil {
		return SidebarSessionIndex{}, fmt.Errorf("counting sidebar roots: %w", err)
	}

	where, args := BuildSessionFilterSQL(filter, dialect)
	var rows []bunmodel.Session
	query := store.NewSelect().Model(&rows)
	query = applyBunWhere(query, where, args)
	if err := query.
		OrderExpr(bunSessionActivityOrderExpr("", dialect) + " DESC").
		OrderExpr("id DESC").Scan(ctx); err != nil {
		return SidebarSessionIndex{}, fmt.Errorf(
			"querying sidebar session index: %w", err,
		)
	}
	return SidebarSessionIndex{
		Sessions: sidebarRowsFromBun(rows),
		Total:    total,
	}, nil
}

func (s *BunStore) getSidebarSessionIndexPage(
	ctx context.Context, store bun.IDB, filter SessionFilter, dialect QueryDialect,
) (SidebarSessionIndex, error) {
	if filter.Limit <= 0 || filter.Limit > MaxSessionLimit {
		filter.Limit = DefaultSessionLimit
	}
	rootWhere, rootArgs := sidebarRootFilter(filter, dialect)
	childAutomationWhere := sidebarChildAutomationWhere(filter, dialect)
	treeSQL := sidebarRootTreeSQL(
		rootWhere, childAutomationWhere, filter.Starred,
	)

	var cursor SessionCursor
	total := 0
	if filter.Cursor != "" {
		var err error
		cursor, err = s.DecodeCursor(filter.Cursor)
		if err != nil {
			return SidebarSessionIndex{}, err
		}
		total = cursor.Total
	}
	if total <= 0 {
		countSQL := "SELECT COUNT(*) FROM root_candidates"
		if filter.Starred {
			countSQL = "SELECT COUNT(*) FROM eligible_roots"
		}
		if err := store.NewRaw(treeSQL+" "+countSQL, rootArgs...).
			Scan(ctx, &total); err != nil {
			return SidebarSessionIndex{}, fmt.Errorf(
				"counting sidebar roots: %w", err,
			)
		}
	}

	type rootRow struct {
		ID       string             `bun:"id"`
		Activity bunmodel.Timestamp `bun:"activity"`
	}
	rootActivityJoin := sidebarStarredRootJoin(filter.Starred)
	rootActivityExpr := bunSessionActivityExpr("session")
	rootActivityOrderExpr := bunSessionActivityOrderExpr("session", dialect)
	rootPageSQL := treeSQL + `,
		ranked_root_activity(id, activity, activity_order, activity_rank) AS (
			SELECT t.root_id AS id,
				` + rootActivityExpr + ` AS activity,
				` + rootActivityOrderExpr + ` AS activity_order,
				ROW_NUMBER() OVER (
					PARTITION BY t.root_id
					ORDER BY ` + rootActivityOrderExpr + ` DESC, session.id DESC
				) AS activity_rank
			FROM tree AS t
			` + rootActivityJoin + `
			JOIN sessions AS session ON session.id = t.id
		),
		root_activity(id, activity, activity_order) AS (
			SELECT id, activity, activity_order
			FROM ranked_root_activity
			WHERE activity_rank = 1
		)
		SELECT id, activity
		FROM root_activity`
	rootPageArgs := append([]any(nil), rootArgs...)
	if filter.Cursor != "" {
		activity, err := bunmodel.ParseTimestamp(cursor.EndedAt)
		if err != nil {
			return SidebarSessionIndex{}, fmt.Errorf(
				"%w: invalid sidebar activity: %v", ErrInvalidCursor, err,
			)
		}
		activityParam := dialect.dateParam("?")
		rootPageSQL += `
		WHERE activity_order < ` + activityParam + `
		   OR (activity_order = ` + activityParam + ` AND id < ?)`
		rootPageArgs = append(
			rootPageArgs, activity.Time, activity.Time, cursor.ID,
		)
	}
	rootPageSQL += `
		ORDER BY activity_order DESC, id DESC
		LIMIT ?`
	rootPageArgs = append(rootPageArgs, filter.Limit+1)
	var roots []rootRow
	if err := store.NewRaw(rootPageSQL, rootPageArgs...).Scan(ctx, &roots); err != nil {
		return SidebarSessionIndex{}, fmt.Errorf(
			"querying sidebar root page: %w", err,
		)
	}

	index := SidebarSessionIndex{
		Sessions: []SidebarSessionIndexRow{},
		Total:    total,
	}
	if len(roots) == 0 {
		return index, nil
	}
	selected := roots
	if len(roots) > filter.Limit {
		selected = roots[:filter.Limit]
		last := selected[len(selected)-1]
		index.NextCursor = s.EncodeCursor(SessionCursor{
			EndedAt: requiredTimestampFromBunRow(last.Activity),
			ID:      last.ID,
			Total:   total,
		})
	}

	valueRows := make([]string, len(selected))
	treeArgs := make([]any, 0, len(selected)*2)
	for i, root := range selected {
		valueRows[i] = "(?, ?)"
		treeArgs = append(treeArgs, root.ID, i)
	}
	treePageSQL := `WITH RECURSIVE root_page(id, ord) AS (
			VALUES ` + strings.Join(valueRows, ", ") + `
		),
		tree(id, ord) AS (
			SELECT id, ord FROM root_page
			UNION
			SELECT child.id, tree.ord
			FROM sessions AS child
			JOIN tree ON child.parent_session_id = tree.id
			WHERE child.message_count > 0
			  AND child.deleted_at IS NULL` + childAutomationWhere + `
		),
		ranked_tree(id, ord) AS (
			SELECT id, MIN(ord) AS ord
			FROM tree
			GROUP BY id
		)
		SELECT ` + bunModelColumns("session", (*bunmodel.Session)(nil)) + `
		FROM sessions AS session
		JOIN ranked_tree AS ranked ON session.id = ranked.id
		ORDER BY ranked.ord ASC, ` +
		bunSessionActivityOrderExpr("session", dialect) + ` DESC,
			session.id DESC`
	var rows []bunmodel.Session
	if err := store.NewRaw(treePageSQL, treeArgs...).Scan(ctx, &rows); err != nil {
		return SidebarSessionIndex{}, fmt.Errorf(
			"querying sidebar tree page: %w", err,
		)
	}
	index.Sessions = sidebarRowsFromBun(rows)
	return index, nil
}

func bunModelColumns(alias string, model any) string {
	columns := bunmodel.ModelColumns(model)
	for i, column := range columns {
		columns[i] = alias + `."` + column + `"`
	}
	return strings.Join(columns, ", ")
}

func sidebarRowsFromBun(rows []bunmodel.Session) []SidebarSessionIndexRow {
	out := make([]SidebarSessionIndexRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, sidebarRowFromBun(row))
	}
	return out
}

func applyBunWhere(
	query *bun.SelectQuery, where string, args []any,
) *bun.SelectQuery {
	if len(args) == 0 {
		return query.Where(where)
	}
	return query.Where(where, args...)
}

func sidebarRowFromBun(row bunmodel.Session) SidebarSessionIndexRow {
	displayName := row.DisplayName
	if displayName == nil {
		displayName = row.SessionName
	}
	return SidebarSessionIndexRow{
		ID: row.ID, ParentSessionID: row.ParentSessionID,
		RelationshipType: row.RelationshipType, Project: row.Project,
		Machine: row.Machine, Agent: row.Agent, AgentLabel: row.AgentLabel,
		Entrypoint: row.Entrypoint, SessionKind: row.SessionKind,
		DisplayName: displayName, StartedAt: timestampFromBunRow(row.StartedAt),
		EndedAt:           timestampFromBunRow(row.EndedAt),
		CreatedAt:         requiredTimestampFromBunRow(row.CreatedAt),
		TerminationStatus: row.TerminationStatus, MessageCount: row.MessageCount,
		UserMessageCount:   row.UserMessageCount,
		TranscriptRevision: &row.TranscriptRevision,
		IsAutomated:        row.IsAutomated,
		IsTeammate:         row.FirstMessage != nil && strings.Contains(*row.FirstMessage, "<teammate-message"),
	}
}

// GetSessionVersion returns the canonical message count and source marker.
func (s *BunStore) GetSessionVersion(id string) (int, int64, bool) {
	var count int
	var marker int64
	err := s.view(context.Background(), func(store bun.IDB) error {
		var versionErr error
		count, marker, versionErr = s.backend.SessionVersion(
			context.Background(), store, id,
		)
		return versionErr
	})
	if err != nil {
		return 0, 0, false
	}
	return count, marker, true
}

// FileSessionVersion returns the portable file-fingerprint marker used by the
// SQLite archive and DuckDB mirror.
func FileSessionVersion(
	ctx context.Context, store bun.IDB, id string,
) (int, int64, error) {
	var row struct {
		MessageCount    int                 `bun:"message_count"`
		FileMtime       *int64              `bun:"file_mtime"`
		FileHash        *string             `bun:"file_hash"`
		LocalModifiedAt *bunmodel.Timestamp `bun:"local_modified_at"`
	}
	err := store.NewSelect().Table("sessions").
		Column("message_count", "file_mtime", "file_hash", "local_modified_at").
		Where("id = ?", id).Scan(ctx, &row)
	if err != nil {
		return 0, 0, err
	}
	mtime := int64(0)
	if row.FileMtime != nil {
		mtime = *row.FileMtime
	}
	hash := ""
	if row.FileHash != nil {
		hash = *row.FileHash
	}
	modified := requiredTimestampFromBunRowPtr(row.LocalModifiedAt)
	return row.MessageCount, SessionVersionMarker(
		fmt.Sprintf("%d", mtime), hash, modified,
	), nil
}

func applyBunRootVisibility(
	query *bun.SelectQuery, excludeOneShot, excludeAutomated bool,
) *bun.SelectQuery {
	query = query.Where("message_count > 0").
		Where("relationship_type NOT IN (?)", bun.List([]string{"subagent", "fork"})).
		Where("deleted_at IS NULL")
	if excludeOneShot {
		if excludeAutomated {
			query = query.Where("user_message_count > 1")
		} else {
			query = query.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Where("user_message_count > 1").WhereOr("is_automated = ?", true)
			})
		}
	}
	if excludeAutomated {
		query = query.Where("is_automated = ?", false)
	}
	return query
}

// GetStats returns aggregate metadata for visible root sessions.
func (s *BunStore) GetStats(
	ctx context.Context, excludeOneShot, excludeAutomated bool,
) (Stats, error) {
	var row struct {
		SessionCount    int                 `bun:"session_count"`
		MessageCount    int                 `bun:"message_count"`
		ProjectCount    int                 `bun:"project_count"`
		MachineCount    int                 `bun:"machine_count"`
		EarliestSession *bunmodel.Timestamp `bun:"earliest_session"`
	}
	err := s.view(ctx, func(store bun.IDB) error {
		query := store.NewSelect().Table("sessions").
			ColumnExpr("COUNT(*) AS session_count").
			ColumnExpr("CAST(COALESCE(SUM(message_count), 0) AS BIGINT) AS message_count").
			ColumnExpr("COUNT(DISTINCT project) AS project_count").
			ColumnExpr("COUNT(DISTINCT machine) AS machine_count").
			ColumnExpr("MIN(COALESCE(" + bunNullableTimestamp("started_at") +
				", created_at)) AS earliest_session")
		return applyBunRootVisibility(query, excludeOneShot, excludeAutomated).
			Scan(ctx, &row)
	})
	if err != nil {
		return Stats{}, fmt.Errorf("fetching stats: %w", err)
	}
	return Stats{
		SessionCount: row.SessionCount, MessageCount: row.MessageCount,
		ProjectCount: row.ProjectCount, MachineCount: row.MachineCount,
		EarliestSession: timestampFromBunRow(row.EarliestSession),
	}, nil
}

// GetProjects returns visible root-session counts by project.
func (s *BunStore) GetProjects(
	ctx context.Context, excludeOneShot, excludeAutomated bool,
) ([]ProjectInfo, error) {
	var rows []struct {
		Name         string `bun:"name"`
		SessionCount int    `bun:"session_count"`
	}
	err := s.view(ctx, func(store bun.IDB) error {
		query := store.NewSelect().Table("sessions").
			ColumnExpr("project AS name").
			ColumnExpr("COUNT(*) AS session_count")
		return applyBunRootVisibility(query, excludeOneShot, excludeAutomated).
			Group("project").OrderExpr("project ASC").Scan(ctx, &rows)
	})
	if err != nil {
		return nil, fmt.Errorf("querying projects: %w", err)
	}
	projects := make([]ProjectInfo, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, ProjectInfo{
			Name: row.Name, SessionCount: row.SessionCount,
		})
	}
	return projects, nil
}

// GetActiveProjectLabels returns all project labels on non-deleted sessions.
func (s *BunStore) GetActiveProjectLabels(ctx context.Context) ([]string, error) {
	var labels []string
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Table("sessions").Distinct().Column("project").
			Where("deleted_at IS NULL").OrderExpr("project ASC").Scan(ctx, &labels)
	})
	if err != nil {
		return nil, fmt.Errorf("querying active project labels: %w", err)
	}
	return labels, nil
}

// GetAgents returns visible root-session counts by agent.
func (s *BunStore) GetAgents(
	ctx context.Context, excludeOneShot, excludeAutomated bool,
) ([]AgentInfo, error) {
	var rows []struct {
		Name         string `bun:"name"`
		SessionCount int    `bun:"session_count"`
	}
	err := s.view(ctx, func(store bun.IDB) error {
		query := store.NewSelect().Table("sessions").
			ColumnExpr("agent AS name").ColumnExpr("COUNT(*) AS session_count").
			Where("agent <> ''")
		return applyBunRootVisibility(query, excludeOneShot, excludeAutomated).
			Group("agent").OrderExpr("agent ASC").Scan(ctx, &rows)
	})
	if err != nil {
		return nil, fmt.Errorf("querying agents: %w", err)
	}
	agents := make([]AgentInfo, 0, len(rows))
	for _, row := range rows {
		agents = append(agents, AgentInfo{
			Name: row.Name, SessionCount: row.SessionCount,
		})
	}
	return agents, nil
}

// GetMachines returns distinct machine names under the requested visibility.
func (s *BunStore) GetMachines(
	ctx context.Context, excludeOneShot, excludeAutomated bool,
) ([]string, error) {
	var machines []string
	err := s.view(ctx, func(store bun.IDB) error {
		query := store.NewSelect().Table("sessions").Distinct().Column("machine").
			Where("deleted_at IS NULL")
		if excludeOneShot {
			if excludeAutomated {
				query = query.Where("user_message_count > 1")
			} else {
				query = query.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
					return q.Where("user_message_count > 1").WhereOr("is_automated = ?", true)
				})
			}
		}
		if excludeAutomated {
			query = query.Where("is_automated = ?", false)
		}
		return query.OrderExpr("machine ASC").Scan(ctx, &machines)
	})
	if err != nil {
		return nil, fmt.Errorf("querying machines: %w", err)
	}
	return machines, nil
}

// GetBranches returns distinct project/branch pairs for visible roots.
func (s *BunStore) GetBranches(
	ctx context.Context, excludeOneShot, excludeAutomated bool,
) ([]BranchInfo, error) {
	var rows []struct {
		Project string `bun:"project"`
		Branch  string `bun:"git_branch"`
	}
	err := s.view(ctx, func(store bun.IDB) error {
		query := store.NewSelect().Table("sessions").Distinct().
			Column("project", "git_branch")
		return applyBunRootVisibility(query, excludeOneShot, excludeAutomated).
			OrderExpr("project ASC").OrderExpr("git_branch ASC").Scan(ctx, &rows)
	})
	if err != nil {
		return nil, fmt.Errorf("querying branches: %w", err)
	}
	branches := make([]BranchInfo, 0, len(rows))
	for _, row := range rows {
		branches = append(branches, BranchInfo{
			Project: row.Project, Branch: row.Branch,
			Token: EncodeBranchFilterToken(row.Project, row.Branch),
		})
	}
	return branches, nil
}
