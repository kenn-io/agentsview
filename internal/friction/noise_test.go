package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
			got := DetectErrors([]Message{failedTool("bash", tt.text)}, "s1")
			assert.Len(t, got, tt.want)
		})
	}
}
