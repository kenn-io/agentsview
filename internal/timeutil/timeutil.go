package timeutil

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/thlib/go-timezone-local/tzlocal"
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

// ParseSince resolves a --since value against now: relative forms
// "Nh", "Nd", "Nw", "Nm" (months), "Ny", or an absolute YYYY-MM-DD
// (that date's midnight in now's location). N is a positive integer.
// Months/years use now.AddDate(-y, -m, 0) for calendar-aware arithmetic;
// hours/days/weeks use now.Add(-d).
func ParseSince(now time.Time, s string) (time.Time, error) {
	if IsValidDate(s) {
		return time.ParseInLocation("2006-01-02", s, now.Location())
	}
	if len(s) < 2 {
		return time.Time{}, sinceFormatError(s)
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return time.Time{}, sinceFormatError(s)
	}
	switch unit {
	case 'h':
		return now.Add(-time.Duration(n) * time.Hour), nil
	case 'd':
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	case 'w':
		return now.Add(-time.Duration(n) * 7 * 24 * time.Hour), nil
	case 'm':
		return now.AddDate(0, -n, 0), nil
	case 'y':
		return now.AddDate(-n, 0, 0), nil
	default:
		return time.Time{}, sinceFormatError(s)
	}
}

// sinceFormatError names the accepted --since forms in the error message so
// callers can react without consulting docs.
func sinceFormatError(s string) error {
	return fmt.Errorf(
		"invalid --since %q: use Nh, Nd, Nw, Nm, Ny, or YYYY-MM-DD", s)
}

func BestEffortLocalTimezone() string {
	return resolveLocalTimezone(
		os.Getenv("TZ"),
		time.Now().Location(),
		tzlocal.LocalTZ,
	)
}

func bestEffortLocalTimezone(envTZ string, loc *time.Location) string {
	return resolveLocalTimezone(envTZ, loc, nil)
}

func resolveLocalTimezone(
	envTZ string,
	loc *time.Location,
	systemLocal func() (string, error),
) string {
	if name := validatedTimezoneName(envTZ); name != "" {
		return name
	}
	if loc != nil {
		if name := validatedTimezoneName(loc.String()); name != "" {
			return name
		}
	}
	if systemLocal == nil {
		return ""
	}
	name, err := systemLocal()
	if err != nil {
		return ""
	}
	return validatedTimezoneName(name)
}

// LocalTimezoneOrUTC returns the best-effort local IANA timezone name, or UTC
// when no loadable local timezone can be resolved.
func LocalTimezoneOrUTC() string {
	return localTimezoneOrUTC(BestEffortLocalTimezone)
}

func localTimezoneOrUTC(resolve func() string) string {
	if name := resolve(); name != "" {
		return name
	}
	return "UTC"
}

// LocalLocation returns the resolved local timezone location, or time.Local
// when no loadable local timezone can be resolved.
func LocalLocation() *time.Location {
	return localLocation(BestEffortLocalTimezone, time.Local)
}

func localLocation(resolve func() string, fallback *time.Location) *time.Location {
	name := resolve()
	if name == "" {
		return fallback
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fallback
	}
	return loc
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
