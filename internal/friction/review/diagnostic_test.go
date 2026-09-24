package review

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
)

var diagAt = time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

func diagEvent(class ledger.EventClass, payload map[string]any) ledger.Event {
	return ledger.Event{
		EventID: ledger.NewEventID(), Zone: "default", Source: "host-a", SourceSeq: 1,
		Timestamp: diagAt, EventClass: class, PayloadTier: ledger.TierStructured, Payload: payload,
	}
}

func validDiag() map[string]any {
	return map[string]any{
		"kind": "diagnostic", "subsystem": "ci", "diagnostic": "ci_nightly_failed",
		"identity": "run-42:ci_nightly_failed", "detail": "lint step exited 1", "seat": "seat-02",
		"summary": "lint step exited 1",
	}
}

func TestDiagnosticSignal(t *testing.T) {
	t.Run("valid_event_becomes_p3_error", func(t *testing.T) {
		sig, ok := DiagnosticSignal(diagEvent(ledger.ClassHealth, validDiag()), "host-a-label")
		require.True(t, ok)
		assert.Equal(t, friction.KindError, sig.Kind)
		assert.Equal(t, friction.SubjectDiagnostic, sig.SubjectKind)
		assert.Equal(t, "run-42:ci_nightly_failed", sig.SubjectID)
		assert.Equal(t, "ci_nightly_failed", sig.ToolName)
		assert.Equal(t, "run-42:ci_nightly_failed: lint step exited 1", sig.Text)
		assert.Equal(t, friction.Dims{Seat: "seat-02", Machine: "host-a-label"}, sig.Dims)
		assert.Nil(t, sig.Ordinal)
		assert.Nil(t, sig.CallIndex)
		assert.Equal(t, diagAt, sig.OccurredAt)
		assert.True(t, strings.HasPrefix(sig.Title(), "[friction/error] ci_nightly_failed: run-42:ci_nightly_failed"))
	})

	t.Run("seat_is_optional", func(t *testing.T) {
		p := validDiag()
		delete(p, "seat")
		sig, ok := DiagnosticSignal(diagEvent(ledger.ClassHealth, p), "")
		require.True(t, ok)
		assert.Empty(t, sig.Dims.Seat)
	})

	for _, tc := range []struct {
		name    string
		class   ledger.EventClass
		mutate  func(map[string]any)
		isDiag  bool
		problem string
	}{
		{"other_kind_is_silently_not_diagnostic", ledger.ClassHealth, func(p map[string]any) { p["kind"] = "friction.pattern.first_seen" }, false, ""},
		{"null_payload_is_not_diagnostic", ledger.ClassHealth, nil, false, ""},
		{"wrong_class", ledger.ClassDecision, func(map[string]any) {}, true, "event_class must be health"},
		{"uppercase_diagnostic", ledger.ClassHealth, func(p map[string]any) { p["diagnostic"] = "CI" }, true, "diagnostic must match ^[a-z0-9._:-]{1,64}$"},
		{"long_diagnostic", ledger.ClassHealth, func(p map[string]any) { p["diagnostic"] = strings.Repeat("a", 65) }, true, "diagnostic must match ^[a-z0-9._:-]{1,64}$"},
		{"missing_identity", ledger.ClassHealth, func(p map[string]any) { delete(p, "identity") }, true, "identity is required"},
		{"blank_identity", ledger.ClassHealth, func(p map[string]any) { p["identity"] = "  " }, true, "identity is required"},
		{"missing_detail", ledger.ClassHealth, func(p map[string]any) { delete(p, "detail") }, true, "detail must be a string"},
		{"numeric_seat", ledger.ClassHealth, func(p map[string]any) { p["seat"] = 7 }, true, "seat must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e ledger.Event
			if tc.mutate == nil {
				e = diagEvent(tc.class, nil)
				e.Payload = nil
			} else {
				p := validDiag()
				tc.mutate(p)
				e = diagEvent(tc.class, p)
			}
			_, isDiag, problem := parseDiagnostic(e)
			assert.Equal(t, tc.isDiag, isDiag)
			assert.Equal(t, tc.problem, problem)
			_, ok := DiagnosticSignal(e, "x")
			assert.False(t, ok)
		})
	}

	// Generic form of jilog's P0 exclusion (detectors.rs:609-633): three
	// distinct diagnostic subjects with one tool never raise P0.
	t.Run("diagnostics_never_raise_p0", func(t *testing.T) {
		var errs []friction.Signal
		for _, id := range []string{"a", "b", "c"} {
			p := validDiag()
			p["identity"] = id + ":ci_nightly_failed"
			sig, ok := DiagnosticSignal(diagEvent(ledger.ClassHealth, p), "")
			require.True(t, ok)
			errs = append(errs, sig)
		}
		assert.Empty(t, friction.DetectP0Alerts(errs, func(string) bool { return false }))
	})
}
