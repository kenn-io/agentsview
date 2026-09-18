package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeToolHealth_NoCalls(t *testing.T) {
	got := ComputeToolHealth(nil)
	assert.Equal(t, ToolHealthSignals{}, got)
}

func TestIsFailure_BashContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			"successful output",
			"hello world\n",
			false,
		},
		{
			"command not found",
			"bash: foo: command not found",
			true,
		},
		{
			"permission denied",
			"Permission denied: /etc/shadow",
			true,
		},
		{
			"python traceback",
			"Traceback (most recent call last):\n" +
				"  File \"x.py\", line 1\n" +
				"NameError: name 'x' is not defined",
			true,
		},
		{
			"go panic",
			"goroutine 1 [running]:\nmain.main()\n",
			true,
		},
		{
			"js stack trace 3 lines",
			"Error: boom\n" +
				"  at Object.foo (app.js:1)\n" +
				"  at bar (app.js:2)\n" +
				"  at baz (app.js:3)\n",
			true,
		},
		{
			"js stack trace 2 lines not enough",
			"Error: boom\n" +
				"  at Object.foo (app.js:1)\n" +
				"  at bar (app.js:2)\n",
			false,
		},
		{
			"exit code with companion",
			"fatal: not a git repository\n" +
				"exit status 128",
			true,
		},
		{
			"exit code alone not failure",
			"exit status 1",
			false,
		},
		{
			"test runner with exit code",
			"FAIL TestSomething 0.5s\nexit status 1",
			false,
		},
		{
			"empty grep result",
			"",
			false,
		},
		{
			"exit code with no such file",
			"ls: No such file or directory\nexit status 2",
			true,
		},
		{
			"exit code with permission denied",
			"Permission denied\nexit code 1",
			true,
		},
		{
			"exit code with panic",
			"panic: runtime error\nexit status 2",
			true,
		},
		{
			"exit code with traceback",
			"Traceback (most recent call last):\n" +
				"  File \"x.py\"\nexit status 1",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := ToolCallRow{
				Category:      "Bash",
				ResultContent: tt.content,
			}
			assert.Equal(t, tt.want, IsFailure(row))
		})
	}
}

func TestIsFailure_EditWrite(t *testing.T) {
	tests := []struct {
		name     string
		category string
		content  string
		want     bool
	}{
		{
			"edit failed",
			"Edit",
			"FAILED: old_string not found",
			true,
		},
		{
			"edit success",
			"Edit",
			"Edit applied successfully",
			false,
		},
		{
			"write failed",
			"Write",
			"FAILED to write file",
			true,
		},
		{
			"write success",
			"Write",
			"File written.",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := ToolCallRow{
				Category:      tt.category,
				ResultContent: tt.content,
			}
			assert.Equal(t, tt.want, IsFailure(row))
		})
	}
}

func TestIsFailure_SearchNotFailure(t *testing.T) {
	row := ToolCallRow{
		Category:      "Search",
		ResultContent: "",
	}
	assert.False(t, IsFailure(row), "empty search result should not be failure")
}

func TestIsFailure_EventStatus(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		content     string
		wantFailure bool
	}{
		{
			"errored",
			"errored",
			"everything is fine",
			true,
		},
		{
			"cancelled",
			"cancelled",
			"",
			true,
		},
		{
			"completed",
			"completed",
			"command not found",
			false,
		},
		{
			"running",
			"running",
			"command not found",
			false,
		},
		{
			"errored overrides clean content",
			"errored",
			"all good",
			true,
		},
		{
			"completed overrides error content",
			"completed",
			"FAILED",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := ToolCallRow{
				Category:      "Bash",
				EventStatus:   tt.status,
				ResultContent: tt.content,
			}
			assert.Equal(t, tt.wantFailure, IsFailure(row))
		})
	}
}

func TestConsecutiveFailureMax(t *testing.T) {
	calls := []ToolCallRow{
		{Category: "Bash", ResultContent: "ok"},
		{Category: "Bash", ResultContent: "command not found"},
		{Category: "Bash", ResultContent: "Permission denied"},
		{Category: "Bash", ResultContent: "Permission denied"},
		{Category: "Bash", ResultContent: "ok"},
		{Category: "Bash", ResultContent: "command not found"},
	}
	got := ComputeToolHealth(calls)
	assert.Equal(t, 3, got.ConsecutiveFailureMax)
	assert.Equal(t, 4, got.FailureSignalCount)
}

func TestRetryCount(t *testing.T) {
	tests := []struct {
		name  string
		calls []ToolCallRow
		want  int
	}{
		{
			"2 identical not retry",
			[]ToolCallRow{
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
			},
			0,
		},
		{
			"3 identical = 2 retries",
			[]ToolCallRow{
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
			},
			2,
		},
		{
			"5 identical = 4 retries",
			[]ToolCallRow{
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
			},
			4,
		},
		{
			"different tool breaks streak",
			[]ToolCallRow{
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Read", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
			},
			0,
		},
		{
			"different input breaks streak",
			[]ToolCallRow{
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"pwd"}`},
			},
			0,
		},
		{
			"two groups",
			[]ToolCallRow{
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`},
				{ToolName: "Read", InputJSON: `{"f":"a"}`},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`},
			},
			5, // 2 + 3
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, countRetries(tt.calls))
		})
	}
}

func TestEditChurn(t *testing.T) {
	tests := []struct {
		name  string
		calls []ToolCallRow
		want  int
	}{
		{
			"2 edits same file no churn",
			[]ToolCallRow{
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"x"}`,
					MessageOrdinal: 1,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"y"}`,
					MessageOrdinal: 5,
				},
			},
			0,
		},
		{
			"3 edits same file within 10 ordinals",
			[]ToolCallRow{
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"x"}`,
					MessageOrdinal: 1,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"y"}`,
					MessageOrdinal: 5,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"z"}`,
					MessageOrdinal: 9,
				},
			},
			1,
		},
		{
			"3 edits same file outside window",
			[]ToolCallRow{
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"x"}`,
					MessageOrdinal: 1,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"y"}`,
					MessageOrdinal: 5,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go","old":"z"}`,
					MessageOrdinal: 20,
				},
			},
			0,
		},
		{
			"write category counts",
			[]ToolCallRow{
				{
					Category:       "Write",
					InputJSON:      `{"file_path":"b.go","content":"x"}`,
					MessageOrdinal: 1,
				},
				{
					Category:       "Write",
					InputJSON:      `{"file_path":"b.go","content":"y"}`,
					MessageOrdinal: 3,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"b.go","old":"z"}`,
					MessageOrdinal: 7,
				},
			},
			1,
		},
		{
			"different files no churn",
			[]ToolCallRow{
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 1,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"b.go"}`,
					MessageOrdinal: 2,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"c.go"}`,
					MessageOrdinal: 3,
				},
			},
			0,
		},
		{
			"non edit category ignored",
			[]ToolCallRow{
				{
					Category:       "Bash",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 1,
				},
				{
					Category:       "Bash",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 2,
				},
				{
					Category:       "Bash",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 3,
				},
			},
			0,
		},
		{
			"boundary exactly 10 span",
			[]ToolCallRow{
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 0,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 5,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 10,
				},
			},
			// hi - lo = 10, which is NOT < 10
			0,
		},
		{
			"boundary span 9",
			[]ToolCallRow{
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 0,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 5,
				},
				{
					Category:       "Edit",
					InputJSON:      `{"file_path":"a.go"}`,
					MessageOrdinal: 9,
				},
			},
			1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, countEditChurn(tt.calls))
		})
	}
}

func TestExtractFilePath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"normal json",
			`{"file_path":"foo/bar.go","old":"x"}`,
			"foo/bar.go",
		},
		{
			"no file_path",
			`{"command":"ls"}`,
			"",
		},
		{
			"empty path",
			`{"file_path":""}`,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractFilePath(tt.input))
		})
	}
}

func TestComputeToolHealth_Combined(t *testing.T) {
	calls := []ToolCallRow{
		// 3 retries of same bash command (all fail)
		{
			ToolName:       "Bash",
			Category:       "Bash",
			InputJSON:      `{"cmd":"make build"}`,
			ResultContent:  "command not found",
			MessageOrdinal: 1,
		},
		{
			ToolName:       "Bash",
			Category:       "Bash",
			InputJSON:      `{"cmd":"make build"}`,
			ResultContent:  "command not found",
			MessageOrdinal: 2,
		},
		{
			ToolName:       "Bash",
			Category:       "Bash",
			InputJSON:      `{"cmd":"make build"}`,
			ResultContent:  "command not found",
			MessageOrdinal: 3,
		},
		// Edit churn: 3 edits to same file within span
		{
			ToolName:       "Edit",
			Category:       "Edit",
			InputJSON:      `{"file_path":"main.go","old":"a"}`,
			ResultContent:  "ok",
			MessageOrdinal: 4,
		},
		{
			ToolName:       "Edit",
			Category:       "Edit",
			InputJSON:      `{"file_path":"main.go","old":"b"}`,
			ResultContent:  "ok",
			MessageOrdinal: 6,
		},
		{
			ToolName:       "Edit",
			Category:       "Edit",
			InputJSON:      `{"file_path":"main.go","old":"c"}`,
			ResultContent:  "ok",
			MessageOrdinal: 8,
		},
	}

	got := ComputeToolHealth(calls)

	assert.Equal(t, 3, got.FailureSignalCount)
	assert.Equal(t, 2, got.RetryCount)
	assert.Equal(t, 1, got.EditChurnCount)
	assert.Equal(t, 3, got.ConsecutiveFailureMax)
}
