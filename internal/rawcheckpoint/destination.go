package rawcheckpoint

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
)

var (
	ErrDestinationMismatch = errors.New("rawcheckpoint: server does not match the checkpoint; use a separate AGENTSVIEW_DATA_DIR for another server")
	ErrDestinationUnknown  = errors.New("rawcheckpoint: existing uploads have no recorded server; use a separate AGENTSVIEW_DATA_DIR and a newly enrolled device")
)

// EnsureDestination binds both watch and backfill uploads to one server. A
// checkpoint with earlier uploads but no server URL cannot prove their destination.
func (s *Store) EnsureDestination(ctx context.Context, destination string) error {
	destination, err := normalizeDestination(destination)
	if err != nil {
		return err
	}
	return s.withImmediateWrite(ctx, "ensure destination", func(conn *sql.Conn) error {
		return ensureDestinationConn(ctx, conn, destination)
	})
}

func normalizeDestination(destination string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(destination), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("rawcheckpoint: server URL is invalid")
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

func ensureDestinationConn(ctx context.Context, conn *sql.Conn, destination string) error {
	var stored string
	if err := conn.QueryRowContext(ctx, `SELECT destination FROM outbox_config WHERE id=1`).Scan(&stored); err != nil {
		return err
	}
	if stored == destination {
		return nil
	}
	if stored != "" {
		return ErrDestinationMismatch
	}
	var uploaded bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM raw_sources WHERE head_receipt != '')
		OR EXISTS (SELECT 1 FROM outbox_generations WHERE state = 'finalized')`).Scan(&uploaded); err != nil {
		return err
	}
	if uploaded {
		return ErrDestinationUnknown
	}
	_, err := conn.ExecContext(ctx, `UPDATE outbox_config SET destination=? WHERE id=1`, destination)
	return err
}
