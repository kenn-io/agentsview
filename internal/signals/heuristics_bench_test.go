package signals

import (
	"fmt"
	"testing"
)

// BenchmarkCountDuplicatePromptsLargeSession measures the quality-signal
// workload that previously rescanned every prior prompt's complete token list.
// The fixture keeps a small shared vocabulary while making every prompt
// distinct, so the index must reject many plausible candidates without
// collapsing the workload into exact-duplicate lookups.
func BenchmarkCountDuplicatePromptsLargeSession(b *testing.B) {
	const (
		promptCount      = 800
		sharedTokenCount = 12
		uniqueTokenCount = 84
	)
	prompts := make([]promptInfo, promptCount)
	for i := range prompts {
		tokens := make([]string, 0, sharedTokenCount+uniqueTokenCount)
		for j := range sharedTokenCount {
			tokens = append(tokens, fmt.Sprintf("shared-%d", j))
		}
		for j := range uniqueTokenCount {
			tokens = append(tokens, fmt.Sprintf("prompt-%d-token-%d", i, j))
		}
		prompts[i] = promptInfo{
			Normalized: fmt.Sprintf(
				"benchmark prompt %d with distinct session context", i,
			),
			Tokens: tokens,
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if repeats := countDuplicatePrompts(prompts); repeats != 0 {
			b.Fatalf("fixture produced %d duplicate prompts", repeats)
		}
	}
}
