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

// bootstrapExportQueue is a test seam for injecting bootstrap failures.
var bootstrapExportQueue = (*db.DB).BootstrapArtifactExportQueue

// bootstrapQueueForOrigin populates the export queue after origin persists.
// On failure it rolls the stored origin back to previous so a retry re-runs
// the bootstrap instead of fast-pathing to success with an unpopulated queue.
func bootstrapQueueForOrigin(database *db.DB, origin, previous string) error {
	err := bootstrapExportQueue(database)
	if err == nil {
		return nil
	}
	err = fmt.Errorf("bootstrapping export queue for origin %s: %w", origin, err)
	if rollbackErr := database.SetSyncState(originStateKey, previous); rollbackErr != nil {
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
	if err := bootstrapQueueForOrigin(database, origin, ""); err != nil {
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
	return bootstrapQueueForOrigin(database, origin, existing)
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
