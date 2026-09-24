// Package frictionevents builds the Friction Log's ledger events (spec
// §12.5). It is pure: callers append the events through a ledger.Sink.
// jilog's architecture doc states that review writes to the ledger
// (docs/architecture.html:290,366); its code never did, so these event
// types are agentsview's (D25).
package frictionevents

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/serdejson"
)

const Subsystem = "friction"

const (
	KindDigestBuilt      = "friction.digest.built"
	KindPatternFirstSeen = "friction.pattern.first_seen"
	KindPatternRecurred  = "friction.pattern.recurred"
	KindIssueFiled       = "friction.issue.filed"
	KindIssueReopened    = "friction.issue.reopened"
	KindIssueLinked      = "friction.issue.linked"
)

var countedKinds = []friction.Kind{
	friction.KindCorrection, friction.KindError, friction.KindWorkaround, friction.KindDeferral,
	friction.KindPattern, friction.KindFrustration, friction.KindInterruption,
}

// DigestBuilt is the payload of one friction.digest.built event.
type DigestBuilt struct {
	Date, Timezone, RulesVersion, MarkdownSHA256, CountsLine string
	Revision                                                 int
	Counts                                                   map[string]int
	P0Tools                                                  []string
	RunID                                                    *uuid.UUID
}

// DigestBuiltFromSnapshot counts signals per kind (every kind present, zero
// included), P0 tools (sorted) and sessions scanned. CountsLine is the first
// line of the CLI's human summary.
func DigestBuiltFromSnapshot(
	s friction.DigestSnapshot, m friction.SummaryMeta, revision int, markdownSHA256 string, runID *uuid.UUID,
) DigestBuilt {
	counts := make(map[string]int, len(countedKinds)+2)
	for _, k := range countedKinds {
		counts[string(k)] = 0
	}
	for _, sig := range s.Signals {
		counts[string(sig.Kind)]++
	}
	counts["p0_alerts"] = len(s.P0Alerts)
	counts["sessions_scanned"] = s.SessionsScanned
	tools := make([]string, 0, len(s.P0Alerts))
	for tool := range s.P0Alerts {
		tools = append(tools, tool)
	}
	slices.Sort(tools)
	return DigestBuilt{
		Date: s.Date, Timezone: s.Timezone, RulesVersion: s.RulesVersion, MarkdownSHA256: markdownSHA256,
		CountsLine: strings.SplitN(friction.HumanSummary(s, m), "\n", 2)[0],
		Revision:   revision, Counts: counts, P0Tools: tools, RunID: runID,
	}
}

func num(n int) serdejson.Number { return serdejson.Number(strconv.Itoa(n)) }

func base(kind, summary string) map[string]any {
	return map[string]any{"kind": kind, "subsystem": Subsystem, "summary": summary}
}

func event(
	id uuid.UUID, source string, class ledger.EventClass, tier ledger.PayloadTier,
	object string, correlation, causation *uuid.UUID, payload map[string]any, at time.Time,
) ledger.Event {
	actor := "machine:" + source
	return ledger.Event{
		EventID: id, Timestamp: at.UTC(), CorrelationID: correlation, CausationID: causation,
		ActorRef: &actor, ObjectRef: &object, EventClass: class, PayloadTier: tier, Payload: payload,
	}
}

// DigestBuiltEvent is friction.digest.built (projection, structured).
func DigestBuiltEvent(source string, d DigestBuilt, at time.Time) ledger.Event {
	counts := make(map[string]any, len(d.Counts))
	for k, v := range d.Counts {
		counts[k] = num(v)
	}
	tools := make([]any, len(d.P0Tools))
	for i, t := range d.P0Tools {
		tools[i] = t
	}
	p := base(KindDigestBuilt, d.CountsLine)
	p["date"], p["timezone"], p["rules_version"] = d.Date, d.Timezone, d.RulesVersion
	p["counts"], p["p0_tools"], p["markdown_sha256"], p["revision"] = counts, tools, d.MarkdownSHA256, num(d.Revision)
	id := ledger.DeterministicEventID(source, KindDigestBuilt+":"+d.Date+":"+strconv.Itoa(d.Revision))
	return event(id, source, ledger.ClassProjection, ledger.TierStructured, "friction-digest:"+d.Date, d.RunID, nil, p, at)
}

// PatternTransition describes a fingerprint's first or repeated appearance.
type PatternTransition struct {
	Fingerprint, Title, SubjectID, Date string
	SignalKind                          friction.Kind
	OccurrenceCount                     int
	RunID                               *uuid.UUID
}

func patternPayload(kind string, p PatternTransition) map[string]any {
	out := base(kind, p.Title)
	out["fingerprint"], out["signal_kind"], out["title"] = p.Fingerprint, string(p.SignalKind), p.Title
	out["subject_id"], out["date"] = p.SubjectID, p.Date
	return out
}

// PatternFirstSeenEvent is friction.pattern.first_seen (health, structured).
func PatternFirstSeenEvent(source string, p PatternTransition, at time.Time) ledger.Event {
	id := ledger.DeterministicEventID(source, KindPatternFirstSeen+":"+p.Fingerprint)
	return event(id, source, ledger.ClassHealth, ledger.TierStructured, "friction:"+p.Fingerprint, p.RunID, nil,
		patternPayload(KindPatternFirstSeen, p), at)
}

// RecurredEventID is the ID of the recurred event for (fingerprint, date);
// a reopen names it as its cause.
func RecurredEventID(source, fingerprint, date string) uuid.UUID {
	return ledger.DeterministicEventID(source, KindPatternRecurred+":"+fingerprint+":"+date)
}

// PatternRecurredEvent is friction.pattern.recurred (health, structured).
func PatternRecurredEvent(source string, p PatternTransition, at time.Time) ledger.Event {
	payload := patternPayload(KindPatternRecurred, p)
	payload["occurrence_count"] = num(p.OccurrenceCount)
	return event(RecurredEventID(source, p.Fingerprint, p.Date), source, ledger.ClassHealth, ledger.TierStructured,
		"friction:"+p.Fingerprint, p.RunID, nil, payload, at)
}

// IssueFiled describes a link that reached state linked by filing.
type IssueFiled struct {
	Fingerprint, Title, IssueUID, QualifiedID, KataInstanceUID, LinkSource string
	RunID                                                                  *uuid.UUID
}

// IssueFiledEvent is friction.issue.filed (delivery, metadata_only).
func IssueFiledEvent(source string, f IssueFiled, at time.Time) ledger.Event {
	p := base(KindIssueFiled, f.Title)
	p["issue_uid"], p["qualified_id"], p["kata_instance_uid"], p["link_source"] = f.IssueUID, f.QualifiedID, f.KataInstanceUID, f.LinkSource
	id := ledger.DeterministicEventID(source, KindIssueFiled+":"+f.Fingerprint+":"+f.IssueUID)
	return event(id, source, ledger.ClassDelivery, ledger.TierMetadataOnly, "friction:"+f.Fingerprint, f.RunID, nil, p, at)
}

// IssueReopened describes a recurrence reopen (§10).
type IssueReopened struct {
	Fingerprint, Title, IssueUID, QualifiedID, Date string
	RunID                                           *uuid.UUID
}

// IssueReopenedEvent is friction.issue.reopened (state_change,
// metadata_only), caused by that date's recurred event.
func IssueReopenedEvent(source string, r IssueReopened, at time.Time) ledger.Event {
	p := base(KindIssueReopened, r.Title)
	p["issue_uid"], p["qualified_id"], p["date"] = r.IssueUID, r.QualifiedID, r.Date
	cause := RecurredEventID(source, r.Fingerprint, r.Date)
	id := ledger.DeterministicEventID(source, KindIssueReopened+":"+r.IssueUID+":"+r.Date)
	return event(id, source, ledger.ClassStateChange, ledger.TierMetadataOnly, "friction:"+r.Fingerprint, r.RunID, &cause, p, at)
}

// IssueLinked describes a manual `friction link`.
type IssueLinked struct{ Fingerprint, Title, IssueUID string }

// IssueLinkedEvent is friction.issue.linked (decision, metadata_only).
func IssueLinkedEvent(source string, l IssueLinked, at time.Time) ledger.Event {
	p := base(KindIssueLinked, l.Title)
	p["issue_uid"], p["link_source"] = l.IssueUID, "manual"
	return event(ledger.NewEventID(), source, ledger.ClassDecision, ledger.TierMetadataOnly, "friction:"+l.Fingerprint, nil, nil, p, at)
}

// ParseRunID parses a friction_digests.run_id; nil when empty or invalid.
func ParseRunID(s string) *uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}
