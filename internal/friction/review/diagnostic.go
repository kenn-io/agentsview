package review

import (
	"regexp"
	"strings"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/serdejson"
)

// DiagnosticPayloadKind marks a producer-written diagnostic event (§11.4).
// PR 5's DiagnosticSource (runner.go) is the interface these signals feed.
const DiagnosticPayloadKind = "diagnostic"

var diagnosticNameRe = regexp.MustCompile(`^[a-z0-9._:-]{1,64}$`)

type diagnosticFields struct{ Diagnostic, Identity, Detail, Seat string }

// parseDiagnostic reports whether e claims to be a diagnostic and, if so,
// what is wrong with it ("" when valid).
func parseDiagnostic(e ledger.Event) (diagnosticFields, bool, string) {
	var f diagnosticFields
	obj, ok := e.Payload.(map[string]any)
	if !ok {
		return f, false, ""
	}
	if kind, _ := obj["kind"].(string); kind != DiagnosticPayloadKind {
		return f, false, ""
	}
	if e.EventClass != ledger.ClassHealth {
		return f, true, "event_class must be health"
	}
	f.Diagnostic, _ = obj["diagnostic"].(string)
	if !diagnosticNameRe.MatchString(f.Diagnostic) {
		return f, true, "diagnostic must match ^[a-z0-9._:-]{1,64}$"
	}
	f.Identity, _ = obj["identity"].(string)
	if strings.TrimSpace(f.Identity) == "" {
		return f, true, "identity is required"
	}
	detail, ok := obj["detail"].(string)
	if !ok {
		return f, true, "detail must be a string"
	}
	f.Detail = detail
	if raw, present := obj["seat"]; present && raw != nil {
		seat, ok := raw.(string)
		if !ok {
			return f, true, "seat must be a string"
		}
		f.Seat = seat
	}
	return f, true, ""
}

// DiagnosticSignal turns one diagnostic event into a P3 error signal
// (§11.4), with the values jilog's record() produced
// (worker_signals.rs:83-109). The envelope goes through DetectErrors, so the
// noise filter and message extraction are jilog's; a diagnostic the filter
// drops yields false.
func DiagnosticSignal(e ledger.Event, sourceLabel string) (friction.Signal, bool) {
	f, isDiagnostic, problem := parseDiagnostic(e)
	if !isDiagnostic || problem != "" {
		return friction.Signal{}, false
	}
	envelope := serdejson.CompactString(map[string]any{
		"error": f.Identity + ": " + f.Detail, "success": false,
	})
	sigs := friction.DetectErrors([]friction.Message{{
		Role: "tool", ToolName: f.Diagnostic, Text: envelope, HadToolResult: true, Timestamp: e.Timestamp,
	}}, f.Identity)
	if len(sigs) == 0 {
		return friction.Signal{}, false
	}
	sig := sigs[0]
	sig.SubjectKind = friction.SubjectDiagnostic
	sig.Ordinal, sig.CallIndex = nil, nil
	sig.Dims = friction.Dims{Seat: f.Seat, Machine: sourceLabel}
	sig.OccurredAt = e.Timestamp.UTC()
	return sig, true
}
