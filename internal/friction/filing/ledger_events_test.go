package filing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/frictionevents"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/ledger"
)

type filingLedgerSink struct {
	events []ledger.Event
	zones  []string
	err    error
}

func (s *filingLedgerSink) Append(_ context.Context, zone string, events []ledger.Event) error {
	s.zones = append(s.zones, zone)
	s.events = append(s.events, events...)
	return s.err
}

func (s *filingLedgerSink) kinds() []string {
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Payload.(map[string]any)["kind"].(string))
	}
	return out
}

func ledgerHarness(t *testing.T) (*harness, *filingLedgerSink) {
	t.Helper()
	h := newHarness(t)
	sink := &filingLedgerSink{}
	h.f.Ledger = sink
	h.f.LedgerSource = "av-hub"
	return h, sink
}

func TestFilerLedgerEvents(t *testing.T) {
	sig := errSig("claude:s1", "Bash", "boom")
	run := runCtx()
	run.RunID = "01920000-0000-7000-8000-000000000003"

	t.Run("new_issue_emits_once_with_digest_correlation", func(t *testing.T) {
		h, sink := ledgerHarness(t)
		link, err := h.f.File(t.Context(), sig, run)
		require.NoError(t, err)
		require.Equal(t, db.FrictionLinkStateLinked, link.State)
		_, err = h.f.File(t.Context(), sig, run)
		require.NoError(t, err)
		require.Equal(t, []string{frictionevents.KindIssueFiled}, sink.kinds())
		assert.Equal(t, []string{""}, sink.zones)
		e := sink.events[0]
		p := e.Payload.(map[string]any)
		assert.Equal(t, "created", p["link_source"])
		assert.Equal(t, "01J00000000000000000000002", p["kata_instance_uid"])
		assert.Equal(t, link.IssueUID, p["issue_uid"])
		require.NotNil(t, e.CorrelationID)
		assert.Equal(t, run.RunID, e.CorrelationID.String())
		require.NotNil(t, e.ObjectRef)
		assert.Equal(t, "friction:"+sig.Fingerprint(), *e.ObjectRef)
	})

	t.Run("done_match_emits_filed_and_reopened", func(t *testing.T) {
		h, sink := ledgerHarness(t)
		h.kata.AddIssue(katatest.Issue{
			Title: sig.Title(), Status: "closed", ClosedReason: "done",
			Metadata: map[string]any{"friction.fingerprint": sig.Fingerprint()},
		})
		_, err := h.f.File(t.Context(), sig, run)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{frictionevents.KindIssueFiled, frictionevents.KindIssueReopened}, sink.kinds())
		for _, e := range sink.events {
			if e.Payload.(map[string]any)["kind"] != frictionevents.KindIssueReopened {
				continue
			}
			require.NotNil(t, e.CausationID)
			assert.Equal(t, frictionevents.RecurredEventID("av-hub", sig.Fingerprint(), run.Date), *e.CausationID)
			require.NotNil(t, e.CorrelationID)
			assert.Equal(t, run.RunID, e.CorrelationID.String())
		}
	})

	t.Run("wontfix_match_never_reopens", func(t *testing.T) {
		h, sink := ledgerHarness(t)
		h.kata.AddIssue(katatest.Issue{
			Title: sig.Title(), Status: "closed", ClosedReason: "wontfix",
			Metadata: map[string]any{"friction.fingerprint": sig.Fingerprint()},
		})
		_, err := h.f.File(t.Context(), sig, run)
		require.NoError(t, err)
		assert.Equal(t, []string{frictionevents.KindIssueFiled}, sink.kinds())
	})

	t.Run("manual_link_emits_redacted_title", func(t *testing.T) {
		h, sink := ledgerHarness(t)
		h.f.Redact = func(string) string { return "redacted" }
		is := h.kata.AddIssue(katatest.Issue{Title: "private title", Status: "open"})
		_, err := h.f.Link(t.Context(), sig.Fingerprint(), is.UID)
		require.NoError(t, err)
		require.Equal(t, []string{frictionevents.KindIssueLinked}, sink.kinds())
		p := sink.events[0].Payload.(map[string]any)
		assert.Equal(t, "manual", p["link_source"])
		assert.Equal(t, "redacted", p["summary"])
	})

	t.Run("ambiguous_match_emits_nothing", func(t *testing.T) {
		h, sink := ledgerHarness(t)
		for range 2 {
			h.kata.AddIssue(katatest.Issue{
				Title: sig.Title(), Status: "open",
				Metadata: map[string]any{"friction.fingerprint": sig.Fingerprint()},
			})
		}
		link, err := h.f.File(t.Context(), sig, run)
		require.NoError(t, err)
		assert.Equal(t, db.FrictionLinkStateNeedsHuman, link.State)
		assert.Empty(t, sink.events)
	})

	t.Run("diagnostic_create_uses_force_new", func(t *testing.T) {
		h, _ := ledgerHarness(t)
		diag := friction.Signal{
			Kind: friction.KindError, SubjectKind: friction.SubjectDiagnostic,
			SubjectID: "run-1:ci", Detector: "error", ToolName: "ci", Text: "run-1:ci: failed",
		}
		_, err := h.f.File(t.Context(), diag, run)
		require.NoError(t, err)
		creates := h.kata.RequestsMatching("POST", "/issues")
		require.Len(t, creates, 1)
		assert.Contains(t, string(creates[0].Body), `"force_new":true`)
	})

	t.Run("ledger_failure_does_not_fail_filing", func(t *testing.T) {
		h, sink := ledgerHarness(t)
		sink.err = errors.New("ledger unavailable")
		link, err := h.f.File(t.Context(), sig, run)
		require.NoError(t, err)
		assert.Equal(t, db.FrictionLinkStateLinked, link.State)
		assert.Equal(t, []string{frictionevents.KindIssueFiled}, sink.kinds())
	})

	t.Run("digest_filing_uses_inline_sink", func(t *testing.T) {
		h, apiSink := ledgerHarness(t)
		inlineSink := &filingLedgerSink{}
		h.f.InlineLedger = inlineSink
		inlineRun := run
		inlineRun.InlineLedger = true
		_, err := h.f.File(t.Context(), sig, inlineRun)
		require.NoError(t, err)
		assert.Equal(t, []string{frictionevents.KindIssueFiled}, inlineSink.kinds())
		assert.Empty(t, apiSink.events)
	})
}
