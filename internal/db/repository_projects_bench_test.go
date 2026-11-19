package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

// Each iteration imports a new session; compare with fixed iteration counts.
func BenchmarkRepositoryProjectImport(b *testing.B) {
	for _, project := range []string{"sample", "pr123"} {
		b.Run(project, func(b *testing.B) {
			d := testDB(b)
			sessions := make([]Session, b.N)
			observations := make([]export.ProjectIdentityObservation, b.N)
			for i := range b.N {
				sessions[i], observations[i] = repositorySession(
					fmt.Sprintf("import-%d", i), project, "https://example.com/team/sample.git",
				)
			}
			ctx := b.Context()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				require.NoError(b, d.UpsertSessionWithProjectIdentity(ctx, sessions[i], observations[i], project))
			}
			b.StopTimer()
			got, err := d.GetSessionFull(ctx, sessions[b.N-1].ID)
			require.NoError(b, err)
			require.NotNil(b, got)
			require.Equal(b, "sample", got.Project)
		})
	}
}
