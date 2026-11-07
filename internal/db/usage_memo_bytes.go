package db

import (
	"unsafe"
)

// TopSessionBytes estimates an entry's own bytes and all retained strings,
// including metadata omitted from JSON.
func TopSessionBytes(e TopSessionEntry) int64 {
	n := int64(unsafe.Sizeof(e))
	for _, value := range []string{e.SessionID, e.DisplayName, e.Agent, e.Project, e.StartedAt, e.Machine, e.GroupKey, e.GroupLabel, e.SessionName} {
		n += int64(len(value))
	}
	return n
}
