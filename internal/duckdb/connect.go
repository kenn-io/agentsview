package duckdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"runtime"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

const quackAttachmentName = "agentsview_remote"

// Open opens a local DuckDB file for the agentsview mirror backend.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("duckdb path is required")
	}
	db, err := openDuckDB(path)
	if err != nil {
		return nil, fmt.Errorf("opening duckdb file: %w", err)
	}
	// DuckDB permits one writer per database file. Keeping a single
	// pooled connection avoids surprising file-lock contention while
	// the mirror sync path is still process-local.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := configureDuckDBThreads(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// ReadLastPushAt reads the local DuckDB push watermark for the optional
// PG-compatible target scope.
func ReadLastPushAt(local *db.DB, syncStateTarget string) (string, error) {
	if local == nil {
		return "", fmt.Errorf("local sync state is required")
	}
	return local.GetSyncState(
		scopedDuckDBSyncStateKey(lastPushStateKey, syncStateTarget),
	)
}

// SyncStateTargetForConfig returns the local sync-state scope for a DuckDB
// target. Local file mirrors keep the historical unscoped watermark; remote
// Quack targets get a non-secret URL fingerprint so distinct remotes cannot
// reuse each other's push watermark.
func SyncStateTargetForConfig(cfg config.DuckDBConfig) string {
	if strings.TrimSpace(cfg.URL) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(canonicalDuckDBSyncTarget(cfg.URL)))
	encoded := hex.EncodeToString(sum[:])
	return "url-" + encoded[:16]
}

func canonicalDuckDBSyncTarget(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if !strings.HasPrefix(rawURL, "quack:") {
		return rawURL
	}
	transport := strings.TrimPrefix(rawURL, "quack:")
	if strings.HasPrefix(transport, "http://") ||
		strings.HasPrefix(transport, "https://") {
		if u, err := neturl.Parse(transport); err == nil {
			u.User = nil
			q := u.Query()
			for key := range q {
				if isSecretURLQueryKey(key) {
					q.Del(key)
				}
			}
			u.RawQuery = q.Encode()
			u.Fragment = ""
			return "quack:" + u.String()
		}
	}
	transport = strings.SplitN(transport, "#", 2)[0]
	transport = strings.SplitN(transport, "?", 2)[0]
	if at := strings.LastIndex(transport, "@"); at >= 0 {
		transport = transport[at+1:]
	}
	return "quack:" + transport
}

func isSecretURLQueryKey(key string) bool {
	lower := strings.ToLower(key)
	return isCredentialQueryKey(lower, "auth") ||
		isCredentialQueryKey(lower, "token") ||
		isCredentialQueryKey(lower, "secret") ||
		isCredentialQueryKey(lower, "password") ||
		isCredentialQueryKey(lower, "key")
}

func isCredentialQueryKey(key, credential string) bool {
	if key == credential {
		return true
	}
	for _, sep := range []string{"_", "-", "."} {
		if strings.HasSuffix(key, sep+credential) {
			return true
		}
	}
	return false
}

// ReadStatusFromConfig reads DuckDB/Quack row counts without requiring a local
// Sync handle. Callers pass any local last-push watermark they want displayed.
func ReadStatusFromConfig(
	ctx context.Context,
	cfg config.DuckDBConfig,
	lastPush string,
) (SyncStatus, error) {
	if cfg.MachineName == "" {
		return SyncStatus{}, fmt.Errorf("machine name must not be empty")
	}
	store, err := NewStoreFromConfig(cfg)
	if err != nil {
		return SyncStatus{}, err
	}
	defer store.Close()
	return readMachineStatus(
		ctx, store.DB(), store.connectionKind, store.quack,
		cfg.MachineName, lastPush,
	)
}

func readMachineStatus(
	ctx context.Context,
	duck *sql.DB,
	connectionKind duckDBConnectionKind,
	quack *quackClient,
	machine string,
	lastPush string,
) (SyncStatus, error) {
	status := SyncStatus{Machine: machine, LastPushAt: lastPush}
	if err := queryDuckDBRowContext(ctx, duck, connectionKind, quack,
		`SELECT COUNT(*) FROM sessions WHERE machine = ?`,
		machine,
	).Scan(&status.DuckDBSessions); err != nil {
		if isMissingDuckDBTable(err) {
			return status, nil
		}
		return SyncStatus{}, fmt.Errorf("counting duckdb sessions: %w", err)
	}
	if err := queryDuckDBRowContext(ctx, duck, connectionKind, quack,
		`SELECT COUNT(*)
		 FROM messages
		 WHERE session_id IN (
			SELECT id FROM sessions WHERE machine = ?
		 )`,
		machine,
	).Scan(&status.DuckDBMessages); err != nil {
		if isMissingDuckDBTable(err) {
			return status, nil
		}
		return SyncStatus{}, fmt.Errorf("counting duckdb messages: %w", err)
	}
	return status, nil
}

func isMissingDuckDBTable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "does not exist") ||
		strings.Contains(message, "table with name")
}

// NewStoreFromConfig opens either a local DuckDB mirror file or a remote
// Quack endpoint. Quack endpoints are attached as the default catalog so the
// Store's unqualified read queries work for both local and remote modes.
func NewStoreFromConfig(cfg config.DuckDBConfig) (*Store, error) {
	if cfg.URL != "" {
		return NewQuackStore(cfg.URL, cfg.Token, cfg.AllowInsecure)
	}
	return NewStore(cfg.Path)
}

// ValidatePushTarget validates remote DuckDB push targets without opening a
// DuckDB connection. Local file targets are validated when opened.
func ValidatePushTarget(cfg config.DuckDBConfig) error {
	if cfg.URL == "" {
		return nil
	}
	return ValidateQuackClientURL(cfg.URL, cfg.Token, cfg.AllowInsecure)
}

// NewFromConfig opens either a local DuckDB mirror file or a remote Quack
// endpoint for push sync.
func NewFromConfig(
	cfg config.DuckDBConfig, local *db.DB, opts SyncOptions,
) (*Sync, error) {
	if err := validateSyncInputs(local, cfg.MachineName); err != nil {
		return nil, err
	}
	var (
		duck           *sql.DB
		quack          *quackClient
		err            error
		connectionKind = duckDBBaseConnection
	)
	if cfg.URL != "" {
		quack, err = openQuackClient(cfg.URL, cfg.Token, cfg.AllowInsecure)
		if err == nil {
			duck = quack.DB()
		}
		connectionKind = duckDBQuackClientConnection
	} else {
		duck, err = Open(cfg.Path)
	}
	if err != nil {
		return nil, err
	}
	backfillTargetScope := opts.SyncStateTarget
	if backfillTargetScope == "" {
		if cfg.URL == "" {
			backfillTargetScope = localDuckDBBackfillTargetScope(cfg.Path)
		} else {
			backfillTargetScope = SyncStateTargetForConfig(cfg)
		}
	}
	return &Sync{
		duck:                  duck,
		local:                 local,
		machine:               cfg.MachineName,
		syncStateScope:        opts.SyncStateTarget,
		backfillTargetScope:   backfillTargetScope,
		projects:              opts.Projects,
		excludeProjects:       opts.ExcludeProjects,
		connectionKind:        connectionKind,
		quack:                 quack,
		maintenance:           duckDBCheckpointMaintenance{},
		loadCodexPushMessages: local.GetAllMessages,
	}, nil
}

// NewQuackStore attaches a remote DuckDB exposed over Quack.
func NewQuackStore(rawURL, token string, allowInsecure bool) (*Store, error) {
	client, err := openQuackClient(rawURL, token, allowInsecure)
	if err != nil {
		return nil, err
	}
	return &Store{
		duck:           client.DB(),
		quack:          client,
		connectionKind: duckDBQuackClientConnection,
	}, nil
}

// OpenQuack opens an in-memory DuckDB client and attaches a remote DuckDB
// exposed over Quack as the default catalog.
func OpenQuack(rawURL, token string, allowInsecure bool) (*sql.DB, error) {
	client, err := openQuackClient(rawURL, token, allowInsecure)
	if err != nil {
		return nil, err
	}
	return client.DB(), nil
}

type quackClient struct {
	duck       *sql.DB
	rawURL     string
	token      string
	reattachMu sync.Mutex
}

func openQuackClient(
	rawURL, token string, allowInsecure bool,
) (*quackClient, error) {
	if err := ValidateQuackClientURL(rawURL, token, allowInsecure); err != nil {
		return nil, err
	}
	conn, err := openDuckDB("")
	if err != nil {
		return nil, fmt.Errorf("opening duckdb client: %w", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := configureDuckDBThreads(conn); err != nil {
		conn.Close()
		return nil, err
	}

	if _, err := conn.Exec("INSTALL quack"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("installing quack extension: %w", err)
	}
	if _, err := conn.Exec("LOAD quack"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("loading quack extension: %w", err)
	}
	client := &quackClient{
		duck:   conn,
		rawURL: rawURL,
		token:  token,
	}
	if err := client.attach(context.Background()); err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

func (q *quackClient) DB() *sql.DB { return q.duck }

func (q *quackClient) attach(ctx context.Context) error {
	conn, err := q.duck.Conn(ctx)
	if err != nil {
		return fmt.Errorf("opening duckdb client connection: %w", err)
	}
	defer conn.Close()
	return q.attachConn(ctx, conn)
}

func (q *quackClient) attachConn(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, quackAttachSQL(q.rawURL, q.token)); err != nil {
		return fmt.Errorf(
			"attaching quack endpoint %s: %w", RedactQuackURL(q.rawURL),
			redactQuackClientError(err, q.rawURL, q.token),
		)
	}
	if _, err := conn.ExecContext(ctx, "USE "+quackAttachmentName); err != nil {
		return fmt.Errorf("selecting quack catalog: %w", err)
	}
	return nil
}

func quackAttachSQL(rawURL, token string) string {
	attach := "ATTACH " + duckLiteral(rawURL) + " AS " + quackAttachmentName
	if token != "" {
		attach += " (TOKEN " + duckLiteral(token) + ")"
	}
	return attach
}

func (q *quackClient) reattachLocked(ctx context.Context) error {
	conn, err := q.duck.Conn(ctx)
	if err != nil {
		return fmt.Errorf("opening duckdb client connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE memory"); err != nil {
		return fmt.Errorf("selecting local duckdb catalog: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "DETACH "+quackAttachmentName); err != nil {
		if !isMissingQuackAttachmentError(err) {
			return fmt.Errorf(
				"detaching quack endpoint %s: %w",
				RedactQuackURL(q.rawURL), err,
			)
		}
	}
	return q.attachConn(ctx, conn)
}

func redactQuackClientError(err error, rawURL, token string) error {
	if err == nil {
		return nil
	}
	msg := redactQuackClientErrorMessage(err.Error(), rawURL, token)
	return errors.New(msg)
}

func redactQuackClientErrorMessage(message, rawURL, token string) string {
	redactedURL := RedactQuackURL(rawURL)
	message = redactQuackErrorValue(message, rawURL, redactedURL)
	if transport, ok := strings.CutPrefix(rawURL, "quack:"); ok {
		redactedTransport := strings.TrimPrefix(redactedURL, "quack:")
		message = redactQuackErrorValue(message, transport, redactedTransport)
	}
	message = redactQuackURLCredentialValues(message, rawURL)
	message = redactQuackErrorValue(message, token, "<redacted>")
	return message
}

func redactQuackErrorValue(message, value, replacement string) string {
	if value == "" {
		return message
	}
	message = strings.ReplaceAll(message, value, replacement)
	return strings.ReplaceAll(message, duckLiteral(value), duckLiteral(replacement))
}

func redactQuackCredentialValue(message, value string) string {
	message = redactQuackErrorValue(message, value, "<redacted>")
	if escaped := neturl.QueryEscape(value); escaped != value {
		message = redactQuackErrorValue(message, escaped, "<redacted>")
	}
	if escaped := neturl.PathEscape(value); escaped != value {
		message = redactQuackErrorValue(message, escaped, "<redacted>")
	}
	return message
}

func redactQuackURLCredentialValues(message, rawURL string) string {
	transport := strings.TrimPrefix(rawURL, "quack:")
	if strings.HasPrefix(transport, "http://") ||
		strings.HasPrefix(transport, "https://") {
		return redactHTTPQuackURLCredentialValues(message, transport)
	}
	return redactNativeQuackCredentialValues(message, transport)
}

func redactHTTPQuackURLCredentialValues(message, transport string) string {
	u, err := neturl.Parse(transport)
	if err != nil {
		return message
	}
	if username := u.User.Username(); username != "" {
		message = redactQuackCredentialValue(message, username)
	}
	if password, ok := u.User.Password(); ok {
		message = redactQuackCredentialValue(message, password)
	}
	for key, values := range u.Query() {
		if !isSecretURLQueryKey(key) {
			continue
		}
		for _, value := range values {
			message = redactQuackCredentialValue(message, value)
		}
	}
	return message
}

func redactNativeQuackCredentialValues(message, transport string) string {
	transport = strings.SplitN(transport, "#", 2)[0]
	base, rawQuery, hasQuery := strings.Cut(transport, "?")
	base = strings.TrimPrefix(base, "//")
	if scheme, rest, ok := strings.Cut(base, "://"); ok && scheme != "" {
		base = rest
	}
	if userinfo := nativeQuackUserinfo(base); userinfo != "" {
		for value := range strings.SplitSeq(userinfo, ":") {
			message = redactQuackCredentialValue(message, value)
		}
	}
	if !hasQuery {
		return message
	}
	q, err := neturl.ParseQuery(rawQuery)
	if err != nil {
		return message
	}
	for key, values := range q {
		if !isSecretURLQueryKey(key) {
			continue
		}
		for _, value := range values {
			message = redactQuackCredentialValue(message, value)
		}
	}
	return message
}

func nativeQuackUserinfo(base string) string {
	authority, _, hasPath := strings.Cut(base, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		return authority[:at]
	}
	if !hasPath {
		return ""
	}
	at := strings.LastIndex(base, "@")
	if at < 0 {
		return ""
	}
	userinfo := base[:at]
	userinfoHead, _, _ := strings.Cut(userinfo, "/")
	if !strings.Contains(userinfoHead, ":") {
		return ""
	}
	reattachedAuthority, _, hasReattachedPath := strings.Cut(base[at+1:], "/")
	if nativeQuackLooksLikeAuthority(reattachedAuthority) ||
		(hasReattachedPath && reattachedAuthority != "" &&
			!nativeQuackLooksLikeAuthority(userinfoHead)) {
		return userinfo
	}
	return ""
}

func nativeQuackLooksLikeAuthority(authority string) bool {
	if authority == "" {
		return false
	}
	host := authority
	if maybeHost, maybePort, ok := strings.Cut(authority, ":"); ok {
		if maybeHost != "" && allDigits(maybePort) {
			return true
		}
		host = maybeHost
	}
	return strings.Contains(host, ".") ||
		strings.EqualFold(host, "localhost") ||
		net.ParseIP(host) != nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (q *quackClient) queryRemote(
	ctx context.Context, sqlText string, retryStale bool,
) (*sql.Rows, error) {
	query := "SELECT * FROM " + quackAttachmentName + ".query(?)"
	rows, err := q.duck.QueryContext(ctx, query, sqlText)
	if err == nil || !retryStale || !isStaleQuackConnectionError(err) ||
		ctx.Err() != nil {
		return rows, err
	}
	q.reattachMu.Lock()
	defer q.reattachMu.Unlock()
	if reattachErr := q.reattachLocked(ctx); reattachErr != nil {
		return nil, fmt.Errorf(
			"%w; reattaching quack endpoint %s: %v",
			err, RedactQuackURL(q.rawURL), reattachErr,
		)
	}
	return q.duck.QueryContext(ctx, query, sqlText)
}

func (q *quackClient) execRemote(
	ctx context.Context, sqlText string, retryStale bool,
) error {
	query := "FROM " + quackAttachmentName + ".query(?)"
	_, err := q.duck.ExecContext(ctx, query, sqlText)
	if err == nil || !retryStale || !isStaleQuackConnectionError(err) ||
		ctx.Err() != nil {
		return err
	}
	q.reattachMu.Lock()
	defer q.reattachMu.Unlock()
	if reattachErr := q.reattachLocked(ctx); reattachErr != nil {
		return fmt.Errorf(
			"%w; reattaching quack endpoint %s: %v",
			err, RedactQuackURL(q.rawURL), reattachErr,
		)
	}
	_, err = q.duck.ExecContext(ctx, query, sqlText)
	return err
}

func isStaleQuackConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid connection id") ||
		isMissingQuackAttachmentError(err) ||
		(strings.Contains(msg, "failed to send message") &&
			strings.Contains(msg, "bad gateway"))
}

func isMissingQuackAttachmentError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "does not exist") &&
		!strings.Contains(msg, "database not found") {
		return false
	}
	return strings.Contains(msg, strings.ToLower(quackAttachmentName)) ||
		strings.Contains(msg, "table function with name query")
}

func configureDuckDBThreads(db *sql.DB) error {
	threads := duckDBThreadCount()
	if _, err := db.Exec(fmt.Sprintf("SET threads TO %d", threads)); err != nil {
		return fmt.Errorf("configuring duckdb threads: %w", err)
	}
	return nil
}

func duckDBThreadCount() int {
	threads := runtime.GOMAXPROCS(0)
	if threads < 1 {
		return 1
	}
	return threads
}

// ValidateQuackClientURL rejects unsafe remote client connections before the
// extension sees any token-bearing attach string.
func ValidateQuackClientURL(rawURL, token string, allowInsecure bool) error {
	if rawURL == "" {
		return fmt.Errorf("duckdb url is required")
	}
	if !strings.HasPrefix(rawURL, "quack:") {
		return fmt.Errorf("duckdb url must start with quack")
	}
	if token == "" {
		return fmt.Errorf(
			"duckdb quack token is required; set AGENTSVIEW_DUCKDB_TOKEN or [duckdb].token",
		)
	}
	transport := strings.TrimPrefix(rawURL, "quack:")
	if strings.HasPrefix(transport, "http://") ||
		strings.HasPrefix(transport, "https://") {
		// The Quack extension parses the string after "quack:" as a native
		// HOST:PORT authority and rejects URL-scheme forms at ATTACH time
		// with "Invalid Port". Reject them here with an actionable message
		// instead of surfacing the cryptic extension error.
		return fmt.Errorf(
			"duckdb quack url must use the native form quack:HOST:PORT; " +
				"the Quack extension does not accept http:// or https:// " +
				"client urls",
		)
	}
	host, err := quackURIHost(rawURL)
	if err != nil {
		return err
	}
	if !allowInsecure && !isLoopbackHost(host) {
		return fmt.Errorf(
			"duckdb native quack url host must be loopback unless allow_insecure is set",
		)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func duckLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// RedactQuackURL removes common token query fields from a URL before logging.
func RedactQuackURL(rawURL string) string {
	transport := strings.TrimPrefix(rawURL, "quack:")
	if !strings.HasPrefix(transport, "http://") &&
		!strings.HasPrefix(transport, "https://") {
		return "quack:" + redactNativeQuackTransport(transport)
	}
	u, err := neturl.Parse(transport)
	if err != nil {
		return "quack:<redacted>"
	}
	u.User = nil
	q := u.Query()
	for key := range q {
		if isSecretURLQueryKey(key) {
			q.Set(key, "<redacted>")
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return "quack:" + u.String()
}

func redactNativeQuackTransport(transport string) string {
	transport = strings.SplitN(transport, "#", 2)[0]
	base, rawQuery, hasQuery := strings.Cut(transport, "?")
	if at := strings.LastIndex(base, "@"); at >= 0 {
		base = base[at+1:]
	}
	if !hasQuery {
		return base
	}
	q, err := neturl.ParseQuery(rawQuery)
	if err != nil {
		return base
	}
	for key := range q {
		if isSecretURLQueryKey(key) {
			q.Set(key, "<redacted>")
		}
	}
	return base + "?" + q.Encode()
}

// ValidateQuackServeURI rejects accidental public Quack exposure unless the
// caller explicitly opted in. Quack exposes the full SQL surface of the DuckDB
// connection, so loopback binding is the safe default.
func ValidateQuackServeURI(uri string, allowOtherHostname bool) error {
	if uri == "" {
		return fmt.Errorf("duckdb quack bind uri is required")
	}
	if !strings.HasPrefix(uri, "quack:") {
		return fmt.Errorf("duckdb quack bind uri must start with quack")
	}
	host, err := quackURIHost(uri)
	if err != nil {
		return err
	}
	if !allowOtherHostname && !isLoopbackHost(host) {
		return fmt.Errorf(
			"duckdb quack bind host must be loopback unless allow_insecure is set",
		)
	}
	return nil
}

func quackURIHost(uri string) (string, error) {
	raw := strings.TrimPrefix(uri, "quack:")
	if raw == "" {
		return "localhost", nil
	}
	if strings.HasPrefix(raw, "//") {
		u, err := neturl.Parse("quack:" + raw)
		if err != nil {
			return "", fmt.Errorf("parsing duckdb quack bind uri: %w", err)
		}
		if u.Hostname() == "" {
			return "", fmt.Errorf("duckdb quack bind uri host is required")
		}
		return u.Hostname(), nil
	}
	if strings.HasPrefix(raw, "[") {
		end := strings.Index(raw, "]")
		if end < 0 {
			return "", fmt.Errorf("duckdb quack bind uri has invalid IPv6 host")
		}
		return raw[1:end], nil
	}
	host := raw
	if i := strings.LastIndex(raw, ":"); i > -1 {
		host = raw[:i]
	}
	if host == "" {
		return "", fmt.Errorf("duckdb quack bind uri host is required")
	}
	return host, nil
}
