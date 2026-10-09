package friction

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateGolden rewrites testdata/golden instead of comparing:
//
//	go test -tags fts5 ./internal/friction -run TestFrictionDigestGolden -update
var updateGolden = flag.Bool(
	"update", false,
	"rewrite golden files under testdata/ instead of comparing",
)

func goldenDigestFixture() DigestSnapshot {
	sigs := []Signal{
		{Kind: KindCorrection, SubjectID: "s-1", Text: "no, use the other branch"},
		{Kind: KindCorrection, SubjectID: "s-2", Text: "don't change the API", Dims: Dims{Seat: "seat-02", Agent: "claude", Machine: "host-a.example"}},
		{Kind: KindError, SubjectID: "s-3", ToolName: "Read", Text: "fatal: denied\ncheck file permissions"},
		{Kind: KindError, SubjectID: "s-1", ToolName: "Bash", Text: "command failed"},
		{Kind: KindError, SubjectID: "s-2", ToolName: "Read", Text: "file unavailable"},
		{Kind: KindError, SubjectID: "s-3", ToolName: "Bash", Text: "command failed"},
		{Kind: KindError, SubjectID: "s-1", ToolName: "Read", Text: "file unavailable"},
		{Kind: KindError, SubjectID: "s-2", ToolName: "Bash", Text: "command failed"},
		{Kind: KindWorkaround, SubjectID: "s-1", Label: "for now", Text: "use the fallback for now\nthen retry"},
		{Kind: KindDeferral, SubjectID: "s-2", Label: "next session"},
		{Kind: KindPattern, SubjectID: "s-1", Label: "retry_loop", Evidence: "Bash x3 identical arguments 09:00-09:02"},
		{Kind: KindPattern, SubjectID: "s-3", Label: "runaway_loop", Evidence: "12 tool calls 09:00-09:11"},
		{Kind: KindFrustration, SubjectID: "s-1", Text: "why won't it\nload???"},
		{Kind: KindInterruption, SubjectID: "s-2", Dims: Dims{Agent: "claude"}},
		{Kind: KindInterruption, SubjectID: "s-1"},
		{Kind: KindInterruption, SubjectID: "s-2", Dims: Dims{Agent: "later"}},
	}
	return DigestSnapshot{
		Date: "2026-09-16", Signals: sigs,
		P0Alerts: map[string][]string{
			"Bash": {"s-1", "s-2", "s-3"},
			"Read": {"s-1", "s-2", "s-3"},
		},
		SessionsScanned: 3,
	}
}

func buildGoldenDocuments() map[string][]byte {
	snap := goldenDigestFixture()
	return map[string][]byte{
		"friction-log.md":       RenderMarkdown(snap),
		"friction-log-empty.md": RenderMarkdown(DigestSnapshot{Date: "2026-09-17"}),
		"summary.json":          RenderSummaryJSON(snap),
	}
}

// TestFrictionDigestGolden pins both formats and every empty section.
func TestFrictionDigestGolden(t *testing.T) {
	got := buildGoldenDocuments()
	repeated := buildGoldenDocuments()
	require.Equal(t, got, repeated, "independent renders must be byte-identical")

	base := filepath.Join("testdata", "golden")
	if *updateGolden {
		require.NoError(t, os.MkdirAll(base, 0o755))
		for name, contents := range got {
			require.NoError(t, os.WriteFile(filepath.Join(base, name), contents, 0o644))
		}
		t.Logf("rewrote friction goldens under %s", base)
		return
	}
	for name, contents := range got {
		want, err := os.ReadFile(filepath.Join(base, name))
		require.NoError(t, err, "read %s (run with -update to generate)", name)
		assert.Equal(t, string(want), string(contents), name)
	}
}
