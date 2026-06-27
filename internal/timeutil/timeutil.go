package timeutil

import (
	"os"
	"time"
)

// Ptr formats a time.Time to an RFC3339Nano string pointer
// for DB storage. Returns nil for zero time.
func Ptr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// Format formats a time.Time to an RFC3339Nano string for DB
// storage. Returns empty string for zero time.
func Format(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// IsValidDate reports whether s is a well-formed YYYY-MM-DD string.
func IsValidDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// IsValidTimestamp reports whether s is a well-formed RFC3339 or
// RFC3339Nano timestamp.
func IsValidTimestamp(s string) bool {
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

func BestEffortLocalTimezone() string {
	return bestEffortLocalTimezone(
		os.Getenv("TZ"),
		time.Now().Location(),
	)
}

func bestEffortLocalTimezone(envTZ string, loc *time.Location) string {
	if name := validatedTimezoneName(envTZ); name != "" {
		return name
	}
	if loc == nil {
		return ""
	}
	return validatedTimezoneName(loc.String())
}

func validatedTimezoneName(name string) string {
	if name == "" || name == "Local" {
		return ""
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}
