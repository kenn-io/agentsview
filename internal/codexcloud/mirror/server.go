package mirror

import (
	"encoding/json/jsontext"
	"time"
)

// KindThreadResume marks thread metadata taken from a thread/resume reply.
// Such a record sets the resume time as updatedAt and abbreviates the home
// directory, so readers take times and the working directory only from
// KindThread records, which come from listings.
const KindThreadResume Kind = "thread_resume"

// The types below are minimal decode views of the hosted app-server's JSON.
// Records keep the server's full objects; unknown fields are ignored here.

// Thread is the part of a listing or resume thread object readers use.
// Times are Unix seconds.
type Thread struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Preview         string         `json:"preview"`
	CreatedAt       *float64       `json:"createdAt"`
	UpdatedAt       *float64       `json:"updatedAt"`
	Model           string         `json:"model"`
	ReasoningEffort string         `json:"reasoningEffort"`
	Cwd             string         `json:"cwd"`
	Environments    []Environment  `json:"environments"`
	Originator      string         `json:"originator"`
	ThreadSource    string         `json:"threadSource"`
	ParentThreadID  string         `json:"parentThreadId"`
	ForkedFromID    string         `json:"forkedFromId"`
	Source          jsontext.Value `json:"source"`
	Status          jsontext.Value `json:"status"`
}

// Environment is one cloud environment a thread ran in.
type Environment struct {
	EnvironmentID string `json:"environmentId"`
	Cwd           string `json:"cwd"`
}

// Turn is a turn object from thread/turns/list. Times are Unix seconds.
type Turn struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	StartedAt   *float64 `json:"startedAt"`
	CompletedAt *float64 `json:"completedAt"`
}

// ItemRecord is one row of thread/items/list, stored as an item record's
// data. Times are Unix milliseconds.
type ItemRecord struct {
	TurnID        string         `json:"turnId"`
	Item          jsontext.Value `json:"item"`
	StartedAtMs   *float64       `json:"startedAtMs"`
	CompletedAtMs *float64       `json:"completedAtMs"`
}

// ItemHeader is the identity and status every item object carries.
// Status is a string on items that have one; other items omit it.
type ItemHeader struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Status jsontext.Value `json:"status"`
}

// Seconds converts Unix seconds to a time, or zero when absent.
func Seconds(v *float64) time.Time {
	if v == nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(*v * 1000))
}

// Millis converts Unix milliseconds to a time, or zero when absent.
func Millis(v *float64) time.Time {
	if v == nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(*v))
}
