package remotesync_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func BenchmarkSelectAllowedTargetsCuratedFiles(b *testing.B) {
	for _, count := range []int{1_000, 10_000, 20_000} {
		b.Run(fmt.Sprintf("files=%d", count), func(b *testing.B) {
			files := make([]string, count)
			for i := range files {
				files[i] = fmt.Sprintf("/srv/sessions/session-%06d.jsonl", i)
			}
			allowed := remotesync.TargetSet{
				Files: map[parser.AgentType][]string{parser.AgentCursor: files},
			}
			var selected remotesync.TargetSet
			var ok bool
			b.ReportAllocs()
			for b.Loop() {
				selected, ok = remotesync.SelectAllowedTargets(allowed, allowed)
			}
			require.True(b, ok)
			require.Len(b, selected.Files[parser.AgentCursor], count)
		})
	}
}

func BenchmarkSelectAllowedFilesCuratedFiles(b *testing.B) {
	for _, count := range []int{1_000, 10_000, 20_000} {
		files := make([]string, count)
		for i := range files {
			files[i] = fmt.Sprintf("/srv/sessions/session-%06d.jsonl", i)
		}
		allowed := remotesync.TargetSet{
			Files: map[parser.AgentType][]string{parser.AgentCursor: files},
		}
		for _, request := range []struct {
			name  string
			files []string
		}{
			{name: "all", files: files},
			{name: "single", files: files[len(files)-1:]},
		} {
			b.Run(fmt.Sprintf("files=%d/%s", count, request.name), func(b *testing.B) {
				var selected []string
				var ok bool
				b.ReportAllocs()
				for b.Loop() {
					selected, ok = remotesync.SelectAllowedFiles(allowed, request.files)
				}
				require.True(b, ok)
				require.Len(b, selected, len(request.files))
			})
		}
	}
}
