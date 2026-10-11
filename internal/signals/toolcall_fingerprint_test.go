package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The frontend's toolCallFingerprint tests pin the same vectors, so the two
// sides agree on what a fingerprint means.
func TestToolCallRowFingerprint(t *testing.T) {
	for _, tc := range []struct{ tool, input, want string }{
		{"Grep", `{"pattern":"retryLimit"}`, "7805cd6e"},
		{"Read", `{"file_path":"界.go"}`, "eee0ae21"},
		{"", "", "050c5d1f"},
	} {
		assert.Equal(t, tc.want, ToolCallRow{ToolName: tc.tool, InputJSON: tc.input}.Fingerprint(), tc.tool)
	}
	assert.NotEqual(t,
		ToolCallRow{ToolName: "Grep", InputJSON: `{"pattern":"a"}`}.Fingerprint(),
		ToolCallRow{ToolName: "Grep", InputJSON: `{"pattern":"b"}`}.Fingerprint())
	assert.NotEqual(t,
		ToolCallRow{ToolName: "Grep", InputJSON: "x"}.Fingerprint(),
		ToolCallRow{ToolName: "Read", InputJSON: "x"}.Fingerprint())
}
