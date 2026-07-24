package artifact

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// bootstrapExportQueue and requeueExportQueue are test seams for injecting
// queue-population failures. Bootstrap enqueues each session once (INSERT OR
// IGNORE); requeue force-dirties every owned session for a divergent origin.
var (
	bootstrapExportQueue = (*db.DB).BootstrapArtifactExportQueue
	requeueExportQueue   = (*db.DB).RequeueAllArtifactExports
)

// populateQueueForOrigin runs populate after the origin persists. On failure it
// rolls the stored origin back to previous so a retry re-runs population instead
// of fast-pathing to success with an unpopulated queue. When previous is empty
// the origin key is deleted rather than set to an empty value, because the
// export gates test key existence, not the stored value.
func populateQueueForOrigin(
	database *db.DB, populate func(*db.DB) error, origin, previous string,
) error {
	err := populate(database)
	if err == nil {
		return nil
	}
	err = fmt.Errorf("populating export queue for origin %s: %w", origin, err)
	var rollbackErr error
	if previous == "" {
		rollbackErr = database.DeleteSyncState(originStateKey)
	} else {
		rollbackErr = database.SetSyncState(originStateKey, previous)
	}
	if rollbackErr != nil {
		return errors.Join(err,
			fmt.Errorf("rolling back artifact origin: %w", rollbackErr))
	}
	return err
}

// EnsureOrigin returns the persisted origin ID, creating one when absent.
func EnsureOrigin(database *db.DB) (string, error) {
	origin, err := StoredOrigin(database)
	if err != nil {
		return "", err
	}
	if origin != "" {
		return origin, nil
	}
	origin, err = newOriginID()
	if err != nil {
		return "", err
	}
	if err := validateOriginID(origin); err != nil {
		return "", fmt.Errorf("generated artifact origin: %w", err)
	}
	if err := database.SetSyncState(originStateKey, origin); err != nil {
		return "", fmt.Errorf("persisting artifact origin: %w", err)
	}
	if err := populateQueueForOrigin(database, bootstrapExportQueue, origin, ""); err != nil {
		return "", err
	}
	return origin, nil
}

// AdoptOrigin persists origin as this machine's artifact origin in the database
// sync state so DB-derived lookups (EnsureOrigin and its callers) agree with the
// authoritative config origin. It validates the input and is idempotent: it only
// writes when the stored value differs. The config origin always wins, so a
// previously stored value is overwritten to converge on a single origin.
func AdoptOrigin(database *db.DB, origin string) error {
	if err := validateOriginID(origin); err != nil {
		return fmt.Errorf("adopting artifact origin: %w", err)
	}
	existing, err := StoredOrigin(database)
	if err != nil {
		return err
	}
	if existing == origin {
		return nil
	}
	if err := database.SetSyncState(originStateKey, origin); err != nil {
		return fmt.Errorf("persisting artifact origin: %w", err)
	}
	// A divergent adoption replaces an established origin whose sessions may
	// already be acknowledged, so bootstrap's INSERT OR IGNORE would leave the
	// new origin's ledger empty. Force-requeue every owned session instead.
	// First-time adoption keeps the cheaper bootstrap.
	if existing != "" {
		return populateQueueForOrigin(database, requeueExportQueue, origin, existing)
	}
	return populateQueueForOrigin(database, bootstrapExportQueue, origin, existing)
}

// StoredOrigin returns the persisted origin ID without creating one.
func StoredOrigin(database *db.DB) (string, error) {
	origin, err := database.GetSyncState(originStateKey)
	if err != nil {
		return "", fmt.Errorf("reading artifact origin: %w", err)
	}
	if origin != "" {
		if err := validateOriginID(origin); err != nil {
			return "", fmt.Errorf("stored artifact origin: %w", err)
		}
		return origin, nil
	}
	return "", nil
}

func newOriginID() (string, error) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "machine"
	}
	host = sanitizeOriginPart(host)
	if host == "" || host == "local" {
		host = "machine"
	}
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating artifact origin suffix: %w", err)
	}
	return fmt.Sprintf("%s-%s", host, hex.EncodeToString(suffix[:])), nil
}

func sanitizeOriginPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
