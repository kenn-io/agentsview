// Package rawderive turns accepted raw custody manifests into derived sessions.
package rawderive

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/rawsync"
)

// ErrLeaseLost means a worker no longer owns the fenced job attempt.
var ErrLeaseLost = errors.New("raw derivation lease lost")

// MaxClaimBatchSize is the shared upper bound on how many raw parse jobs one
// worker may lease per claim. Queues and workers validate against this single
// contract so a configured worker can never request a claim the queue rejects.
const MaxClaimBatchSize = 256

// MaxLeaseOwnerBytes is the shared upper bound for a durable worker identity.
const MaxLeaseOwnerBytes = 128

// ValidLeaseOwner reports whether owner is a nonblank UTF-8 worker identity
// that every queue implementation can persist.
func ValidLeaseOwner(owner string) bool {
	return strings.TrimSpace(owner) != "" && utf8.ValidString(owner) &&
		len(owner) <= MaxLeaseOwnerBytes
}

// JobLease is the fenced ownership token for one parse attempt.
type JobLease struct {
	ID                int64
	Identity          rawsync.AuthIdentity
	ManifestID        string
	ProcessingVersion string
	Attempt           int
	Owner             string
	ExpiresAt         time.Time
}
