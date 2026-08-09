package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type replicationFingerprintPin struct {
	ID        int64
	SessionID string
	MessageID int64
	Ordinal   int
	Note      *string
	CreatedAt string
}

// SessionReplicationSnapshot is one consistent archive view of a session and
// its replicated dependents. DuckDB mirrors pins from this snapshot;
// PostgreSQL preserves target-owned pins and only reconciles them to messages.
type SessionReplicationSnapshot struct {
	Session        Session
	Messages       []Message
	UsageEvents    []UsageEvent
	SecretFindings []SecretFinding
	PinnedMessages []PinnedMessage
}

// CanonicalSessionReplicationFingerprint hashes exactly the portable rows in
// a replication snapshot. Adapter-owned identity values must be stamped onto
// snapshot.Session before calling it; extraScope covers operational ownership
// markers that are intentionally outside the canonical sessions table.
func CanonicalSessionReplicationFingerprint(
	snapshot SessionReplicationSnapshot, extraScope ...string,
) (string, error) {
	session, err := CanonicalSessionRow(snapshot.Session)
	if err != nil {
		return "", err
	}
	messages, calls, results, err := CanonicalMessageRows(snapshot.Messages)
	if err != nil {
		return "", err
	}
	usage, err := CanonicalUsageEventRows(snapshot.UsageEvents)
	if err != nil {
		return "", err
	}
	for i := range usage {
		usage[i].ID = 0
	}
	findings := CanonicalSecretFindingRows(snapshot.SecretFindings)
	for i := range findings {
		findings[i].ID = nil
		findings[i].CreatedAt = bunmodel.Timestamp{}
	}
	pins := make([]replicationFingerprintPin, len(snapshot.PinnedMessages))
	for i, pin := range snapshot.PinnedMessages {
		pins[i] = replicationFingerprintPin{
			ID: pin.ID, SessionID: pin.SessionID, MessageID: pin.MessageID,
			Ordinal: pin.Ordinal, Note: pin.Note, CreatedAt: pin.CreatedAt,
		}
	}
	payload := struct {
		Session    bunmodel.Session
		Messages   []bunmodel.Message
		Calls      []bunmodel.ToolCall
		Results    []bunmodel.ToolResultEvent
		Usage      []bunmodel.UsageEvent
		Findings   []bunmodel.SecretFinding
		Pins       []replicationFingerprintPin
		ExtraScope []string
	}{session, messages, calls, results, usage, findings, pins, extraScope}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding canonical replication fingerprint: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ReadSessionReplicationSnapshot loads one replication unit inside a single
// SQLite read transaction so a target cannot commit rows from mixed source
// revisions.
func (db *DB) ReadSessionReplicationSnapshot(
	ctx context.Context, sessionID string,
) (SessionReplicationSnapshot, error) {
	var snapshot SessionReplicationSnapshot
	err := db.consistentView(ctx, func(store bun.IDB) error {
		var sessionRow bunmodel.Session
		if err := store.NewSelect().Model(&sessionRow).
			Where("id = ?", sessionID).Scan(ctx); err != nil {
			return fmt.Errorf("reading replication session: %w", err)
		}
		snapshot.Session = sessionFromBunRow(sessionRow)
		if hydrator, ok := db.backend.(bunSessionFullHydrator); ok {
			if err := hydrator.HydrateSessionFull(
				ctx, store, &snapshot.Session,
			); err != nil {
				return fmt.Errorf("hydrating replication session: %w", err)
			}
		}

		messageRows, err := scanBunMessages(ctx, store.NewSelect().
			Model((*bunmodel.Message)(nil)).Where("session_id = ?", sessionID).
			OrderExpr("ordinal ASC"), preserveRawMessageTimestamps(db.backend))
		if err != nil {
			return fmt.Errorf("reading replication messages: %w", err)
		}
		if err := attachBunToolData(ctx, store, messageRows); err != nil {
			return err
		}
		snapshot.Messages = messageRows

		snapshot.UsageEvents, err = usageEventsWithQuerier(ctx, store, sessionID, 0)
		if err != nil {
			return err
		}
		snapshot.SecretFindings, err = sessionSecretFindingsWithQuerier(
			ctx, store, sessionID,
		)
		if err != nil {
			return err
		}
		snapshot.PinnedMessages, err = listPinnedMessagesWithStore(
			ctx, store, sessionID, "",
		)
		return err
	})
	if err != nil {
		return SessionReplicationSnapshot{}, err
	}
	return snapshot, nil
}
