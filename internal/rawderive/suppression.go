package rawderive

import (
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// SuppressionFor finds the suppression recorded for a parser identity. Captures
// made before data version 134 key a Codex revert page by its bare rollout id,
// while the parser now emits "<thread>_<rollout>", so that spelling also
// matches the page's rollout.
func SuppressionFor(policy map[string]db.RawArchiveSuppression, native string) (db.RawArchiveSuppression, bool) {
	if d, ok := policy[native]; ok {
		return d, true
	}
	prefixEnd := strings.LastIndex(native, ":") + 1
	raw := native[prefixEnd:]
	// "<thread>_<rollout>" with two UUIDs.
	if len(raw) != 73 || raw[36] != '_' {
		return db.RawArchiveSuppression{}, false
	}
	d, ok := policy[native[:prefixEnd]+raw[37:]]
	return d, ok
}
