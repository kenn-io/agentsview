// Package review builds dated Friction Log digests from stored findings.
// It ports jilog's run_review (crates/jilog-review/src/digest.rs:208-706)
// onto database state: subjects come from the archive, the processed set is
// friction_digest_sessions, and "nightly" becomes an hourly catch-up.
package review

import "time"

const dateLayout = "2006-01-02"

func localDate(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(dateLayout)
}

// addDays shifts a YYYY-MM-DD date. Inputs are validated at the API edge
// (BuildDate) or produced by localDate, so a parse failure is a bug.
func addDays(date string, n int) string {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		panic("review: malformed internal date " + date)
	}
	return d.AddDate(0, 0, n).Format(dateLayout)
}
