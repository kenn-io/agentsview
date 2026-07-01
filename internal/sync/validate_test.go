package sync

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestSanitizeMessage(t *testing.T) {
	tests := []struct {
		name        string
		in          db.Message
		wantRole    string
		wantContent string
		wantModel   string
		wantCtx     int
		wantOut     int
		wantTS      string
		wantStats   validationStats
	}{
		{
			name: "clean message untouched",
			in: db.Message{
				Role:          "assistant",
				Content:       "hello world\nsecond line",
				Model:         "claude-opus-4",
				ContextTokens: 1000,
				OutputTokens:  50,
				Timestamp:     "2026-06-20T10:00:00Z",
			},
			wantRole:    "assistant",
			wantContent: "hello world\nsecond line",
			wantModel:   "claude-opus-4",
			wantCtx:     1000,
			wantOut:     50,
			wantTS:      "2026-06-20T10:00:00Z",
		},
		{
			name: "C1 control rune in model stripped",
			in: db.Message{
				Role:  "assistant",
				Model: "claude\u0085-opus", // U+0085 NEL is a C1 control
			},
			wantRole:  "assistant",
			wantModel: "claude-opus",
			wantStats: validationStats{ControlCharsStripped: 1},
		},
		{
			name: "ESC and BEL terminal escape in content stripped",
			in: db.Message{
				Role:    "assistant",
				Content: "before\x1b]0;title\x07after",
			},
			wantRole:    "assistant",
			wantContent: "before]0;titleafter",
			wantStats:   validationStats{ControlCharsStripped: 1},
		},
		{
			name: "tab newline cr preserved in content",
			in: db.Message{
				Role:    "user",
				Content: "a\tb\nc\rd",
			},
			wantRole:    "user",
			wantContent: "a\tb\nc\rd",
		},
		{
			name: "5MB printable model clamped",
			in: db.Message{
				Role:  "assistant",
				Model: strings.Repeat("a", 5_000_000),
			},
			wantRole:  "assistant",
			wantModel: strings.Repeat("a", maxModelLen),
			wantStats: validationStats{ModelClamped: 1},
		},
		{
			name: "dirty model prefix stripped before clamp",
			in: db.Message{
				Role:  "assistant",
				Model: strings.Repeat("\x00", maxModelLen+16) + "claude-opus-4",
			},
			wantRole:  "assistant",
			wantModel: "claude-opus-4",
			wantStats: validationStats{ControlCharsStripped: 1},
		},
		{
			name: "out-of-enum role coerced to blank",
			in: db.Message{
				Role:    "wizard",
				Content: "x",
			},
			wantRole:    "",
			wantContent: "x",
			wantStats:   validationStats{RoleCoerced: 1},
		},
		{
			name: "known system and tool roles preserved",
			in: db.Message{
				Role: "system",
			},
			wantRole: "system",
		},
		{
			name: "over-bound token counts clamped",
			in: db.Message{
				Role:          "assistant",
				ContextTokens: 9_000_000,
				OutputTokens:  3_000_000,
			},
			wantRole:  "assistant",
			wantCtx:   maxPlausibleTokens,
			wantOut:   maxPlausibleTokens,
			wantStats: validationStats{TokensClamped: 2},
		},
		{
			name: "negative token count floored",
			in: db.Message{
				Role:         "assistant",
				OutputTokens: -5,
			},
			wantRole:  "assistant",
			wantOut:   0,
			wantStats: validationStats{TokensClamped: 1},
		},
		{
			name: "out-of-window timestamp blanked",
			in: db.Message{
				Role:      "assistant",
				Timestamp: "1850-01-01T00:00:00Z",
			},
			wantRole:  "assistant",
			wantTS:    "",
			wantStats: validationStats{TimestampsBlanked: 1},
		},
		{
			name: "far-future timestamp blanked",
			in: db.Message{
				Role:      "assistant",
				Timestamp: "2999-01-01T00:00:00Z",
			},
			wantRole:  "assistant",
			wantTS:    "",
			wantStats: validationStats{TimestampsBlanked: 1},
		},
		{
			name: "unparseable timestamp left as-is",
			in: db.Message{
				Role:      "assistant",
				Timestamp: "not-a-timestamp",
			},
			wantRole: "assistant",
			wantTS:   "not-a-timestamp",
		},
		{
			name: "empty timestamp untouched",
			in: db.Message{
				Role:      "user",
				Timestamp: "",
			},
			wantRole: "user",
			wantTS:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.in
			stats := sanitizeMessage(&m)
			assert.Equal(t, tc.wantRole, m.Role, "role")
			assert.Equal(t, tc.wantContent, m.Content, "content")
			assert.Equal(t, tc.wantModel, m.Model, "model")
			assert.Equal(t, tc.wantCtx, m.ContextTokens, "context tokens")
			assert.Equal(t, tc.wantOut, m.OutputTokens, "output tokens")
			assert.Equal(t, tc.wantTS, m.Timestamp, "timestamp")
			assert.Equal(t, tc.wantStats, stats, "stats")
		})
	}
}

// TestSanitizeMessageRecomputesContentLength verifies that ContentLength
// is reduced by the stripped-byte delta. When a parser set it from the
// raw content length, after control runes are stripped it must match the
// actually-stored byte length, not the original raw length.
func TestSanitizeMessageRecomputesContentLength(t *testing.T) {
	raw := "before\x1b]0;title\x07after"
	m := db.Message{
		Role:          "assistant",
		Content:       raw,
		ContentLength: len(raw), // parser-set raw length
	}
	require.Greater(t, len(raw), len("before]0;titleafter"),
		"raw must be longer than sanitized for this test to matter")

	stats := sanitizeMessage(&m)

	assert.Equal(t, "before]0;titleafter", m.Content)
	assert.Equal(t, len(m.Content), m.ContentLength,
		"ContentLength must match sanitized content byte length")
	assert.Less(t, m.ContentLength, len(raw),
		"ContentLength must drop below the raw length after stripping")
	assert.Equal(t, 1, stats.ControlCharsStripped)

	// Idempotent: a second pass strips nothing and leaves length unchanged.
	wantLen := m.ContentLength
	second := sanitizeMessage(&m)
	assert.Equal(t, validationStats{}, second, "second pass must be a no-op")
	assert.Equal(t, wantLen, m.ContentLength, "length stable on re-run")
}

// TestSanitizeMessageContentLengthDelta verifies the ContentLength
// adjustment is a delta, not an overwrite: a parser-set semantic length
// that differs from len(Content) is preserved when nothing is stripped,
// and reduced by exactly the removed-byte count (not overwritten to
// len(Content)) when control runes are stripped.
func TestSanitizeMessageContentLengthDelta(t *testing.T) {
	t.Run("semantic length preserved when nothing stripped", func(t *testing.T) {
		content := "clean visible content"
		// Model a thinking/reasoning-inclusive length that intentionally
		// exceeds len(Content) by 100 bytes.
		semantic := len(content) + 100
		m := db.Message{
			Role:          "assistant",
			Content:       content,
			ContentLength: semantic,
		}

		stats := sanitizeMessage(&m)

		assert.Equal(t, content, m.Content, "clean content untouched")
		assert.Equal(t, 0, stats.ControlCharsStripped, "nothing should be stripped")
		assert.Equal(t, semantic, m.ContentLength,
			"semantic ContentLength must be left untouched when nothing is stripped")
	})

	t.Run("tool-only empty content with nonzero length preserved", func(t *testing.T) {
		// Tool-only message: empty display Content but a nonzero work
		// length set by the parser.
		m := db.Message{
			Role:          "assistant",
			Content:       "",
			ContentLength: 4096,
		}

		stats := sanitizeMessage(&m)

		assert.Equal(t, "", m.Content)
		assert.Equal(t, 0, stats.ControlCharsStripped, "nothing should be stripped")
		assert.Equal(t, 4096, m.ContentLength,
			"tool-only ContentLength must not be overwritten to len(Content)=0")
	})

	t.Run("reduced by exactly the removed-byte delta", func(t *testing.T) {
		raw := "before\x1b]0;title\x07after"
		sanitized := "before]0;titleafter"
		removed := len(raw) - len(sanitized)
		require.Greater(t, removed, 0, "this case requires bytes to be stripped")
		// Semantic length intentionally larger than len(Content).
		semantic := len(raw) + 100
		m := db.Message{
			Role:          "assistant",
			Content:       raw,
			ContentLength: semantic,
		}

		stats := sanitizeMessage(&m)

		assert.Equal(t, sanitized, m.Content)
		assert.Equal(t, 1, stats.ControlCharsStripped)
		assert.Equal(t, semantic-removed, m.ContentLength,
			"ContentLength must drop by exactly the removed-byte count, not be overwritten to len(Content)")
		assert.NotEqual(t, len(m.Content), m.ContentLength,
			"delta adjustment must not collapse the semantic length to len(Content)")
	})

	t.Run("thinking-inclusive length reduced by thinking delta", func(t *testing.T) {
		content := "visible answer"
		rawThinking := "think\x1b]0;title\x07more"
		sanitizedThinking := "think]0;titlemore"
		removed := len(rawThinking) - len(sanitizedThinking)
		require.Greater(t, removed, 0, "this case requires thinking bytes to be stripped")
		m := db.Message{
			Role:          "assistant",
			Content:       content,
			ThinkingText:  rawThinking,
			ContentLength: len(content) + len(rawThinking),
		}

		stats := sanitizeMessage(&m)

		assert.Equal(t, content, m.Content)
		assert.Equal(t, sanitizedThinking, m.ThinkingText)
		assert.Equal(t, 1, stats.ControlCharsStripped)
		assert.Equal(t, len(content)+len(sanitizedThinking), m.ContentLength,
			"ContentLength must drop by stripped thinking bytes when thinking contributes to the parser semantic length")
		assert.Equal(t, len(content)+len(rawThinking)-removed, m.ContentLength,
			"length adjustment must use the same removed-byte delta as content stripping")
	})
}

func TestSanitizeMessageStripsNULFromResultContent(t *testing.T) {
	resultRaw := "tool\x00result"
	resultClean := "toolresult"
	eventRaw := "event\x00content"
	eventClean := "eventcontent"
	m := db.Message{
		Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolUseID:           "tu1",
			ToolName:            "Bash",
			Category:            "Bash",
			ResultContent:       resultRaw,
			ResultContentLength: len(resultRaw),
			ResultEvents: []db.ToolResultEvent{{
				Source:        "wait_output",
				Status:        "completed",
				Content:       eventRaw,
				ContentLength: len(eventRaw),
			}},
		}},
	}

	stats := sanitizeMessage(&m)

	require.Len(t, m.ToolCalls, 1)
	tc := m.ToolCalls[0]
	assert.Equal(t, resultClean, tc.ResultContent)
	assert.Equal(t, len(resultClean), tc.ResultContentLength)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, eventClean, tc.ResultEvents[0].Content)
	assert.Equal(t, len(eventClean), tc.ResultEvents[0].ContentLength)
	assert.Equal(t, 2, stats.ControlCharsStripped)

	second := sanitizeMessage(&m)
	assert.Equal(t, validationStats{}, second, "second pass must be a no-op")
}

func TestSanitizeUsageEvent(t *testing.T) {
	ev := db.UsageEvent{
		Source:                   "api\x1bcall",
		Model:                    strings.Repeat("m", 200),
		InputTokens:              5_000_000,
		OutputTokens:             10,
		CacheCreationInputTokens: -1,
		CacheReadInputTokens:     0,
		ReasoningTokens:          maxPlausibleTokens + 1,
		OccurredAt:               "1700-01-01T00:00:00Z",
		CostStatus:               "ok",
	}
	stats := sanitizeUsageEvent(&ev)

	assert.Equal(t, "apicall", ev.Source)
	assert.Equal(t, strings.Repeat("m", maxModelLen), ev.Model)
	assert.Equal(t, maxPlausibleTokens, ev.InputTokens)
	assert.Equal(t, 10, ev.OutputTokens)
	assert.Equal(t, 0, ev.CacheCreationInputTokens)
	assert.Equal(t, maxPlausibleTokens, ev.ReasoningTokens)
	assert.Equal(t, "", ev.OccurredAt)
	assert.Equal(t, "ok", ev.CostStatus)

	assert.Equal(t, 1, stats.ControlCharsStripped)
	assert.Equal(t, 1, stats.ModelClamped)
	// InputTokens, CacheCreationInputTokens (negative), ReasoningTokens.
	assert.Equal(t, 3, stats.TokensClamped)
	assert.Equal(t, 1, stats.TimestampsBlanked)
}

func TestSanitizeUsageEventStripsDirtyModelPrefixBeforeClamp(t *testing.T) {
	ev := db.UsageEvent{
		Source: "api",
		Model:  strings.Repeat("\x00", maxModelLen+16) + "claude-opus-4",
	}
	stats := sanitizeUsageEvent(&ev)

	assert.Equal(t, "claude-opus-4", ev.Model)
	assert.Equal(t, 1, stats.ControlCharsStripped)
	assert.Zero(t, stats.ModelClamped)
}

func TestSanitizeSession(t *testing.T) {
	first := "hi\x07there"
	name := "clean name"
	farPast := "1500-01-01T00:00:00Z"
	good := "2026-06-20T10:00:00Z"
	s := db.Session{
		Project:      "proj\x1bx",
		Machine:      "host",
		Cwd:          "/home/u/dev",
		FirstMessage: &first,
		SessionName:  &name,
		StartedAt:    &farPast,
		EndedAt:      &good,
	}
	stats := sanitizeSession(&s)

	assert.Equal(t, "projx", s.Project)
	assert.Equal(t, "host", s.Machine)
	require.NotNil(t, s.FirstMessage)
	assert.Equal(t, "hithere", *s.FirstMessage)
	require.NotNil(t, s.SessionName)
	assert.Equal(t, "clean name", *s.SessionName)
	assert.Nil(t, s.StartedAt, "absurd started_at blanked to nil")
	require.NotNil(t, s.EndedAt)
	assert.Equal(t, good, *s.EndedAt)

	assert.Equal(t, 2, stats.ControlCharsStripped) // project + first message
	assert.Equal(t, 1, stats.TimestampsBlanked)
}

func TestValidateAndSanitizeAggregatesStats(t *testing.T) {
	s := db.Session{Project: "p\x1bq"}
	msgs := []db.Message{
		{Role: "bogus", Content: "c\x07"},
		{Role: "assistant", Model: strings.Repeat("z", 500)},
	}
	events := []db.UsageEvent{
		{Source: "s", InputTokens: -3},
	}

	stats := validateAndSanitize(&s, msgs, events)

	// project + first message content control strips.
	assert.Equal(t, 2, stats.ControlCharsStripped)
	assert.Equal(t, 1, stats.RoleCoerced)
	assert.Equal(t, 1, stats.ModelClamped)
	assert.Equal(t, 1, stats.TokensClamped)

	assert.Equal(t, "", msgs[0].Role)
	assert.Equal(t, "c", msgs[0].Content)
	assert.Equal(t, strings.Repeat("z", maxModelLen), msgs[1].Model)
	assert.Equal(t, 0, events[0].InputTokens)
}

func TestValidateAndSanitizeNilArgs(t *testing.T) {
	// All-nil call must not panic and must report no fixes.
	stats := validateAndSanitize(nil, nil, nil)
	assert.Equal(t, validationStats{}, stats)
}

// TestValidateAndSanitizeIdempotent verifies the critical invariant:
// running the pass over its own output yields no further fixes and an
// identical result. This is what keeps fingerprints stable across pushes.
func TestValidateAndSanitizeIdempotent(t *testing.T) {
	s := db.Session{
		Project:      "p\x1bq",
		Cwd:          "/x",
		FirstMessage: new("first\x07line"),
		StartedAt:    new("1500-01-01T00:00:00Z"),
		EndedAt:      new("2026-06-20T10:00:00Z"),
	}
	msgs := []db.Message{
		{
			Role:          "wizard",
			Content:       "esc\x1b]0;t\x07tail\nkeep\ttabs",
			ThinkingText:  "think\u0090ing", // U+0090 is a C1 control
			Model:         strings.Repeat("m", 5_000_000),
			ContextTokens: 9_000_000,
			OutputTokens:  -10,
			Timestamp:     "2999-01-01T00:00:00Z",
			SourceUUID:    "uuid\x00val",
			ToolCalls: []db.ToolCall{{
				ToolUseID:           "tu1",
				ToolName:            "Bash",
				Category:            "Bash",
				ResultContent:       "tool\x00result",
				ResultContentLength: len("tool\x00result"),
				ResultEvents: []db.ToolResultEvent{{
					Source:        "wait_output",
					Status:        "completed",
					Content:       "event\x00content",
					ContentLength: len("event\x00content"),
				}},
			}},
		},
		{
			Role:      "assistant",
			Content:   "clean",
			Timestamp: "2026-06-20T11:00:00Z",
		},
	}
	events := []db.UsageEvent{
		{
			Source:          "api\x1bx",
			Model:           strings.Repeat("z", 999),
			InputTokens:     5_000_000,
			OccurredAt:      "1700-01-01T00:00:00Z",
			ReasoningTokens: -1,
		},
	}

	// First pass.
	first := validateAndSanitize(&s, msgs, events)
	assert.Positive(t, first.ControlCharsStripped)
	assert.Positive(t, first.ModelClamped)
	assert.Positive(t, first.TokensClamped)
	assert.Positive(t, first.RoleCoerced)
	assert.Positive(t, first.TimestampsBlanked)

	// Snapshot the cleaned values.
	sAfter := s
	msgsAfter := append([]db.Message(nil), msgs...)
	eventsAfter := append([]db.UsageEvent(nil), events...)

	// Second pass over the already-clean output: no further fixes.
	second := validateAndSanitize(&s, msgs, events)
	assert.Equal(t, validationStats{}, second, "second pass must be a no-op")

	// Values must be byte-identical after the second pass.
	assert.Equal(t, sAfter, s)
	assert.Equal(t, msgsAfter, msgs)
	assert.Equal(t, eventsAfter, events)
}

// TestSanitizeUTF8Idempotent guards the shared seam directly: the write
// path stores SanitizeUTF8(x), and the fingerprint/readback path applies
// SanitizeUTF8 again, so it must be a fixed point.
func TestSanitizeUTF8Idempotent(t *testing.T) {
	inputs := []string{
		"plain",
		"keep\n\t\rwhitespace",
		"esc\x1b]0;title\x07bell",
		"c1\u0085\u0090\u009f",
		"nul\x00byte",
		"bad\xe2utf8",
		"emoji \U0001F600 ok",
	}
	for _, in := range inputs {
		once := db.SanitizeUTF8(in)
		twice := db.SanitizeUTF8(once)
		assert.Equal(t, once, twice, "SanitizeUTF8 must be idempotent for %q", in)
		assert.True(t, utf8.ValidString(once), "result must be valid UTF-8 for %q", in)
	}
}

func TestClampModelRuneBoundary(t *testing.T) {
	// A multibyte rune straddling the cap must not be split, so the
	// result stays valid UTF-8 (and re-sanitizing is a no-op).
	m := strings.Repeat("a", maxModelLen-1) + "é" + "tail"
	changed := clampModel(&m)
	assert.True(t, changed)
	assert.True(t, utf8.ValidString(m), "clamped model must be valid UTF-8")
	assert.LessOrEqual(t, len(m), maxModelLen)
	assert.Equal(t, m, db.SanitizeUTF8(m), "clamped model must survive re-sanitization")
}
