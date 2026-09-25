package friction

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSignalTitle(t *testing.T) {
	long := strings.Repeat("é", 100)
	tests := []struct {
		name string
		sig  Signal
		want string
	}{
		{"correction", Signal{Kind: KindCorrection, SubjectID: "s1", Text: "no, use the other cli"}, "[friction/correction] s1: no, use the other cli"},
		{"correction truncates at 80 runes", Signal{Kind: KindCorrection, SubjectID: "s1", Text: long}, "[friction/correction] s1: " + strings.Repeat("é", 80)},
		{"error excludes session ID", Signal{Kind: KindError, SubjectID: "s1", ToolName: "bash", Text: "cargo test: mismatched types"}, "[friction/error] bash: cargo test: mismatched types"},
		{"diagnostic error keeps supplied identity", Signal{Kind: KindError, SubjectID: "diag-1", SubjectKind: SubjectDiagnostic, ToolName: "ledger_event", Text: "diag-1: 1 row"}, "[friction/error] ledger_event: diag-1: 1 row"},
		{"error truncates message", Signal{Kind: KindError, ToolName: "bash", Text: long}, "[friction/error] bash: " + strings.Repeat("é", 80)},
		{"workaround", Signal{Kind: KindWorkaround, SubjectID: "s1", Label: "TODO", Text: "update the task list"}, "[friction/workaround] TODO: update the task list"},
		{"pattern", Signal{Kind: KindPattern, SubjectID: "s1", Label: "retry_loop", Text: "same call six times"}, "[friction/pattern] s1: same call six times"},
		{"deferral", Signal{Kind: KindDeferral, SubjectID: "s1", Label: "next session"}, "[friction/deferral] s1: next session"},
		{"deferral truncates label", Signal{Kind: KindDeferral, SubjectID: "s1", Label: long}, "[friction/deferral] s1: " + strings.Repeat("é", 80)},
		{"frustration", Signal{Kind: KindFrustration, SubjectID: "s1", Text: "this is broken again"}, "[friction/frustration] s1: this is broken again"},
		{"interruption ignores text", Signal{Kind: KindInterruption, SubjectID: "s1", Text: "[Request interrupted by user]"}, "[friction/interruption] s1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.sig.Title())
		})
	}
}

func TestInterruptionTitleIsStableAsCountGrows(t *testing.T) {
	first := Signal{Kind: KindInterruption, SubjectID: "s1", Ordinal: new(3)}
	later := Signal{Kind: KindInterruption, SubjectID: "s1", Ordinal: new(40), Text: "ignored"}
	assert.Equal(t, first.Title(), later.Title())
	assert.Equal(t, first.Fingerprint(), later.Fingerprint())
}

func TestSignalFingerprint(t *testing.T) {
	sig := Signal{Kind: KindDeferral, SubjectID: "s1", Label: "next session"}
	sum := sha256.Sum256([]byte("[friction/deferral] s1: next session"))
	assert.Equal(t, "fl1:"+hex.EncodeToString(sum[:]), sig.Fingerprint())
	assert.Len(t, sig.Fingerprint(), 4+64)

	// Dimensions and position do not change title identity.
	other := sig
	other.Dims = Dims{Seat: "seat-02"}
	other.Ordinal = new(4)
	other.Seq = 3
	assert.Equal(t, sig.Fingerprint(), other.Fingerprint())

	// Error titles omit session ID, so identical errors share a fingerprint.
	a := Signal{Kind: KindError, SubjectID: "a", ToolName: "bash", Text: "boom"}
	b := Signal{Kind: KindError, SubjectID: "b", ToolName: "bash", Text: "boom"}
	assert.Equal(t, a.Fingerprint(), b.Fingerprint())

	// The producer's diagnostic identity is part of the message, so separate
	// diagnostics remain distinct without a tool-specific title rule.
	diagA := Signal{Kind: KindError, SubjectKind: SubjectDiagnostic, ToolName: "ledger_event", Text: "diag-1: 1 row"}
	diagB := Signal{Kind: KindError, SubjectKind: SubjectDiagnostic, ToolName: "ledger_event", Text: "diag-2: 1 row"}
	assert.NotEqual(t, diagA.Fingerprint(), diagB.Fingerprint())
}
