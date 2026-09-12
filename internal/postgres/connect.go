package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// RedactDSN returns the host portion of the DSN for diagnostics,
// stripping credentials, query parameters, and path components
// that may contain secrets.
func RedactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// CheckSSL returns an error when the PG connection string targets
// a non-loopback host without TLS encryption. It uses the pgx
// driver's own DSN parser to resolve the effective host and TLS
// configuration, avoiding bypasses from exotic DSN formats.
//
// A connection is rejected when any path in the TLS negotiation
// chain (primary + fallbacks) permits plaintext for a non-loopback
// host. This rejects sslmode=disable, allow, and prefer.
func CheckSSL(dsn string) error {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parsing pg connection string: %w", err)
	}
	if isLoopback(cfg.Host) {
		return nil
	}
	if hasPlaintextPath(cfg) {
		return fmt.Errorf(
			"pg connection to %s permits plaintext; "+
				"set sslmode=require (or verify-full) "+
				"for non-local hosts, "+
				"or set allow_insecure = true under [pg] or [pg.NAME] "+
				"in config to override",
			cfg.Host,
		)
	}
	return nil
}

// WarnInsecureSSL logs a warning when the PG connection string
// targets a non-loopback host without TLS encryption. Uses the
// pgx driver's DSN parser for accurate host/TLS resolution.
func WarnInsecureSSL(dsn string) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return
	}
	if isLoopback(cfg.Host) {
		return
	}
	if hasPlaintextPath(cfg) {
		log.Printf(
			"warning: pg connection to %s permits "+
				"plaintext; consider sslmode=require or "+
				"verify-full for non-local hosts",
			cfg.Host,
		)
	}
}

// hasPlaintextPath returns true if any path in the pgconn
// connection chain (primary config + fallbacks) has TLS disabled.
// This catches sslmode=disable (no TLS), sslmode=allow (plaintext
// first, TLS fallback), and sslmode=prefer (TLS first, plaintext
// fallback).
func hasPlaintextPath(cfg *pgconn.Config) bool {
	if cfg.TLSConfig == nil {
		return true
	}
	for _, fb := range cfg.Fallbacks {
		if fb.TLSConfig == nil {
			return true
		}
	}
	return false
}

// isLoopback returns true if host is a loopback address,
// localhost, a unix socket path, or empty (defaults to local
// connection).
func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	// Unix socket paths start with /
	if len(host) > 0 && host[0] == '/' {
		return true
	}
	return false
}

// validIdentifier matches simple SQL identifiers (letters,
// digits, underscores). Used to reject schema names that could
// enable SQL injection.
var validIdentifier = regexp.MustCompile(
	`^[a-zA-Z_][a-zA-Z0-9_]*$`,
)

// quoteIdentifier double-quotes a SQL identifier, escaping any
// embedded double quotes. Rejects empty or non-identifier strings
// to prevent injection.
func quoteIdentifier(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf(
			"schema name must not be empty",
		)
	}
	if !validIdentifier.MatchString(name) {
		return "", fmt.Errorf(
			"invalid schema name: %q", name,
		)
	}
	return `"` + name + `"`, nil
}

// Open opens a PG connection pool, validates SSL, and sets
// search_path to the given schema on every connection.
//
// The schema name is validated and quoted to prevent injection.
// When allowInsecure is true, non-loopback connections without
// TLS produce a warning instead of failing.
func Open(
	dsn, schema string, allowInsecure bool,
) (*sql.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres URL is required")
	}
	quoted, err := quoteIdentifier(schema)
	if err != nil {
		return nil, fmt.Errorf("invalid pg schema: %w", err)
	}

	if allowInsecure {
		WarnInsecureSSL(dsn)
	} else if err := CheckSSL(dsn); err != nil {
		return nil, err
	}

	// Append search_path and timezone as runtime parameters in
	// the DSN so every connection in the pool inherits them.
	// pgx's stdlib driver passes options through to ConnConfig.
	connStr, err := appendConnParams(dsn, map[string]string{
		"search_path": quoted,
		"TimeZone":    "UTC",
	})
	if err != nil {
		return nil, fmt.Errorf(
			"setting connection params: %w", err,
		)
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf(
			"opening pg (host=%s): %w",
			RedactDSN(dsn), err,
		)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf(
			"pg ping (host=%s): %w",
			RedactDSN(dsn), err,
		)
	}
	return db, nil
}

func pgTargetFingerprint(dsn, schema string) (string, error) {
	if dsn == "" {
		return "", fmt.Errorf("postgres URL is required")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing pg connection string: %w", err)
	}

	fields := []string{
		"v1",
		cfg.Database,
		cfg.User,
		schema,
	}
	appendTargetEndpoint := func(host string, port uint16) {
		if host != "" && !strings.HasPrefix(host, "/") {
			host = strings.ToLower(host)
		}
		fields = append(fields, host, fmt.Sprintf("%d", port))
	}
	appendTargetEndpoint(cfg.Host, cfg.Port)
	for _, fallback := range cfg.Fallbacks {
		appendTargetEndpoint(fallback.Host, fallback.Port)
	}
	h := sha256.New()
	for _, field := range fields {
		fmt.Fprintf(h, "%d:%s", len(field), field)
	}
	return "v1:" + hex.EncodeToString(h.Sum(nil)), nil
}

// appendConnParams injects key=value connection parameters into
// the DSN. For URI-style DSNs it adds query parameters; for
// key=value DSNs it appends key=value pairs.
func appendConnParams(
	dsn string, params map[string]string,
) (string, error) {
	// URI format: postgres://...
	if strings.HasPrefix(dsn, "postgres://") ||
		strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf(
				"parsing pg URI: %w", err,
			)
		}
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// Key=value format: append parameters.
	result := dsn
	for k, v := range params {
		if result != "" {
			result += " "
		}
		result += k + "=" + v
	}
	return result, nil
}

// OpenHosted opens and verifies a restricted pool permanently assigned to one
// tenant schema. Startup parameters override caller values on every connection;
// checkout reset restores context after connection reuse or caller SET commands.
func OpenHosted(dsn, schema, tenant string, allowInsecure bool) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return OpenHostedContext(ctx, dsn, schema, tenant, allowInsecure)
}

// OpenHostedContext opens a restricted hosted pool and uses ctx for its initial
// connection and tenant verification. Operations on the returned pool use the
// contexts supplied by their callers.
func OpenHostedContext(ctx context.Context, dsn, schema, tenant string, allowInsecure bool) (*sql.DB, error) {
	if err := validateHostedBinding(schema, tenant); err != nil {
		return nil, err
	}
	if dsn == "" {
		return nil, fmt.Errorf("postgres URL is required")
	}
	if allowInsecure {
		WarnInsecureSSL(dsn)
	} else if err := CheckSSL(dsn); err != nil {
		return nil, err
	}
	quoted, _ := quoteIdentifier(schema)
	connStr, err := appendConnParams(dsn, map[string]string{"search_path": quoted, "TimeZone": "UTC"})
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		return nil, err
	}
	// libpq options and role parameters must not override our startup boundary.
	delete(cfg.RuntimeParams, "options")
	delete(cfg.RuntimeParams, "role")
	// Naming pg_temp explicitly prevents its otherwise implicit first priority.
	searchPath := quoted + ", pg_temp"
	cfg.RuntimeParams["search_path"] = searchPath
	cfg.RuntimeParams["agentsview.tenant_id"] = tenant
	cfg.RuntimeParams["row_security"] = "on"
	database := stdlib.OpenDB(*cfg, stdlib.OptionResetSession(func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SELECT set_config('search_path',$1,false),set_config('agentsview.tenant_id',$2,false),set_config('row_security','on',false)`, searchPath, tenant)
		return err
	}))
	database.SetMaxOpenConns(5)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(30 * time.Minute)
	database.SetConnMaxIdleTime(5 * time.Minute)
	if err = CheckHostedTenant(ctx, database, schema, tenant); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}
