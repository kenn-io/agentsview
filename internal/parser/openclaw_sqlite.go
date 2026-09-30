package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const openClawSQLiteDBName = "openclaw-agent.sqlite"

type openClawSQLiteUnsupportedSchemaError struct {
	reason string
}

func (err openClawSQLiteUnsupportedSchemaError) Error() string {
	return "openclaw SQLite: unsupported schema: " + err.reason
}

type openClawSQLiteMalformedMemberError struct {
	reason string
}

func (err openClawSQLiteMalformedMemberError) Error() string {
	return "openclaw SQLite: malformed member: " + err.reason
}

func newOpenClawSQLiteSourceSet(
	roots []string,
) multiSessionContainerSourceSet {
	return NewMultiSessionContainerSourceSet(
		AgentOpenClaw,
		roots,
		WithStreamingSourceDiscovery(openClawSQLiteDiscoverEach),
		WithWatchRoots(openClawSQLiteWatchRoots),
		WithChangedPathClassifier(openClawSQLiteClassifyPath),
		WithMemberLookup(openClawSQLiteFindMember),
		WithReconciliationIdentity(
			func(_ context.Context, match multiSessionMatch) (string, error) {
				return match.MemberID, nil
			},
			func(fullSessionID string) string {
				return ProviderRawSessionIDFromFull(
					AgentDef{IDPrefix: "openclaw:"}, fullSessionID,
				)
			},
		),
		WithContextFingerprint(openClawSQLiteFingerprint),
		WithContainerParseOutcome(openClawSQLiteParseContainerOutcome),
		WithContextMemberParse(openClawSQLiteParseMember),
		WithMemberPresence(openClawSQLiteMemberPresent),
		WithBatchMemberPresence(openClawSQLiteBatchMemberPresence),
	)
}

func openClawSQLiteDiscoverEach(
	ctx context.Context,
	root string,
	yield func(multiSessionMatch) error,
) error {
	return openClawSQLiteDiscoverEachWithEnumerator(
		ctx, root, yield, openClawSQLiteSessionIDsEach,
	)
}

type openClawSQLiteSessionEnumerator func(
	context.Context, string, func(string, SourceFingerprint) error,
) error

func openClawSQLiteDiscoverEachWithEnumerator(
	ctx context.Context,
	root string,
	yield func(multiSessionMatch) error,
	enumerate openClawSQLiteSessionEnumerator,
) error {
	if root == "" {
		return nil
	}
	var incomplete error
	var callbackErr error
	err := streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
		if !IsValidSessionID(entry.Name()) {
			return nil
		}
		isDir, err := streamingDirCandidateOrIncomplete(
			AgentOpenClaw, "OpenClaw agent directory", entry, root,
		)
		if err != nil {
			return err
		}
		if !isDir {
			return nil
		}
		dbPath := openClawSQLiteDBPath(root, entry.Name())
		regular, err := openClawSQLiteRegularFile(dbPath)
		if err != nil {
			callbackErr = yield(multiSessionMatch{
				Path:        dbPath,
				Container:   dbPath,
				ProjectHint: entry.Name(),
			})
			return callbackErr
		}
		if !regular {
			return nil
		}
		emitted := 0
		err = enumerate(ctx, dbPath, func(
			sessionID string, fingerprint SourceFingerprint,
		) error {
			callbackErr = yield(multiSessionMatch{
				Path:                   VirtualSourcePath(dbPath, entry.Name()+":"+sessionID),
				Container:              dbPath,
				MemberID:               entry.Name() + ":" + sessionID,
				ReconciliationIdentity: entry.Name() + ":" + sessionID,
				ProjectHint:            entry.Name(),
				DiscoveryMTimeNS:       fingerprint.MTimeNS,
				DiscoveryFingerprint:   &fingerprint,
			})
			if callbackErr != nil {
				return callbackErr
			}
			emitted++
			return nil
		})
		if callbackErr != nil {
			return callbackErr
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, context.Canceled) || errors.Is(
				err, context.DeadlineExceeded,
			) {
				return err
			}
			if emitted == 0 {
				callbackErr = yield(multiSessionMatch{
					Path:        dbPath,
					Container:   dbPath,
					ProjectHint: entry.Name(),
				})
				return callbackErr
			}
			incomplete = errors.Join(incomplete, incompleteDiscoveryError(
				AgentOpenClaw,
				"enumerate OpenClaw SQLite sessions "+dbPath,
				err,
			))
		}
		return nil
	})
	if err != nil {
		if callbackErr != nil {
			return callbackErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		incomplete = errors.Join(incomplete, incompleteDiscoveryError(
			AgentOpenClaw,
			"read OpenClaw agent directory "+root,
			err,
		))
	}
	return incomplete
}

func openClawSQLiteWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:      root,
			Recursive: true,
			IncludeGlobs: []string{
				openClawSQLiteDBName,
				openClawSQLiteDBName + "-*",
			},
			DebounceKey: string(AgentOpenClaw) + ":sqlite:" + root,
		})
	}
	return out
}

func openClawSQLiteDBPath(root, agentID string) string {
	return filepath.Join(root, agentID, "agent", openClawSQLiteDBName)
}

func openClawSQLiteClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if dbPath, memberID, ok := ParseVirtualSourcePathForBase(
		path, openClawSQLiteDBName,
	); ok {
		agentID, valid := openClawSQLiteAgentForDB(root, dbPath)
		if !valid || !openClawSQLiteMemberMatchesAgent(memberID, agentID) {
			return multiSessionMatch{}, false
		}
		match, ok := classifySQLiteContainerPath(
			filepath.Join(root, agentID),
			path,
			filepath.ToSlash(filepath.Join("agent", openClawSQLiteDBName)),
			allowMissing,
			true,
			openClawSQLiteParseVirtualPath,
		)
		if !ok {
			return multiSessionMatch{}, false
		}
		match.Path = path
		match.Container = dbPath
		match.MemberID = memberID
		return match, true
	}

	agentID, ok := openClawSQLiteAgentForEvent(root, path)
	if !ok {
		return multiSessionMatch{}, false
	}
	match, ok := classifySQLiteContainerPath(
		filepath.Join(root, agentID),
		path,
		filepath.ToSlash(filepath.Join("agent", openClawSQLiteDBName)),
		allowMissing,
		true,
		openClawSQLiteParseVirtualPath,
	)
	if !ok {
		return multiSessionMatch{}, false
	}
	match.Path = match.Container
	return match, true
}

func openClawSQLiteAgentForEvent(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[1] != "agent" ||
		!IsValidSessionID(parts[0]) {
		return "", false
	}
	base := parts[2]
	if base != openClawSQLiteDBName &&
		!strings.HasPrefix(base, openClawSQLiteDBName+"-") {
		return "", false
	}
	return parts[0], true
}

func openClawSQLiteAgentForDB(root, dbPath string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(dbPath))
	if !ok {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[1] != "agent" ||
		parts[2] != openClawSQLiteDBName || !IsValidSessionID(parts[0]) {
		return "", false
	}
	return parts[0], true
}

func openClawSQLiteParseVirtualPath(
	path string,
) (string, string, bool) {
	dbPath, memberID, ok := ParseVirtualSourcePathForBase(
		path, openClawSQLiteDBName,
	)
	if !ok || !openClawSQLiteMemberIDValid(memberID) {
		return "", "", false
	}
	return dbPath, memberID, true
}

func openClawSQLiteMemberIDValid(memberID string) bool {
	agentID, sessionID, ok := strings.Cut(memberID, ":")
	return ok && IsValidSessionID(agentID) && IsValidSessionID(sessionID)
}

func openClawSQLiteMemberMatchesAgent(memberID, agentID string) bool {
	memberAgent, _, ok := strings.Cut(memberID, ":")
	return ok && memberAgent == agentID
}

func openClawSQLiteMemberParts(memberID string) (string, string, bool) {
	agentID, sessionID, ok := strings.Cut(memberID, ":")
	if !ok || !IsValidSessionID(agentID) || !IsValidSessionID(sessionID) {
		return "", "", false
	}
	return agentID, sessionID, true
}

func openClawSQLiteFindMember(
	ctx context.Context, root, rawID string,
) (multiSessionMatch, bool) {
	match, found, _ := openClawSQLiteFindMemberChecked(ctx, root, rawID)
	return match, found
}

func openClawSQLiteFindMemberChecked(
	ctx context.Context, root, rawID string,
) (multiSessionMatch, bool, error) {
	agentID, sessionID, ok := openClawSQLiteMemberParts(rawID)
	if !ok || root == "" {
		return multiSessionMatch{}, false, nil
	}
	dbPath := openClawSQLiteDBPath(root, agentID)
	regular, err := openClawSQLiteRegularFile(dbPath)
	if err != nil {
		return multiSessionMatch{}, false, fmt.Errorf(
			"stat OpenClaw SQLite database %s: %w", dbPath, err,
		)
	}
	if !regular {
		return multiSessionMatch{}, false, nil
	}
	found, discoveryMTimeNS, err := openClawSQLiteMemberExistsWithMTime(
		ctx, dbPath, sessionID,
	)
	if err != nil || !found {
		return multiSessionMatch{}, found, err
	}
	return multiSessionMatch{
		Path:                   VirtualSourcePath(dbPath, rawID),
		Container:              dbPath,
		MemberID:               rawID,
		ReconciliationIdentity: rawID,
		DiscoveryMTimeNS:       discoveryMTimeNS,
	}, true, nil
}

func openClawSQLiteRegularFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

func openClawSQLiteMemberExists(
	ctx context.Context, dbPath, sessionID string,
) (bool, error) {
	found, _, err := openClawSQLiteMemberExistsWithMTime(ctx, dbPath, sessionID)
	return found, err
}

func openClawSQLiteMemberExistsWithMTime(
	ctx context.Context, dbPath, sessionID string,
) (bool, int64, error) {
	db, tx, err := openOpenClawSQLiteTx(ctx, dbPath)
	if err != nil {
		return false, 0, err
	}
	defer db.Close()
	defer func() { _ = tx.Rollback() }()
	if err := openClawSQLiteInspectSchema(ctx, tx); err != nil {
		return false, 0, err
	}
	var createdAt any
	err = tx.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM transcript_events WHERE session_id = ?`,
		sessionID,
	).Scan(&createdAt)
	if err != nil {
		return false, 0, fmt.Errorf("looking up session %s: %w", sessionID, err)
	}
	if createdAt == nil {
		return false, 0, nil
	}
	return true, openClawSQLiteCreatedAtMTimeNS(createdAt), nil
}

func openClawSQLiteMemberPresent(
	ctx context.Context, src multiSessionSource,
) bool {
	if err := ctx.Err(); err != nil {
		return true
	}
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	found, err := openClawSQLiteMemberExists(
		ctx, src.Container, openClawSQLiteSessionID(src.MemberID),
	)
	if err != nil {
		return true
	}
	return found
}

func openClawSQLiteBatchMemberPresence(
	ctx context.Context, container multiSessionSource,
	members []multiSessionSource,
) map[string]bool {
	presence := make(map[string]bool, len(members))
	for _, member := range members {
		presence[member.Path] = true
	}
	if len(members) == 0 || container.Container == "" ||
		!IsRegularFile(container.Container) || ctx.Err() != nil {
		return presence
	}
	db, tx, err := openOpenClawSQLiteTx(ctx, container.Container)
	if err != nil {
		return presence
	}
	defer db.Close()
	defer func() { _ = tx.Rollback() }()
	if err := openClawSQLiteInspectSchema(ctx, tx); err != nil {
		return presence
	}
	rows, err := tx.QueryContext(
		ctx, `SELECT DISTINCT session_id FROM transcript_events`,
	)
	if err != nil {
		return presence
	}
	defer rows.Close()
	ids := make(map[string]struct{})
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return presence
		}
		ids[sessionID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return presence
	}
	for _, member := range members {
		rawID := openClawSQLiteSessionID(member.MemberID)
		_, presence[member.Path] = ids[rawID]
	}
	return presence
}

func openClawSQLiteSessionID(memberID string) string {
	_, sessionID, ok := openClawSQLiteMemberParts(memberID)
	if !ok {
		return ""
	}
	return sessionID
}

func openClawSQLiteFingerprint(
	ctx context.Context, src multiSessionSource,
) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if errors.Is(err, os.ErrNotExist) {
		return SourceFingerprint{}, nil
	}
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	if !info.Mode().IsRegular() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is not a file", src.Container)
	}
	if src.MemberID == "" {
		mtime := info.ModTime().UnixNano()
		if composite, err := sqliteDBCompositeMtime(
			src.Container, sqliteDBJournalSuffixes,
		); err == nil {
			mtime = composite
		}
		hash, err := hashJSONLSourceFileContext(ctx, src.Container)
		if err != nil {
			return SourceFingerprint{}, err
		}
		return SourceFingerprint{Size: info.Size(), MTimeNS: mtime, Hash: hash}, nil
	}
	if src.DiscoveryFingerprint != nil {
		return *src.DiscoveryFingerprint, nil
	}

	db, tx, err := openOpenClawSQLiteTx(ctx, src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	defer db.Close()
	defer func() { _ = tx.Rollback() }()
	if err := openClawSQLiteInspectSchema(ctx, tx); err != nil {
		return SourceFingerprint{}, err
	}
	return openClawSQLiteMemberFingerprint(
		ctx, tx, openClawSQLiteSessionID(src.MemberID), nil,
	)
}

func openClawSQLiteParseMember(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	info, err := os.Stat(src.Container)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	agentID, sessionID, ok := openClawSQLiteMemberParts(src.MemberID)
	if !ok {
		return nil, fmt.Errorf("invalid OpenClaw SQLite member ID: %s", src.MemberID)
	}
	db, tx, err := openOpenClawSQLiteTx(ctx, src.Container)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	defer func() { _ = tx.Rollback() }()
	if err := openClawSQLiteInspectSchema(ctx, tx); err != nil {
		return nil, err
	}
	result, _, err := openClawSQLiteParseMemberTx(
		ctx, tx, src, req.Machine, info, agentID, sessionID,
	)
	return result, err
}

func openClawSQLiteParseContainerOutcome(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) (ParseOutcome, error) {
	info, err := os.Stat(src.Container)
	if errors.Is(err, os.ErrNotExist) {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if err != nil {
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	db, tx, err := openOpenClawSQLiteTx(ctx, src.Container)
	if err != nil {
		return ParseOutcome{}, err
	}
	defer db.Close()
	defer func() { _ = tx.Rollback() }()
	if err := openClawSQLiteInspectSchema(ctx, tx); err != nil {
		return ParseOutcome{}, err
	}
	ids, err := openClawSQLiteSessionIDsTx(ctx, tx)
	if err != nil {
		return ParseOutcome{}, err
	}
	agentID, ok := openClawSQLiteAgentForDB(src.Root, src.Container)
	if !ok {
		return ParseOutcome{}, fmt.Errorf(
			"OpenClaw SQLite database is outside configured agent root: %s",
			src.Container,
		)
	}
	results := make([]ParseResultOutcome, 0, len(ids))
	var sourceErrors []SourceError
	for _, sessionID := range ids {
		if err := ctx.Err(); err != nil {
			return ParseOutcome{}, err
		}
		memberID := agentID + ":" + sessionID
		member := multiSessionSource{
			Root:      src.Root,
			Path:      VirtualSourcePath(src.Container, memberID),
			Container: src.Container,
			MemberID:  memberID,
		}
		result, digest, err := openClawSQLiteParseMemberTx(
			ctx, tx, member, req.Machine, info, agentID, sessionID,
		)
		if err != nil {
			if _, ok := errors.AsType[openClawSQLiteMalformedMemberError](err); ok {
				sourceErrors = append(sourceErrors, SourceError{
					SourceKey:   member.Path,
					DisplayPath: member.Path,
					SessionID:   "openclaw:" + memberID,
					Err:         err,
				})
				continue
			}
			return ParseOutcome{}, err
		}
		if result == nil {
			continue
		}
		result.Session.File.Hash = digest
		results = append(results, ParseResultOutcome{
			Result:      *result,
			DataVersion: DataVersionCurrent,
		})
	}
	if len(results) == 0 && len(sourceErrors) == 0 {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	return ParseOutcome{
		Results:           results,
		SourceErrors:      sourceErrors,
		ResultSetComplete: len(sourceErrors) == 0,
		ForceReplace:      true,
	}, nil
}

func openClawSQLiteParseMemberTx(
	ctx context.Context,
	tx *sql.Tx,
	src multiSessionSource,
	machine string,
	info os.FileInfo,
	agentID, sessionID string,
) (*ParseResult, string, error) {
	builder := newOpenClawRecordBuilder()
	fingerprint, err := openClawSQLiteMemberFingerprint(
		ctx, tx, sessionID, builder,
	)
	if err != nil {
		return nil, "", err
	}
	if builder.sessionID != "" && builder.sessionID != sessionID {
		return nil, "", openClawSQLiteMalformedMemberError{
			reason: fmt.Sprintf(
				"header session ID %q does not match selected member %q",
				builder.sessionID, sessionID,
			),
		}
	}
	if fingerprint.Hash == "" {
		return nil, "", nil
	}
	sess, messages, err := builder.finish(
		src.Path, "", machine, info, agentID, sessionID,
	)
	if err != nil {
		return nil, "", err
	}
	if sess == nil {
		return nil, "", nil
	}
	sess.File.Size = fingerprint.Size
	sess.File.Mtime = fingerprint.MTimeNS
	sess.File.Hash = fingerprint.Hash
	return &ParseResult{Session: *sess, Messages: messages}, fingerprint.Hash, nil
}

func openClawSQLiteMemberFingerprint(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	builder *openClawRecordBuilder,
) (SourceFingerprint, error) {
	var fingerprint SourceFingerprint
	err := openClawSQLiteFingerprintsEachTx(ctx, tx, sessionID, builder,
		func(_ string, current SourceFingerprint) error {
			fingerprint = current
			return nil
		},
	)
	return fingerprint, err
}

// Stream one member at a time so discovery opens the database once, while
// retaining a content digest that detects timestamp-preserving edits and deletes.
func openClawSQLiteFingerprintsEachTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	builder *openClawRecordBuilder,
	yield func(string, SourceFingerprint) error,
) error {
	// Each transcript window belongs to a session node that carries the
	// session's title. Schemas without session nodes have no titles.
	titleExpr, err := openClawSQLiteTitleExpr(ctx, tx)
	if err != nil {
		return err
	}
	query := `SELECT e.session_id, e.seq, e.event_json, e.created_at, NULL
		FROM transcript_events e`
	if titleExpr != "" {
		query = `SELECT e.session_id, e.seq, e.event_json, e.created_at, ` + titleExpr + `
		FROM transcript_events e
		LEFT JOIN session_windows w ON w.session_id = e.session_id
		LEFT JOIN session_nodes n ON n.session_key = w.session_key`
	}
	var args []any
	if sessionID != "" {
		query += ` WHERE e.session_id = ?`
		args = append(args, sessionID)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY e.session_id, e.seq`, args...)
	if err != nil {
		return fmt.Errorf("reading OpenClaw SQLite events: %w", err)
	}
	defer rows.Close()
	hash := sha256.New()
	var memberID, title string
	var fingerprint SourceFingerprint
	finish := func() error {
		if memberID == "" {
			return nil
		}
		// A rename or a new generated title changes only the session node,
		// so the title must change the digest. Untitled sessions keep their
		// event-only digest.
		if title != "" {
			openClawSQLiteHashField(hash, "title")
			openClawSQLiteHashField(hash, title)
		}
		fingerprint.Hash = hex.EncodeToString(hash.Sum(nil))
		return yield(memberID, fingerprint)
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var (
			id        string
			seq       int64
			eventJSON string
			createdAt any
			rowTitle  sql.NullString
		)
		if err := rows.Scan(&id, &seq, &eventJSON, &createdAt, &rowTitle); err != nil {
			return fmt.Errorf("scanning OpenClaw SQLite event: %w", err)
		}
		if !IsValidSessionID(id) {
			continue
		}
		if id != memberID {
			if err := finish(); err != nil {
				return err
			}
			memberID = id
			title = rowTitle.String
			fingerprint = SourceFingerprint{}
			hash.Reset()
			if builder != nil {
				builder.sessionName = title
			}
		}
		openClawSQLiteHashField(hash, strconv.FormatInt(seq, 10))
		openClawSQLiteHashField(hash, openClawSQLiteValueString(createdAt))
		openClawSQLiteHashField(hash, eventJSON)
		fingerprint.Size += int64(len(eventJSON))
		fingerprint.MTimeNS = max(fingerprint.MTimeNS, openClawSQLiteCreatedAtMTimeNS(createdAt))
		if builder != nil {
			if err := builder.consume(eventJSON, true); err != nil {
				return openClawSQLiteMalformedMemberError{reason: err.Error()}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading OpenClaw SQLite events: %w", err)
	}
	return finish()
}

func openClawSQLiteHashField(hash interface{ Write([]byte) (int, error) }, value string) {
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(value))
}

func openClawSQLiteValueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case []byte:
		return string(typed)
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func openOpenClawSQLiteTx(
	ctx context.Context, path string,
) (*sql.DB, *sql.Tx, error) {
	db, err := openSQLiteReadOnly(path, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return nil, nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, tx, nil
}

func openClawSQLiteInspectSchema(
	ctx context.Context, tx *sql.Tx,
) error {
	columns, err := openClawSQLiteTableColumns(ctx, tx, "transcript_events")
	if err != nil {
		return err
	}
	for _, name := range []string{"session_id", "seq", "event_json", "created_at"} {
		if !columns[name] {
			return openClawSQLiteUnsupportedSchemaError{
				reason: "missing transcript_events." + name,
			}
		}
	}
	return nil
}

// openClawSQLiteTitleExpr returns the SQL expression that selects a session
// node's title, or "" when the database does not link transcript sessions to
// titled session nodes. The label the user set with /name or the web UI
// rename wins over the generated display_name.
func openClawSQLiteTitleExpr(
	ctx context.Context, tx *sql.Tx,
) (string, error) {
	windows, err := openClawSQLiteTableColumns(ctx, tx, "session_windows")
	if err != nil {
		return "", err
	}
	if !windows["session_id"] || !windows["session_key"] {
		return "", nil
	}
	nodes, err := openClawSQLiteTableColumns(ctx, tx, "session_nodes")
	if err != nil {
		return "", err
	}
	if !nodes["session_key"] {
		return "", nil
	}
	var titles []string
	for _, column := range []string{"label", "display_name"} {
		if nodes[column] {
			titles = append(titles, "NULLIF(TRIM(n."+column+"), '')")
		}
	}
	switch len(titles) {
	case 0:
		return "", nil
	case 1:
		return titles[0], nil
	default:
		return "COALESCE(" + strings.Join(titles, ", ") + ")", nil
	}
}

// openClawSQLiteTableColumns returns the column names of table, or an empty
// set when the table does not exist.
func openClawSQLiteTableColumns(
	ctx context.Context, tx *sql.Tx, table string,
) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf(
			"inspecting OpenClaw SQLite table %s: %w", table, err,
		)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf(
				"scanning OpenClaw SQLite table %s: %w", table, err,
			)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"reading OpenClaw SQLite table %s: %w", table, err,
		)
	}
	return columns, nil
}

func openClawSQLiteSessionIDsEach(
	ctx context.Context, path string, yield func(string, SourceFingerprint) error,
) error {
	db, tx, err := openOpenClawSQLiteTx(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	defer func() { _ = tx.Rollback() }()
	if err := openClawSQLiteInspectSchema(ctx, tx); err != nil {
		return err
	}
	return openClawSQLiteFingerprintsEachTx(ctx, tx, "", nil, yield)
}

func openClawSQLiteSessionIDsTx(
	ctx context.Context, tx *sql.Tx,
) ([]string, error) {
	var ids []string
	err := openClawSQLiteSessionIDsEachTx(ctx, tx, func(
		sessionID string, _ int64,
	) error {
		ids = append(ids, sessionID)
		return nil
	})
	return ids, err
}

func openClawSQLiteSessionIDsEachTx(
	ctx context.Context, tx *sql.Tx,
	yield func(string, int64) error,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, MAX(created_at)
		FROM transcript_events
		GROUP BY session_id
		ORDER BY session_id
	`)
	if err != nil {
		return fmt.Errorf(
			"listing OpenClaw SQLite sessions: %w", err,
		)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var createdAt any
		if err := rows.Scan(&sessionID, &createdAt); err != nil {
			return fmt.Errorf(
				"scanning OpenClaw SQLite session ID: %w", err,
			)
		}
		if !IsValidSessionID(sessionID) {
			continue
		}
		if err := yield(
			sessionID, openClawSQLiteCreatedAtMTimeNS(createdAt),
		); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf(
			"reading OpenClaw SQLite session IDs: %w", err,
		)
	}
	return nil
}

func openClawSQLiteCreatedAtMTimeNS(value any) int64 {
	milliseconds, err := strconv.ParseInt(
		openClawSQLiteValueString(value), 10, 64,
	)
	if err != nil {
		return 0
	}
	return time.UnixMilli(milliseconds).UnixNano()
}
