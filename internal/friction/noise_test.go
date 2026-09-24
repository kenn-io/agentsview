package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/serdejson"
)

// Ports jilog detectors.rs error_text_classification (:1190-1209).
func TestErrorText(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		wantKind errTextKind
		wantText string
	}{
		{"empty", `{}`, errTextAbsent, ""},
		{"error null", `{"error":null}`, errTextAbsent, ""},
		{"error string", `{"error":"x"}`, errTextText, "x"},
		{"error object message", `{"error":{"message":"m"}}`, errTextText, "m"},
		{"top message", `{"message":"top"}`, errTextText, "top"},
		{"error object without message", `{"error":{"code":"c"}}`, errTextUnrecognized, ""},
		{"error number", `{"error":7}`, errTextUnrecognized, ""},
		{"message number", `{"message":7}`, errTextUnrecognized, ""},
		{"error beside message", `{"error":"x","message":"y"}`, errTextUnrecognized, ""},
		{"null error with message", `{"error":null,"message":"y"}`, errTextText, "y"},
		{"null message beside error", `{"error":"x","message":null}`, errTextText, "x"},
		{"both null", `{"error":null,"message":null}`, errTextAbsent, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := serdejson.Decode([]byte(tt.data))
			require.NoError(t, err)
			kind, text := errorText(v.(map[string]any))
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantText, text)
		})
	}
}

// TestBareTimeoutDigitClass pins the RE2 \d difference: Rust regex's \d
// is Unicode (fullwidth "３０" matches, captured with regex 1.12.3); RE2's
// is ASCII, so the fullwidth sentence is emitted here.
func TestBareTimeoutDigitClass(t *testing.T) {
	tests := []struct {
		name, text string
		want       int
	}{
		{"ASCII digits suppressed", "Command timed out after 30 seconds", 0},
		{"fullwidth digits emitted", "Command timed out after ３０ seconds", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectErrors([]Message{tool("bash", `{"error":"`+tt.text+`","success":false}`)}, "s1")
			assert.Len(t, got, tt.want)
		})
	}
}
