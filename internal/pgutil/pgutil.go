package pgutil

import (
	"fmt"
	"log"
	"net"
	"net/url"

	"github.com/jackc/pgx/v5/pgconn"
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
				"set sslmode=require (or verify-full) for non-local hosts, "+
				"or set allow_insecure_pg: true in config to override",
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
		log.Printf("warning: pg connection to %s permits plaintext; "+
			"consider sslmode=require or verify-full for non-local hosts",
			cfg.Host)
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

// isLoopback returns true if host is a loopback address, localhost,
// a unix socket path, or empty (defaults to local connection).
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
