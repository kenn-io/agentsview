package filing

import (
	"context"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/frictionevents"
	"go.kenn.io/agentsview/internal/kata"
)

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

// recur reopens a done-closed issue, then adds an idempotent comment and
// label. The reopen must succeed; annotation failures only warn.
func (f *Filer) recur(ctx context.Context, sig friction.Signal, run RunContext, row db.FrictionIssueLink, is kata.Issue) (db.FrictionIssueLink, outcome, error) {
	row.IssueUID, row.QualifiedID = is.UID, is.QualifiedID
	if is.WebURL != "" {
		row.WebURL = is.WebURL
	}
	changed, err := f.Kata.Reopen(ctx, is.UID)
	if err != nil {
		failed, ferr := f.fail(ctx, row, err)
		return failed, outcomeNone, ferr
	}
	if changed {
		f.appendLedger(ctx, frictionevents.IssueReopenedEvent(f.LedgerSource, frictionevents.IssueReopened{
			Fingerprint: row.Fingerprint, Title: f.redact(sig.Title()), IssueUID: is.UID, QualifiedID: is.QualifiedID,
			Date: run.Date, RunID: frictionevents.ParseRunID(run.RunID),
		}, f.now()), run.InlineLedger)
	}
	url := ""
	if sig.SubjectKind != friction.SubjectDiagnostic {
		url = SessionURL(run.PublicURL, sig.SubjectID, sig.Ordinal)
	}
	if err := f.Kata.Comment(ctx, is.UID, RecurrenceCommentKey(is.UID, run.Date), f.redact(RecurrenceComment(run.Date, url))); err != nil {
		log.Printf("friction: Kata recurrence comment on %s failed (issue reopened anyway): %s", is.QualifiedID, f.redact(err.Error()))
	}
	if err := f.Kata.AddLabel(ctx, is.UID, LabelRecurred); err != nil {
		log.Printf("friction: Kata %s label on %s failed (issue reopened anyway): %s", LabelRecurred, is.QualifiedID, f.redact(err.Error()))
	}
	source := row.LinkSource
	if source == "" {
		source = db.FrictionLinkSourceFound
	}
	linked, err := f.linked(ctx, row, is, source, run.Date)
	if err != nil {
		return linked, outcomeNone, err
	}
	return linked, outcomeReopened, nil
}
