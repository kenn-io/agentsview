// Package clickhouse mirrors the SQLite archive into ClickHouse and serves
// the read-only HTTP API from it. SQLite stays the archive; ClickHouse is a
// one-way mirror populated by `agentsview clickhouse push` and read by
// `agentsview clickhouse serve`.
package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// DefaultDatabase is the ClickHouse database the mirror uses when the
// configuration names none.
const DefaultDatabase = "agentsview"

// Target identifies one ClickHouse mirror.
type Target struct {
	// URL is a clickhouse-go DSN: clickhouse://user:pass@host:9000/db?secure=true
	// for the native protocol or https://host:8443/db for HTTP.
	URL string
	// Database overrides the DSN path. Empty falls back to the DSN path, then
	// DefaultDatabase.
	Database string
}

// DatabaseName returns the database the mirror tables live in.
func (t Target) DatabaseName() (string, error) {
	if t.Database != "" {
		if !validIdentifier(t.Database) {
			return "", fmt.Errorf("clickhouse database %q is not a valid identifier", t.Database)
		}
		return t.Database, nil
	}
	opt, err := parseDSN(t.URL)
	if err != nil {
		return "", fmt.Errorf("parsing clickhouse url: %w", err)
	}
	if opt.Auth.Database != "" {
		if !validIdentifier(opt.Auth.Database) {
			return "", fmt.Errorf("clickhouse database %q is not a valid identifier", opt.Auth.Database)
		}
		return opt.Auth.Database, nil
	}
	return DefaultDatabase, nil
}

// parseDSN parses a mirror URL. An https:// URL implies TLS, so the
// driver's secure=true parameter is added when the operator left it out.
func parseDSN(dsn string) (*clickhouse.Options, error) {
	normalized, err := normalizeDSN(dsn)
	if err != nil {
		return nil, err
	}
	return clickhouse.ParseDSN(normalized)
}

func normalizeDSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing clickhouse url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return dsn, nil
	}
	q := u.Query()
	if q.Has("secure") {
		return dsn, nil
	}
	q.Set("secure", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func validIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// CheckTransportSecurity rejects a target that would send credentials and
// session content in plaintext, or over TLS that skips certificate
// verification, to a non-loopback host. Native connections need secure=true
// without skip_verify; HTTP connections need the https scheme. allowInsecure
// skips the check, matching the PostgreSQL allow_insecure setting.
func CheckTransportSecurity(dsn string, allowInsecure bool) error {
	if allowInsecure {
		return nil
	}
	opt, err := parseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parsing clickhouse url: %w", err)
	}
	unverifiedTLS := opt.TLS != nil && opt.TLS.InsecureSkipVerify
	if opt.TLS != nil && !unverifiedTLS {
		return nil
	}
	for _, addr := range opt.Addr {
		if isLoopback(addr) {
			continue
		}
		if unverifiedTLS {
			return fmt.Errorf(
				"clickhouse url for %s disables TLS certificate verification; omit skip_verify, or set allow_insecure = true to accept unverified TLS",
				addr,
			)
		}
		fix := "add secure=true to the url"
		if opt.Protocol == clickhouse.HTTP {
			fix = "use an https:// url"
		}
		return fmt.Errorf(
			"clickhouse url for %s does not use TLS; %s, or set allow_insecure = true to accept plaintext",
			addr, fix,
		)
	}
	return nil
}

func isLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RedactDSN returns only the host list of a DSN for log and error text.
func RedactDSN(dsn string) string {
	opt, err := parseDSN(dsn)
	if err != nil || len(opt.Addr) == 0 {
		return "<invalid clickhouse url>"
	}
	return strings.Join(opt.Addr, ",")
}

// TargetFingerprint identifies a mirror destination so a later change of
// host, user, or database is detectable without storing credentials.
func TargetFingerprint(t Target) (string, error) {
	opt, err := parseDSN(t.URL)
	if err != nil {
		return "", fmt.Errorf("parsing clickhouse url: %w", err)
	}
	database, err := t.DatabaseName()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range append([]string{"v1", database, opt.Auth.Username}, opt.Addr...) {
		fmt.Fprintf(h, "%d:%s", len(part), strings.ToLower(part))
	}
	return "v1:" + hex.EncodeToString(h.Sum(nil)), nil
}

// The pool and thread defaults trade one query's speed for many queries at
// once: the Analytics page sends 11 requests together, and on a 32-thread
// host 5 queries at ClickHouse's default of one thread per core already
// spent seconds each waiting for CPU. Both are starting points to retune
// per host with the URL's max_open_conns and max_threads parameters; how
// much kernel time each query thread costs depends on the server's storage.
const (
	// defaultMaxOpenConns lets every Analytics panel's query start at once.
	defaultMaxOpenConns = 12
	// pageMaxOpenConns sizes the serve pool kept for the session list,
	// sidebar filters, and one-session reads, so dashboard and Usage scans
	// on the main pool never hold up page navigation.
	pageMaxOpenConns = 4
	// defaultMaxThreads caps each query's threads so a dozen concurrent
	// queries share the cores instead of queueing for them.
	defaultMaxThreads = 8
	connMaxLifetime   = 30 * time.Minute
	connMaxIdleTime   = 5 * time.Minute
	pingTimeout       = 10 * time.Second
)

// Open connects to the mirror database with the settings every mirror
// connection needs: final=1 so ReplacingMergeTree deduplication applies to
// every read without FINAL in query text, and the pool and thread limits
// described above.
func Open(ctx context.Context, t Target) (*sql.DB, error) {
	database, err := t.DatabaseName()
	if err != nil {
		return nil, err
	}
	return openDatabase(ctx, t.URL, database, 0)
}

// openPages opens the serve pool for page reads; see pageMaxOpenConns.
func openPages(ctx context.Context, t Target) (*sql.DB, error) {
	database, err := t.DatabaseName()
	if err != nil {
		return nil, err
	}
	return openDatabase(ctx, t.URL, database, pageMaxOpenConns)
}

// OpenForAdmin connects to a database that already exists on the server so
// EnsureSchema can CREATE the mirror database. The DSN path is often the
// not-yet-created mirror, so the bootstrap database is `default`.
func OpenForAdmin(ctx context.Context, t Target) (*sql.DB, error) {
	return openDatabase(ctx, t.URL, "default", 0)
}

// openDatabase opens a pool of poolSize connections, or of the URL's
// max_open_conns (default defaultMaxOpenConns) when poolSize is 0.
func openDatabase(ctx context.Context, dsn, database string, poolSize int) (*sql.DB, error) {
	opt, err := connectOptions(dsn, database, poolSize)
	if err != nil {
		return nil, err
	}
	conn := clickhouse.OpenDB(opt)
	conn.SetMaxOpenConns(opt.MaxOpenConns)
	conn.SetMaxIdleConns(opt.MaxIdleConns)
	conn.SetConnMaxLifetime(connMaxLifetime)
	conn.SetConnMaxIdleTime(connMaxIdleTime)
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := conn.PingContext(pingCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connecting to clickhouse at %s: %w", RedactDSN(dsn), err)
	}
	return conn, nil
}

// openBulk opens a native-protocol pool, with the settings Open applies, for
// reads that return rows by the million. database/sql hands Scan every value
// boxed in an interface, with reflection for nullable columns and a text
// round trip into int fields; the native API scans into typed destinations
// at a fraction of that cost.
func openBulk(ctx context.Context, t Target) (clickhouse.Conn, error) {
	database, err := t.DatabaseName()
	if err != nil {
		return nil, err
	}
	opt, err := connectOptions(t.URL, database, 0)
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(opt)
	if err != nil {
		return nil, fmt.Errorf("opening clickhouse native connection at %s: %w", RedactDSN(t.URL), err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		return nil, errors.Join(
			fmt.Errorf("connecting to clickhouse at %s: %w", RedactDSN(t.URL), err), conn.Close())
	}
	return conn, nil
}

// connectOptions parses dsn and applies the mirror's connection settings.
// A max_threads or max_open_conns in the URL wins over the default.
func connectOptions(dsn, database string, poolSize int) (*clickhouse.Options, error) {
	opt, err := parseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing clickhouse url: %w", err)
	}
	opt.Auth.Database = database
	if opt.Settings == nil {
		opt.Settings = clickhouse.Settings{}
	}
	opt.Settings["final"] = 1
	if _, ok := opt.Settings["max_threads"]; !ok {
		opt.Settings["max_threads"] = defaultMaxThreads
	}
	switch {
	case poolSize > 0:
		opt.MaxOpenConns = poolSize
	case opt.MaxOpenConns <= 0:
		opt.MaxOpenConns = defaultMaxOpenConns
	}
	opt.MaxIdleConns = opt.MaxOpenConns
	opt.ConnMaxLifetime = connMaxLifetime
	return opt, nil
}

// ClickHouse error codes the mirror inspects.
const (
	codeUnsupportedMethod  = 1
	codeAccessDenied       = 497
	codeUnknownTable       = 60
	codeTableAlreadyExists = 57
	codeUnknownDatabase    = 81
	codeUnknownIdentifier  = 47
)

// IsPermissionError reports whether err is a ClickHouse "not enough
// privileges" failure, so serve can tolerate a read-only role that cannot
// run schema statements.
func IsPermissionError(err error) bool {
	ex, ok := errors.AsType[*clickhouse.Exception](err)
	return ok && ex.Code == codeAccessDenied
}

func isMissingTableError(err error) bool {
	ex, ok := errors.AsType[*clickhouse.Exception](err)
	return ok &&
		(ex.Code == codeUnknownTable || ex.Code == codeUnknownDatabase)
}
