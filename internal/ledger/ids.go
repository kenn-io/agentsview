package ledger

import (
	"github.com/google/uuid"
)

// EventIDNamespace is the UUIDv5 namespace for deterministic event IDs:
// v5(NAMESPACE_URL, "agentsview:ledger:event-id").
var EventIDNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("agentsview:ledger:event-id"))

// NewEventID returns a time-ordered UUIDv7, the form jilog's tests and
// opsctl use.
func NewEventID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

// DeterministicEventID returns v5(EventIDNamespace, source + "\x00" + key)
// so a producer that re-emits the same fact (for example a `diagnostic`
// event, spec §12.5) gets the same event_id and the event_id dedupe drops
// the repeat.
func DeterministicEventID(source, key string) uuid.UUID {
	name := make([]byte, 0, len(source)+1+len(key))
	name = append(name, source...)
	name = append(name, 0)
	name = append(name, key...)
	return uuid.NewSHA1(EventIDNamespace, name)
}

// ValidSourceName ports ledger-spool lib.rs:52-63:
// ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ over ASCII bytes. It is the guard
// against path traversal wherever a source or zone becomes a path.
func ValidSourceName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	if !isASCIIAlnum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !isASCIIAlnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
