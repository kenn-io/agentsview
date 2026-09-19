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

const (
	maxOpenConns    = 5
	connMaxLifetime = 30 * time.Minute
	connMaxIdleTime = 5 * time.Minute
	pingTimeout     = 10 * time.Second
)

// Open connects to the mirror database with the settings every mirror
// connection needs: final=1 so ReplacingMergeTree deduplication applies to
// every read without FINAL in query text.
func Open(ctx context.Context, t Target) (*sql.DB, error) {
	database, err := t.DatabaseName()
	if err != nil {
		return nil, err
	}
	return openDatabase(ctx, t.URL, database)
}

// OpenForAdmin connects to a database that already exists on the server so
// EnsureSchema can CREATE the mirror database. The DSN path is often the
// not-yet-created mirror, so the bootstrap database is `default`.
func OpenForAdmin(ctx context.Context, t Target) (*sql.DB, error) {
	return openDatabase(ctx, t.URL, "default")
}

func openDatabase(ctx context.Context, dsn, database string) (*sql.DB, error) {
	opt, err := parseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing clickhouse url: %w", err)
	}
	opt.Auth.Database = database
	if opt.Settings == nil {
		opt.Settings = clickhouse.Settings{}
	}
	opt.Settings["final"] = 1
	opt.MaxOpenConns = maxOpenConns
	opt.MaxIdleConns = maxOpenConns
	opt.ConnMaxLifetime = connMaxLifetime
	conn := clickhouse.OpenDB(opt)
	conn.SetMaxOpenConns(maxOpenConns)
	conn.SetMaxIdleConns(maxOpenConns)
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

// ClickHouse error codes the mirror inspects.
const (
	codeAccessDenied      = 497
	codeUnknownTable      = 60
	codeUnknownDatabase   = 81
	codeUnknownIdentifier = 47
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
