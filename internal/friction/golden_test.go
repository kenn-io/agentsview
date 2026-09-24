package friction

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// goldenDigestFixture ports jilog tests/golden_digest.rs render_fixture,
// scrubbed per spec §9.3 (see docs/internal/jilog-adaptation.md).
func goldenDigestFixture(t *testing.T) (DigestSnapshot, RenderLinks) {
	t.Helper()
	d := func(s string) USD { return mustUSD(t, s) }
	c1 := Signal{Kind: KindCorrection, SubjectID: "842c45ce-77b2-4d72-b995-f2a10466eb40", Text: "do calendar re-auth"}
	c2 := Signal{
		Kind: KindCorrection, SubjectID: "chat-1", Text: "no, use the gh cli",
		Dims: Dims{Seat: "seat-02", Persona: "helper", Channel: "general"},
	}
	e1 := Signal{
		Kind: KindError, SubjectID: "0e91a2b4-7d3f-4e2a-9c1b-44a7f3d8a1e2", ToolName: "bash",
		Text: `{"error":null,"output":{"returncode":101,"stderr":"error[E0308]: mismatched types","stdout":""},"success":false}`,
	}
	w1 := Signal{
		Kind: KindWorkaround, SubjectID: "0000000000000000-79f0e43ee2304cdb_self", Label: "TODO",
		Text: "All data collected. Let me update the todo list and finalize",
	}
	d1 := Signal{Kind: KindDeferral, SubjectID: "ae5a0552-47f6-4030-af78-09fe71542d3d", Label: "next session"}
	p1 := Signal{
		Kind: KindPattern, SubjectID: "ee58d934-1049-4da0-b5b3-9a00f50efcc7", Label: "stuck_loop",
		Text: "stuck loop", Evidence: "`bash` x6 identical arguments 01:35-01:54",
		Dims: Dims{Seat: "seat-03"},
	}
	snap := DigestSnapshot{
		Date:     "2026-09-16",
		Signals:  []Signal{c1, c2, e1, w1, d1, p1},
		P0Alerts: map[string][]string{"bash": {"s1", "s2", "s3"}},
		Personas: map[PersonaKey]*PersonaCounts{
			{"helper", "general"}: {
				Sessions: 2, Corrections: 1,
				InputTokens: 5000, OutputTokens: 250, CostUSD: new(d("0.5")),
			},
		},
		Spend: &SpendSummary{
			Total: new(d("4.2")), SessionsWithStats: 3, SessionsWithCost: 2,
			InputTokens: 1000, OutputTokens: 50,
			RoleCosts:  map[string]USD{"(root)": d("1.2"), "explore": d("3")},
			ModelCosts: map[string]USD{"claude-opus-5": d("4.2")},
		},
		RecurrenceCosts: map[string]USD{c1.Fingerprint(): d("4.2")},
	}
	links := RenderLinks{IssueIndex: map[string]IssueRef{
		w1.Fingerprint(): {ID: "#7", Backend: "kata", Title: w1.Title()},
	}}
	return snap, links
}

// extraKindsFixture is the D36 golden (spec §9.3): non-empty Frustration
// and Interruptions sections, one annotated frustration line, a persona
// whose line does not count the new kinds, and interruptions grouped per
// session in run order.
func extraKindsFixture(t *testing.T) (DigestSnapshot, RenderLinks) {
	t.Helper()
	c := Signal{Kind: KindCorrection, SubjectID: "s-1", Text: "no, use the other branch"}
	f1 := Signal{Kind: KindFrustration, SubjectID: "s-1", Text: "this is broken again"}
	f2 := Signal{
		Kind: KindFrustration, SubjectID: "chat-2", Text: "why won't it load???",
		Dims: Dims{Seat: "seat-02", Persona: "helper", Channel: "general"},
	}
	i1 := Signal{Kind: KindInterruption, SubjectID: "s-1", Dims: Dims{Agent: "claude"}}
	i2 := Signal{Kind: KindInterruption, SubjectID: "s-2"}
	i3 := Signal{Kind: KindInterruption, SubjectID: "s-1", Dims: Dims{Agent: "claude"}}
	snap := DigestSnapshot{
		Date:            "2026-09-17",
		Signals:         []Signal{c, f1, f2, i1, i2, i3},
		Personas:        map[PersonaKey]*PersonaCounts{{"helper", "general"}: {Sessions: 1}},
		RecurrenceCosts: map[string]USD{f1.Fingerprint(): mustUSD(t, "1.5")},
	}
	links := RenderLinks{IssueIndex: map[string]IssueRef{
		f1.Fingerprint(): {ID: "#11", Backend: "kata", Title: f1.Title()},
	}}
	return snap, links
}

func buildGoldenDocuments(t *testing.T) map[string][]byte {
	t.Helper()
	snap, links := goldenDigestFixture(t)
	extra, extraLinks := extraKindsFixture(t)
	jsonSnap, meta := digestReportFixture(t)
	return map[string][]byte{
		"friction-log.md":             RenderMarkdown(snap, links),
		"friction-log-extra-kinds.md": RenderMarkdown(extra, extraLinks),
		"summary.json":                RenderSummaryJSON(jsonSnap, meta),
	}
}

func goldenManifest(docs map[string][]byte) []byte {
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)
	var b bytes.Buffer
	for _, name := range names {
		sum := sha256.Sum256(docs[name])
		_, _ = fmt.Fprintf(&b, "%x  %s\n", sum, name)
	}
	return b.Bytes()
}

// TestFrictionDigestGolden ports jilog digest_bytes_match_golden and
// review_json_bytes_match_golden, in the TestExportReportingGolden shape
// (cmd/agentsview/export_reporting_test.go:453-482).
func TestFrictionDigestGolden(t *testing.T) {
	got := buildGoldenDocuments(t)
	repeated := buildGoldenDocuments(t)
	require.Equal(t, got, repeated, "independent renders must be byte-identical")

	base := filepath.Join("testdata", "golden")
	manifest := goldenManifest(got)
	if *updateGolden {
		require.NoError(t, os.MkdirAll(base, 0o755))
		for name, contents := range got {
			require.NoError(t, os.WriteFile(filepath.Join(base, name), contents, 0o644))
		}
		require.NoError(t, os.WriteFile(filepath.Join(base, "manifest.sha256"), manifest, 0o644))
		t.Logf("rewrote friction goldens under %s", base)
		return
	}
	for name, contents := range got {
		want, err := os.ReadFile(filepath.Join(base, name))
		require.NoError(t, err, "read %s (run with -update to generate)", name)
		assert.Equal(t, string(want), string(contents), name)
	}
	wantManifest, err := os.ReadFile(filepath.Join(base, "manifest.sha256"))
	require.NoError(t, err, "read golden manifest")
	assert.Equal(t, string(wantManifest), string(manifest))
}
