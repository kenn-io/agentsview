package signals

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"unicode"

	"github.com/stretchr/testify/assert"
)

func TestAnalyzeHeuristics_PromptQuality(t *testing.T) {
	tests := []struct {
		name string
		in   HeuristicInput
		want HeuristicSignals
	}{
		{
			name: "ignores short control prompts",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{Role: "user", Content: "yes"},
				{Role: "user", Content: "continue"},
			}},
			want: HeuristicSignals{},
		},
		{
			name: "counts only short task-start prompts",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{Role: "user", Content: "fix bug"},
				{Role: "user", Content: "add tests"},
			}},
			want: HeuristicSignals{
				ShortPromptCount:            1,
				UnstructuredStart:           true,
				MissingSuccessCriteriaCount: 1,
				NoCodeContextCount:          1,
			},
		},
		{
			name: "structured first prompt avoids start penalty",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Fix internal/signals/score.go\n\n" +
						"- Must preserve existing grades\n" +
						"- Run go test ./internal/signals\n" +
						"Expected result: tests pass",
				},
			}},
			want: HeuristicSignals{},
		},
		{
			name: "code task missing verification language",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Implement the backend scorer in the " +
						"codebase. Success means the score changes " +
						"only for repeated prompts.",
				},
			}},
			want: HeuristicSignals{
				MissingVerificationCount: 1,
				NoCodeContextCount:       1,
			},
		},
		{
			name: "non code conversation is not penalized",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "What are useful ways to think about " +
						"technical debt in a planning meeting?",
				},
			}},
			want: HeuristicSignals{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnalyzeHeuristics(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAnalyzeHeuristics_ShortStartsIgnoreRecentSteering(t *testing.T) {
	in := HeuristicInput{Messages: []HeuristicMessage{
		{
			Role:      "user",
			Content:   "Please fix the parser bug in internal/parser.go.",
			Timestamp: "2026-05-27T10:00:00Z",
		},
		{
			Role:      "assistant",
			Content:   "I changed the parser.",
			Timestamp: "2026-05-27T10:05:00Z",
		},
		{
			Role:      "user",
			Content:   "add tests",
			Timestamp: "2026-05-27T10:06:00Z",
		},
		{
			Role:      "assistant",
			Content:   "Done.",
			Timestamp: "2026-05-27T10:10:00Z",
		},
		{
			Role:      "user",
			Content:   "fix docs",
			Timestamp: "2026-05-27T11:00:01Z",
		},
	}}

	got := AnalyzeHeuristics(in)
	assert.Equal(t, 1, got.ShortPromptCount)
}

func TestAnalyzeHeuristics_RepeatedPrompts(t *testing.T) {
	in := HeuristicInput{Messages: []HeuristicMessage{
		{
			Role: "user",
			Content: "Please fix the failing tests in the backend " +
				"scorer and keep the changes small.",
		},
		{
			Role:    "assistant",
			Content: "I'll inspect the scorer.",
		},
		{
			Role: "user",
			Content: "Please fix failing backend scorer tests and " +
				"keep the changes small.",
		},
		{
			Role:    "user",
			Content: "yes",
		},
	}}

	got := AnalyzeHeuristics(in)
	assert.Equal(t, 1, got.DuplicatePromptCount)
}

func TestCountDuplicatePromptsAllocationGrowthStaysNearLinear(t *testing.T) {
	prompts := func(count int) []promptInfo {
		items := make([]promptInfo, count)
		for i := range items {
			tokens := []string{
				"shared-0", "shared-1", "shared-2", "shared-3",
				"shared-4", "shared-5", "shared-6", "shared-7",
			}
			for j := range 8 {
				tokens = append(tokens, fmt.Sprintf("prompt-%d-%d", i, j))
			}
			items[i] = promptInfo{
				Normalized: fmt.Sprintf(
					"shared alpha beta prompt number %d", i,
				),
				Tokens: tokens,
			}
		}
		return items
	}

	small := prompts(64)
	large := prompts(256)
	var got int
	smallAllocs := testing.AllocsPerRun(3, func() {
		got = countDuplicatePrompts(small)
	})
	assert.Zero(t, got, "fixture prompts must remain distinct")
	largeAllocs := testing.AllocsPerRun(3, func() {
		got = countDuplicatePrompts(large)
	})
	assert.Zero(t, got, "fixture prompts must remain distinct")

	assert.LessOrEqual(t, largeAllocs, 6*smallAllocs,
		"quadrupling prompts must not cause quadratic allocation growth")
}

func TestCountDuplicatePromptsMatchesPairwiseReference(t *testing.T) {
	vocabulary := []string{
		"alpha", "beta", "gamma", "delta", "epsilon", "zeta",
		"eta", "theta", "iota", "kappa", "lambda", "mu",
	}
	trial := 0
	config := &quick.Config{
		MaxCount: 100,
		Rand:     rand.New(rand.NewSource(1)),
		Values: func(args []reflect.Value, rng *rand.Rand) {
			count := 1 + rng.Intn(60)
			prompts := make([]promptInfo, 0, count)
			for i := range count {
				tokenCount := 4 + rng.Intn(12)
				tokens := make([]string, tokenCount)
				for j := range tokenCount {
					tokens[j] = vocabulary[rng.Intn(len(vocabulary))]
				}
				normalized := fmt.Sprintf(
					"synthetic prompt trial %d item %d", trial, i,
				)
				if i > 0 && rng.Intn(8) == 0 {
					normalized = prompts[rng.Intn(i)].Normalized
				}
				prompts = append(prompts, promptInfo{
					Normalized: normalized,
					Tokens:     tokens,
				})
			}
			args[0] = reflect.ValueOf(prompts)
			trial++
		},
	}
	assert.NoError(t, quick.Check(func(prompts []promptInfo) bool {
		return assert.Equal(t,
			countDuplicatePromptsPairwise(prompts),
			countDuplicatePrompts(prompts),
			"trial %d", trial-1,
		)
	}, config))
}

func countDuplicatePromptsPairwise(prompts []promptInfo) int {
	seen := make([]promptInfo, 0, len(prompts))
	repeats := 0
	for _, p := range prompts {
		if isControlPrompt(p.Normalized) ||
			len(p.Normalized) < 20 || len(p.Tokens) < 4 {
			continue
		}
		duplicate := false
		for _, previous := range seen {
			if p.Normalized == previous.Normalized ||
				pairwiseJaccard(p.Tokens, previous.Tokens) >= 0.85 {
				duplicate = true
				break
			}
		}
		if duplicate {
			repeats++
			continue
		}
		seen = append(seen, p)
	}
	return repeats
}

func pairwiseJaccard(current, previous []string) float64 {
	currentSet := make(map[string]struct{}, len(current))
	for _, token := range current {
		currentSet[token] = struct{}{}
	}
	intersections := 0
	union := len(currentSet)
	for _, token := range previous {
		if _, ok := currentSet[token]; ok {
			intersections++
		} else {
			union++
		}
	}
	return float64(intersections) / float64(union)
}

func TestAnalyzeHeuristics_CodeContext(t *testing.T) {
	tests := []struct {
		name string
		in   HeuristicInput
		want int
	}{
		{
			name: "code task without prompt or tool context",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Fix the backend test failure in the " +
						"codebase.",
				},
			}},
			want: 1,
		},
		{
			name: "file reference is context",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Fix the backend test failure in " +
						"internal/signals/score.go.",
				},
			}},
			want: 0,
		},
		{
			name: "grep tool activity is context",
			in: HeuristicInput{
				Messages: []HeuristicMessage{
					{
						Role: "user",
						Content: "Fix the backend test failure in " +
							"the codebase.",
					},
				},
				ToolRows: []ToolCallRow{
					{Category: "Grep", ToolName: "Grep"},
				},
			},
			want: 0,
		},
		{
			name: "glob tool activity is context",
			in: HeuristicInput{
				Messages: []HeuristicMessage{
					{
						Role: "user",
						Content: "Fix the backend test failure in " +
							"the codebase.",
					},
				},
				ToolRows: []ToolCallRow{
					{Category: "Glob", ToolName: "Glob"},
				},
			},
			want: 0,
		},
		{
			name: "test command is context",
			in: HeuristicInput{
				Messages: []HeuristicMessage{
					{
						Role: "user",
						Content: "Fix the backend test failure in " +
							"the codebase.",
					},
				},
				ToolRows: []ToolCallRow{
					{
						Category:  "Bash",
						ToolName:  "Bash",
						InputJSON: `{"command":"go test ./internal/signals"}`,
					},
				},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnalyzeHeuristics(tt.in)
			assert.Equal(t, tt.want, got.NoCodeContextCount)
		})
	}
}

func TestAnalyzeHeuristics_RunawayToolLoop(t *testing.T) {
	t.Run("repeated failing exact calls", func(t *testing.T) {
		calls := make([]ToolCallRow, 12)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category:      "Bash",
				ToolName:      "Bash",
				InputJSON:     `{"command":"npm test"}`,
				EventStatus:   "errored",
				ResultContent: "exit status 1\nFAIL",
			}
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 1, got.RunawayToolLoopCount)
	})

	t.Run("repeated successful harness calls are not runaway", func(t *testing.T) {
		calls := make([]ToolCallRow, 12)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category:      "Bash",
				ToolName:      "Bash",
				InputJSON:     `{"command":"npm test"}`,
				ResultContent: "PASS",
			}
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 0, got.RunawayToolLoopCount)
	})

	t.Run("ordinary varied calls", func(t *testing.T) {
		calls := []ToolCallRow{
			{Category: "Read", ToolName: "Read", InputJSON: `{"file_path":"a.go"}`},
			{Category: "Grep", ToolName: "Grep", InputJSON: `{"pattern":"x"}`},
			{Category: "Edit", ToolName: "Edit", InputJSON: `{"file_path":"a.go"}`},
			{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"go test ./..."}`},
			{Category: "Read", ToolName: "Read", InputJSON: `{"file_path":"b.go"}`},
			{Category: "Edit", ToolName: "Edit", InputJSON: `{"file_path":"b.go"}`},
			{Category: "Glob", ToolName: "Glob", InputJSON: `{"pattern":"*.go"}`},
			{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"go test ./internal/db"}`},
			{Category: "Read", ToolName: "Read", InputJSON: `{"file_path":"c.go"}`},
			{Category: "Edit", ToolName: "Edit", InputJSON: `{"file_path":"c.go"}`},
			{Category: "Grep", ToolName: "Grep", InputJSON: `{"pattern":"z"}`},
			{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"go test ./internal/signals"}`},
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 0, got.RunawayToolLoopCount)
	})

	t.Run("six failures in any twelve-call window", func(t *testing.T) {
		calls := make([]ToolCallRow, 13)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category: "Bash",
				ToolName: "Bash",
				InputJSON: fmt.Sprintf(
					`{"command":"npm run step-%c"}`,
					rune('a'+i),
				),
			}
		}
		for _, i := range []int{1, 3, 5, 7, 9, 11} {
			calls[i].EventStatus = "errored"
			calls[i].ResultContent = "exit status 1\nFAIL"
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 1, got.RunawayToolLoopCount)
	})

	t.Run("dominant command class requires three failures", func(t *testing.T) {
		calls := make([]ToolCallRow, 12)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category: "Bash",
				ToolName: "Bash",
				InputJSON: fmt.Sprintf(
					`{"command":"npm run step-%c"}`,
					rune('a'+i),
				),
			}
		}
		for _, i := range []int{2, 5} {
			calls[i].EventStatus = "errored"
			calls[i].ResultContent = "exit status 1\nFAIL"
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 0, got.RunawayToolLoopCount)

		calls[9].EventStatus = "errored"
		calls[9].ResultContent = "exit status 1\nFAIL"
		got = AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 1, got.RunawayToolLoopCount)
	})

	t.Run("failing calls without retained input are not an exact run", func(t *testing.T) {
		// Five failing Bash calls whose inputs the archive dropped, then
		// varied work. Only a shared name links the five calls.
		calls := make([]ToolCallRow, 0, 12)
		for i := range 5 {
			calls = append(calls, ToolCallRow{
				Category: "Bash", ToolName: "Bash",
				MessageOrdinal: i, EventStatus: "errored",
			})
		}
		for i, name := range []string{"Read", "Grep", "Edit", "Glob", "Read", "Grep", "Edit"} {
			calls = append(calls, ToolCallRow{
				Category: name, ToolName: name,
				MessageOrdinal: 5 + i, EventStatus: "completed",
			})
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 0, got.RunawayToolLoopCount)
	})

	t.Run("shell calls without retained input share no command class", func(t *testing.T) {
		calls := make([]ToolCallRow, 12)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category: "Bash", ToolName: "Bash",
				MessageOrdinal: i, EventStatus: "completed",
			}
		}
		for _, i := range []int{2, 5, 9} {
			calls[i].EventStatus = "errored"
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 0, got.RunawayToolLoopCount,
			"three failures need a provably dominant command")

		for _, i := range []int{0, 7, 11} {
			calls[i].EventStatus = "errored"
		}
		got = AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 1, got.RunawayToolLoopCount,
			"six failures in a window stay a runaway loop")
	})
}

func TestNormalizePromptRemovesCodeFences(t *testing.T) {
	got := normalizePrompt("Fix this:\n```go\nfunc main() {}\n```\nPlease")
	want := "fix this: please"
	assert.Equal(t, want, got)
}

func TestCountFrustrationMarkers(t *testing.T) {
	msgs := []HeuristicMessage{
		{Role: "user", Content: "WHY WONT THIS WORK???!!!"},
		{Role: "user", Content: "this is broken after the retry"},
		{Role: "assistant", Content: "I will inspect it."},
		{Role: "user", Content: "Please run the focused test again."},
		{
			Role:    "user",
			Content: "```text\nFUCK\n```\nPlease handle the log.",
		},
	}

	assert.Equal(t, 2, CountFrustrationMarkers(msgs))
}

// Starting the regular expression where frustrationPhraseStart says must
// find a match exactly when reading the whole prompt does, including
// prompts whose letters only match under Unicode case folding and literals
// whose word boundary depends on the rune before them.
func TestFrustrationPhraseStartKeepsEveryRegexMatch(t *testing.T) {
	prompts := []string{
		"WHY WON'T THIS WORK???!!!",
		"this is broken after the retry",
		"Come On, it still fails",
		"doe\u017f not work",       // long s folds to s
		"you bro\u212ae the build", // Kelvin sign folds to k
		"wtf\u017f now",            // long s after a literal is no word character
		"the SAME ERROR shows up",
		"doesn\u2019t work", // curly apostrophe: regex requires ASCII
		"Please run the focused test again.",
		"awtf then wtf",
		"wtfs and nothing else",
		"\u00e9wtf is it",
		"\u4e2dwtf is it",
		strings.Repeat("a", 5000) + " wtf " + strings.Repeat("b", 5000),
	}
	matches := 0
	for _, prompt := range prompts {
		normalized := []byte(normalizePrompt(prompt))
		want := frustrationPhraseRe.Match(normalized)
		start := frustrationPhraseStart(normalized)
		assert.Equal(t, want, start >= 0 && frustrationPhraseRe.Match(normalized[start:]), "%q", prompt)
		if want {
			matches++
		}
	}
	assert.Equal(t, 11, matches)
	assert.Equal(t, -1, frustrationPhraseStart([]byte(normalizePrompt("Please run the focused test again."))))
}

// After lowercasing, long s is the only non-ASCII rune whose simple fold
// reaches an ASCII letter; mayContainFrustrationPhrase depends on that.
func TestLongSIsTheOnlyLowercaseRuneFoldingToASCII(t *testing.T) {
	var found []rune
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		lower := unicode.ToLower(r)
		if lower < 0x80 {
			continue
		}
		for f := unicode.SimpleFold(lower); f != lower; f = unicode.SimpleFold(f) {
			if f < 0x80 {
				found = append(found, lower)
				break
			}
		}
	}
	slices.Sort(found)
	assert.Equal(t, []rune{'\u017f'}, slices.Compact(found))
}

func TestStripCodeFencesMatchesRegexp(t *testing.T) {
	inputs := []string{
		"",
		"no fences",
		"```go\nfunc main() {}\n```",
		"a ```one``` b ```two``` c",
		"open ``` never closed",
		"```a``````b```",
		"``` ```",
		"x``````y",
		"tail ```\n",
		strings.Repeat("`", 7),
	}
	for _, input := range inputs {
		assert.Equal(t, codeFenceRe.ReplaceAllString(input, " "), stripCodeFences(input), "%q", input)
	}
}
