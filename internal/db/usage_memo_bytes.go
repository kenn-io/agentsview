package db

import (
	"encoding/json/v2"
	"unsafe"
)

// Bytes includes the immutable side data omitted from the public response.
func (r DailyUsageResult) Bytes() int64 {
	encoded, err := json.Marshal(r)
	if err != nil {
		return 64<<20 + 1
	}
	n := int64(len(encoded))
	if r.Pairwise != nil {
		n += int64(unsafe.Sizeof(*r.Pairwise))
		for _, entries := range []map[string]UsagePairwiseValue{r.Pairwise.Models, r.Pairwise.Projects} {
			for key, value := range entries {
				n += int64(len(key)) + int64(unsafe.Sizeof(value)) + 32
				n += int64(cap(value.Sessions)) * int64(unsafe.Sizeof(""))
				for _, id := range value.Sessions {
					n += int64(len(id))
				}
			}
		}
	}
	return n
}

// TopSessionBytes estimates an entry's own bytes and all retained strings,
// including metadata omitted from JSON.
func TopSessionBytes(e TopSessionEntry) int64 {
	n := int64(unsafe.Sizeof(e))
	for _, value := range []string{e.SessionID, e.DisplayName, e.Agent, e.Project, e.StartedAt, e.Machine, e.GroupKey, e.GroupLabel, e.SessionName} {
		n += int64(len(value))
	}
	return n
}
