package frictionevents

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/serdejson"
)

var at = time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)

func payload(t *testing.T, e ledger.Event) map[string]any {
	t.Helper()
	p, ok := e.Payload.(map[string]any)
	require.True(t, ok, "payload must be an object")
	return p
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEventShapes(t *testing.T) {
	run := uuid.MustParse("01920000-0000-7000-8000-000000000001")
	pt := PatternTransition{
		Fingerprint: "fl1:ab", Title: "[friction/error] bash: exit 1", SubjectID: "s1", Date: "2026-09-13",
		SignalKind: friction.KindError, OccurrenceCount: 4, RunID: &run,
	}
	for _, tc := range []struct {
		name     string
		ev       ledger.Event
		class    ledger.EventClass
		tier     ledger.PayloadTier
		object   string
		kind     string
		summary  string
		payloadK []string
	}{
		{
			"first_seen", PatternFirstSeenEvent("host-a", pt, at), ledger.ClassHealth, ledger.TierStructured,
			"friction:fl1:ab", KindPatternFirstSeen, pt.Title,
			[]string{"kind", "subsystem", "summary", "fingerprint", "signal_kind", "title", "subject_id", "date"},
		},
		{
			"recurred", PatternRecurredEvent("host-a", pt, at), ledger.ClassHealth, ledger.TierStructured,
			"friction:fl1:ab", KindPatternRecurred, pt.Title,
			[]string{"kind", "subsystem", "summary", "fingerprint", "signal_kind", "title", "subject_id", "date", "occurrence_count"},
		},
		{
			"filed", IssueFiledEvent("host-a", IssueFiled{
				Fingerprint: "fl1:ab", Title: pt.Title, IssueUID: "01J0", QualifiedID: "agentsview#7",
				KataInstanceUID: "inst", LinkSource: "created", RunID: &run,
			}, at), ledger.ClassDelivery, ledger.TierMetadataOnly,
			"friction:fl1:ab", KindIssueFiled, pt.Title,
			[]string{"kind", "subsystem", "summary", "issue_uid", "qualified_id", "kata_instance_uid", "link_source"},
		},
		{
			"reopened", IssueReopenedEvent("host-a", IssueReopened{
				Fingerprint: "fl1:ab", Title: pt.Title, IssueUID: "01J0",
				QualifiedID: "agentsview#7", Date: "2026-09-13", RunID: &run,
			}, at), ledger.ClassStateChange, ledger.TierMetadataOnly,
			"friction:fl1:ab", KindIssueReopened, pt.Title,
			[]string{"kind", "subsystem", "summary", "issue_uid", "qualified_id", "date"},
		},
		{
			"linked", IssueLinkedEvent("host-a", IssueLinked{Fingerprint: "fl1:ab", Title: pt.Title, IssueUID: "01J0"}, at),
			ledger.ClassDecision, ledger.TierMetadataOnly, "friction:fl1:ab", KindIssueLinked, pt.Title,
			[]string{"kind", "subsystem", "summary", "issue_uid", "link_source"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.class, tc.ev.EventClass)
			assert.Equal(t, tc.tier, tc.ev.PayloadTier)
			require.NotNil(t, tc.ev.ActorRef)
			assert.Equal(t, "machine:host-a", *tc.ev.ActorRef)
			require.NotNil(t, tc.ev.ObjectRef)
			assert.Equal(t, tc.object, *tc.ev.ObjectRef)
			assert.Equal(t, at, tc.ev.Timestamp)
			p := payload(t, tc.ev)
			assert.ElementsMatch(t, tc.payloadK, keys(p))
			assert.Equal(t, tc.kind, p["kind"])
			assert.Equal(t, Subsystem, p["subsystem"])
			assert.Equal(t, tc.summary, p["summary"])
			assert.Equal(t, "friction", ledger.Subsystem(tc.ev))
			assert.Equal(t, tc.summary, ledger.Summary(tc.ev))
		})
	}
}

func TestDeterministicIDs(t *testing.T) {
	pt := PatternTransition{Fingerprint: "fl1:ab", Date: "2026-09-13"}
	assert.Equal(t, PatternFirstSeenEvent("host-a", pt, at).EventID, PatternFirstSeenEvent("host-a", pt, at.Add(time.Hour)).EventID)
	assert.NotEqual(t, PatternFirstSeenEvent("host-a", pt, at).EventID, PatternFirstSeenEvent("host-b", pt, at).EventID)
	assert.Equal(t, RecurredEventID("host-a", "fl1:ab", "2026-09-13"), PatternRecurredEvent("host-a", pt, at).EventID)
	d1 := DigestBuiltEvent("host-a", DigestBuilt{Date: "2026-09-13", Revision: 1}, at)
	d2 := DigestBuiltEvent("host-a", DigestBuilt{Date: "2026-09-13", Revision: 2}, at)
	assert.NotEqual(t, d1.EventID, d2.EventID, "a rebuild is a new build")
	r := IssueReopenedEvent("host-a", IssueReopened{Fingerprint: "fl1:ab", IssueUID: "U", Date: "2026-09-13"}, at)
	require.NotNil(t, r.CausationID)
	assert.Equal(t, RecurredEventID("host-a", "fl1:ab", "2026-09-13"), *r.CausationID)
	l1 := IssueLinkedEvent("host-a", IssueLinked{Fingerprint: "fl1:ab", IssueUID: "U"}, at)
	l2 := IssueLinkedEvent("host-a", IssueLinked{Fingerprint: "fl1:ab", IssueUID: "U"}, at)
	assert.NotEqual(t, l1.EventID, l2.EventID, "each manual link is its own event")
}

func TestDigestBuiltFromSnapshot(t *testing.T) {
	sig := func(k friction.Kind) friction.Signal {
		return friction.Signal{Kind: k, SubjectID: "s", SubjectKind: friction.SubjectSession}
	}
	snap := friction.DigestSnapshot{
		Date: "2026-09-13", Timezone: "UTC", RulesVersion: friction.RulesVersion, SessionsScanned: 3,
		Signals:  []friction.Signal{sig(friction.KindCorrection), sig(friction.KindCorrection), sig(friction.KindError), sig(friction.KindFrustration)},
		P0Alerts: map[string][]string{"zeta": {"a", "b", "c"}, "bash": {"a", "b", "c"}},
	}
	run := uuid.MustParse("01920000-0000-7000-8000-000000000002")
	d := DigestBuiltFromSnapshot(snap, friction.SummaryMeta{}, 2, "sha", &run)
	assert.Equal(t, map[string]int{
		"correction": 2, "error": 1, "workaround": 0, "deferral": 0, "pattern": 0,
		"frustration": 1, "interruption": 0, "p0_alerts": 2, "sessions_scanned": 3,
	}, d.Counts)
	assert.Equal(t, []string{"bash", "zeta"}, d.P0Tools)
	assert.Equal(t, strings.SplitN(friction.HumanSummary(snap, friction.SummaryMeta{}), "\n", 2)[0], d.CountsLine)

	e := DigestBuiltEvent("host-a", d, at)
	assert.Equal(t, ledger.ClassProjection, e.EventClass)
	assert.Equal(t, ledger.TierStructured, e.PayloadTier)
	assert.Equal(t, "friction-digest:2026-09-13", *e.ObjectRef)
	require.NotNil(t, e.CorrelationID)
	assert.Equal(t, run, *e.CorrelationID)
	p := payload(t, e)
	assert.ElementsMatch(t, []string{
		"kind", "subsystem", "summary", "date", "timezone", "rules_version", "counts",
		"p0_tools", "markdown_sha256", "revision",
	}, keys(p))
	assert.Equal(t, serdejson.Number("2"), p["revision"])
}

// Roadmap acceptance "Events carry no transcript text": only the 80-rune
// title may carry context, never Text, Evidence or Label beyond it.
func TestEventsCarryNoTranscriptText(t *testing.T) {
	sig := friction.Signal{
		Kind: friction.KindCorrection, SubjectID: "s1", SubjectKind: friction.SubjectSession,
		Text: strings.Repeat("a", 90) + " SECRET-TAIL-TEXT", Evidence: "EVIDENCE-SECRET", Label: "LABEL-SECRET",
	}
	pt := PatternTransition{
		Fingerprint: sig.Fingerprint(), Title: sig.Title(), SubjectID: sig.SubjectID,
		Date: "2026-09-13", SignalKind: sig.Kind,
	}
	for _, e := range []ledger.Event{
		PatternFirstSeenEvent("host-a", pt, at),
		PatternRecurredEvent("host-a", pt, at),
		IssueFiledEvent("host-a", IssueFiled{Fingerprint: pt.Fingerprint, Title: pt.Title, IssueUID: "U"}, at),
	} {
		b, err := e.MarshalSerde()
		require.NoError(t, err)
		for _, secret := range []string{"SECRET-TAIL-TEXT", "EVIDENCE-SECRET", "LABEL-SECRET"} {
			assert.NotContains(t, string(b), secret)
		}
	}
}
