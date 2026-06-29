package duckdb

import (
	"database/sql"
	"fmt"
	"net"
	neturl "net/url"
	"runtime"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

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

// NewStoreFromConfig opens either a local DuckDB mirror file or a remote
// Quack endpoint. Quack endpoints are attached as the default catalog so the
// Store's unqualified read queries work for both local and remote modes.
func NewStoreFromConfig(cfg config.DuckDBConfig) (*Store, error) {
	if cfg.URL != "" {
		return NewQuackStore(cfg.URL, cfg.Token, cfg.AllowInsecure)
	}
	return NewStore(cfg.Path)
}

// NewQuackStore attaches a remote DuckDB exposed over Quack.
func NewQuackStore(rawURL, token string, allowInsecure bool) (*Store, error) {
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
	attach := "ATTACH " + duckLiteral(rawURL) + " AS agentsview_remote"
	if token != "" {
		attach += " (TOKEN " + duckLiteral(token) + ")"
	}
	if _, err := conn.Exec(attach); err != nil {
		conn.Close()
		return nil, fmt.Errorf(
			"attaching quack endpoint %s: %w",
			RedactQuackURL(rawURL), err,
		)
	}
	if _, err := conn.Exec("USE agentsview_remote"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("selecting quack catalog: %w", err)
	}
	return NewStoreFromDB(conn), nil
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
		return fmt.Errorf("duckdb quack token is required")
	}
	transport := strings.TrimPrefix(rawURL, "quack:")
	if !strings.HasPrefix(transport, "http://") &&
		!strings.HasPrefix(transport, "https://") {
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
	u, err := neturl.Parse(transport)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf(
			"duckdb quack url must include an http:// or https:// endpoint",
		)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("duckdb quack url must use http or https")
	}
	if u.Scheme == "http" && !allowInsecure && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf(
			"duckdb quack url uses plain HTTP for a non-loopback host; use https or set allow_insecure",
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
	u, err := neturl.Parse(transport)
	if err != nil {
		return "quack:<redacted>"
	}
	q := u.Query()
	for _, key := range []string{"token", "access_token", "auth"} {
		if q.Has(key) {
			q.Set(key, "<redacted>")
		}
	}
	u.RawQuery = q.Encode()
	return "quack:" + u.String()
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
