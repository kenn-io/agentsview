package filing

import "fmt"

// ReopenAllowed ports jilog's done-only reason gate. A missing reason or any
// other closure reason does not claim that the underlying problem went away.
func ReopenAllowed(closedReason string) bool { return closedReason == "done" }

// RecurrenceCommentKey identifies one recurrence note per issue and digest day.
func RecurrenceCommentKey(issueUID, date string) string {
	return fmt.Sprintf("friction-recur-%s-%s", issueUID, date)
}

// RecurrenceComment gives the digest day and an optional public session link.
func RecurrenceComment(date, sessionURL string) string {
	comment := fmt.Sprintf("Recurred on %s — closure may have been premature.", date)
	if sessionURL != "" {
		comment += "\nSession: " + sessionURL
	}
	return comment
}
