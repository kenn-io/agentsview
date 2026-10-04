package filing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/stringutil"
)

// ArchiveStore reads existing findings and transcript evidence from either
// the local archive or the PostgreSQL hub. It never reparses source files.
type ArchiveStore interface {
	ListFrictionFindings(context.Context, db.FrictionFindingFilter) ([]db.FrictionFinding, string, error)
	GetSession(context.Context, string) (*db.Session, error)
	GetMessagesWindow(context.Context, string, db.MessageWindow) ([]db.Message, error)
}

const evidenceMessageBytes = 2000

// PlanWithContext is shared by previews and creates so both show the same
// bounded, redacted evidence. An unavailable archive is an error.
func (f *Filer) PlanWithContext(ctx context.Context, sig friction.Signal, run RunContext) (kata.CreateIssue, error) {
	if f.Archive != nil && sig.SubjectKind == friction.SubjectSession {
		session, err := f.Archive.GetSession(ctx, sig.SubjectID)
		if err != nil {
			return kata.CreateIssue{}, fmt.Errorf("reading friction session: %w", err)
		}
		if session == nil || session.DeletedAt != nil {
			return kata.CreateIssue{}, ErrSignalNotFound
		}
	}
	plan := f.Plan(sig, run)
	if sig.Evidence != "" {
		plan.Body += "\n\n## Detector evidence\n" + f.boundedEvidence(sig.Evidence)
	}
	if f.Archive == nil || sig.SubjectKind == friction.SubjectDiagnostic || sig.Ordinal == nil {
		return plan, nil
	}
	messages, err := f.Archive.GetMessagesWindow(ctx, sig.SubjectID, db.MessageWindow{
		Around: sig.Ordinal, Before: 2, After: 2, Roles: []string{"user", "assistant"},
	})
	if err != nil {
		return kata.CreateIssue{}, fmt.Errorf("reading friction transcript context: %w", err)
	}
	var b strings.Builder
	for _, msg := range messages {
		if msg.SessionID != sig.SubjectID || msg.IsSystem || (msg.Role != "user" && msg.Role != "assistant") {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\n\n## Transcript context\n")
		}
		fmt.Fprintf(&b, "\nMessage %d (%s):\n", msg.Ordinal, msg.Role)
		for line := range strings.SplitSeq(f.boundedEvidence(msg.Content), "\n") {
			b.WriteString("> " + line + "\n")
		}
	}
	plan.Body += b.String()
	return plan, nil
}

func (f *Filer) boundedEvidence(text string) string {
	text = f.redact(text)
	const suffix = " [truncated]"
	if len(text) > evidenceMessageBytes {
		return stringutil.SafeTruncate(text, evidenceMessageBytes-len(suffix)) + suffix
	}
	return text
}

func (f *Filer) storedSignal(ctx context.Context, fingerprint string, latest bool) (friction.Signal, RunContext, error) {
	if f.Archive == nil {
		return friction.Signal{}, RunContext{}, ErrSignalNotFound
	}
	var selected friction.Signal
	var selectedRun RunContext
	var cursor string
	for {
		findings, next, err := f.Archive.ListFrictionFindings(ctx, db.FrictionFindingFilter{Fingerprint: fingerprint, Limit: db.MaxFrictionListLimit, Cursor: cursor})
		if err != nil {
			return friction.Signal{}, RunContext{}, err
		}
		for _, finding := range findings {
			sig, run, err := f.findingSignal(ctx, fingerprint, finding)
			if err == ErrSignalNotFound {
				continue
			}
			if err != nil {
				return friction.Signal{}, RunContext{}, err
			}
			if !latest {
				return sig, run, nil
			}
			if selectedRun.Date == "" || run.Date > selectedRun.Date ||
				(run.Date == selectedRun.Date && sig.OccurredAt.After(selected.OccurredAt)) {
				selected, selectedRun = sig, run
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if selectedRun.Date == "" {
		return friction.Signal{}, RunContext{}, ErrSignalNotFound
	}
	return selected, selectedRun, nil
}

func (f *Filer) findingSignal(ctx context.Context, fingerprint string, finding db.FrictionFinding) (friction.Signal, RunContext, error) {
	session, err := f.Archive.GetSession(ctx, finding.SessionID)
	if err != nil {
		return friction.Signal{}, RunContext{}, err
	}
	if session == nil || session.DeletedAt != nil {
		return friction.Signal{}, RunContext{}, ErrSignalNotFound
	}
	sig := friction.Signal{
		Kind: friction.Kind(finding.Kind), SubjectKind: friction.SubjectSession, SubjectID: finding.SessionID,
		Dims:     friction.Dims{Agent: session.Agent, Machine: session.Machine},
		Detector: finding.Detector, Text: finding.Text, ToolName: finding.ToolName, Label: finding.Label,
		Evidence: finding.Evidence, Ordinal: finding.MessageOrdinal, CallIndex: finding.CallIndex, Seq: finding.Seq,
	}
	if sig.Fingerprint() != fingerprint {
		return friction.Signal{}, RunContext{}, fmt.Errorf("stored friction fingerprint does not match its finding")
	}
	at := f.now()
	if finding.OccurredAt != nil {
		at, sig.OccurredAt = *finding.OccurredAt, *finding.OccurredAt
	} else {
		for _, stamp := range []*string{session.EndedAt, session.StartedAt} {
			if stamp != nil {
				parsed, err := time.Parse(time.RFC3339Nano, *stamp)
				if err != nil {
					return friction.Signal{}, RunContext{}, fmt.Errorf("reading friction session date: %w", err)
				}
				at = parsed
				break
			}
		}
	}
	loc := f.Location
	if loc == nil {
		loc = time.UTC
	}
	sig.OccurredAt = at
	// No digest exists yet, so include the session link without a digest URL.
	return sig, RunContext{Date: at.In(loc).Format(time.DateOnly), PublicURL: f.PublicURL}, nil
}
