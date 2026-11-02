package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildForkyClaudeFile writes a Claude JSONL DAG: a main line of `turns`
// user/assistant pairs and `forks` large-gap forks (4 user turns each) that
// branch off the main line at evenly spaced points. kids[0] at each fork point
// is the long main continuation, exercising overlapping subtree turn counts.
func buildForkyClaudeFile(tb testing.TB, turns, forks int) string {
	tb.Helper()
	var sb strings.Builder
	ts := func(i int) string {
		return fmt.Sprintf("2024-01-01T%02d:%02d:%02dZ", (i/3600)%24, (i/60)%60, i%60)
	}
	user := func(uuid, parent, text string, i int) {
		p := "null"
		if parent != "" {
			p = `"` + parent + `"`
		}
		fmt.Fprintf(&sb, `{"type":"user","timestamp":"%s","sessionId":"s","uuid":"%s","parentUuid":%s,"message":{"role":"user","content":"%s"}}`+"\n", ts(i), uuid, p, text)
	}
	asst := func(uuid, parent, text string, i int) {
		fmt.Fprintf(&sb, `{"type":"assistant","timestamp":"%s","sessionId":"s","uuid":"%s","parentUuid":"%s","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}`+"\n", ts(i), uuid, parent, text)
	}
	prev := ""
	forkAt := map[int]bool{}
	if forks > 0 {
		step := max(turns/(forks+1), 1)
		for k := 1; k <= forks; k++ {
			forkAt[k*step] = true
		}
	}
	var forkParents []string
	for t := range turns {
		u := fmt.Sprintf("u%d", t)
		a := fmt.Sprintf("a%d", t)
		user(u, prev, fmt.Sprintf("question number %d about the code", t), t*2)
		asst(a, u, fmt.Sprintf("answer number %d with some explanation", t), t*2+1)
		prev = a
		if forkAt[t] {
			forkParents = append(forkParents, a)
		}
	}
	// Fork branches written after the main line, so main continuation is kids[0].
	for k, fp := range forkParents {
		parent := fp
		for j := range 4 {
			u := fmt.Sprintf("f%du%d", k, j)
			a := fmt.Sprintf("f%da%d", k, j)
			user(u, parent, fmt.Sprintf("fork %d question %d", k, j), turns*2+j*2)
			asst(a, u, fmt.Sprintf("fork %d answer %d", k, j), turns*2+j*2+1)
			parent = a
		}
	}
	path := filepath.Join(tb.TempDir(), "forky.jsonl")
	require.NoError(tb, os.WriteFile(path, []byte(sb.String()), 0o600))
	return path
}

func BenchmarkPerfhuntClaudeForks(b *testing.B) {
	const turns = 10000 // 20k entries
	for _, forks := range []int{0, 10, 100, 1000} {
		b.Run(fmt.Sprintf("turns=%d/forks=%d", turns, forks), func(b *testing.B) {
			path := buildForkyClaudeFile(b, turns, forks)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				res, err := parseClaudeSession(path, "proj", "local")
				require.NoError(b, err)
				require.Len(b, res, forks+1)
			}
		})
	}
}

func BenchmarkPerfhuntClaudeForksFixedForks(b *testing.B) {
	const forks = 100
	for _, turns := range []int{1000, 10000, 40000} {
		b.Run(fmt.Sprintf("forks=%d/turns=%d", forks, turns), func(b *testing.B) {
			path := buildForkyClaudeFile(b, turns, forks)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				res, err := parseClaudeSession(path, "proj", "local")
				require.NoError(b, err)
				require.Len(b, res, forks+1)
			}
		})
	}
}

// Forks grow in proportion to session length (1 rewind per 100 turns).
func BenchmarkPerfhuntClaudeForksProportional(b *testing.B) {
	for _, turns := range []int{1000, 10000, 40000} {
		forks := turns / 100
		b.Run(fmt.Sprintf("turns=%d/forks=%d", turns, forks), func(b *testing.B) {
			path := buildForkyClaudeFile(b, turns, forks)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				res, err := parseClaudeSession(path, "proj", "local")
				require.NoError(b, err)
				require.Len(b, res, forks+1)
			}
		})
	}
}
