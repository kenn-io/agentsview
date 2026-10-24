package review

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

func sampleSnapshot(t *testing.T) Snapshot {
	t.Helper()
	cost := friction.USDFromMicros(1500000)
	total, err := friction.ParseUSD("4.2")
	require.NoError(t, err)
	ord := 3
	return Snapshot{
		Timezone: "Asia/Tokyo", RulesVersion: "friction-v1",
		Date: "2026-09-15", SessionsScanned: 3,
		Signals: []friction.Signal{
			{
				Kind: friction.KindCorrection, SubjectID: "s1", Detector: "correction.chat",
				Dims: friction.Dims{Agent: "claude", Machine: "laptop-01", Persona: "helper", Channel: "general"},
				Text: "no, don't do that", Ordinal: &ord, Seq: 0, SubjectKind: friction.SubjectSession,
				OccurredAt: time.Date(2026, 9, 15, 1, 2, 3, 400000000, time.UTC),
			},
			{
				Kind: friction.KindError, SubjectID: "s2", Detector: "error", ToolName: "Bash",
				Text: "exit 1", CallIndex: &ord, Seq: 1, Dims: friction.Dims{Seat: "seat-02"},
			},
			{
				Kind: friction.KindFrustration, SubjectID: "s2", SubjectKind: friction.SubjectSession,
				Detector: "frustration", Text: "this is still broken", Ordinal: &ord, Seq: 2,
			},
			{
				Kind: friction.KindInterruption, SubjectID: "s2", SubjectKind: friction.SubjectSession,
				Detector: "interruption", Ordinal: &ord, Seq: 3,
			},
			{
				Kind: friction.KindError, SubjectID: "ci-run-7:nightly_check", SubjectKind: friction.SubjectDiagnostic,
				Detector: "error", ToolName: "nightly_check", Text: "ci-run-7:nightly_check: failed", Seq: 0,
			},
		},
		P0Alerts: map[string][]string{"bash": {"s1", "s2", "s3"}},
		Spend: &SpendSummary{
			Total: &total, SessionsWithStats: 3, SessionsWithCost: 2,
			InputTokens: 12345, OutputTokens: 678,
			RoleCosts:  map[string]friction.USD{"(root)": total},
			ModelCosts: map[string]friction.USD{"claude-opus-4-8": total},
		},
		ArchiveSpend: &db.FrictionArchiveSpendSummary{
			Week:     db.FrictionPeriodSpend{Total: cost, Days: 1, Agents: map[string]friction.USD{"codex": cost}, Models: map[string]friction.USD{}},
			WeekFrom: "2026-09-08", WeekTo: "2026-09-14", Timezone: "Asia/Tokyo",
		},
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	in := sampleSnapshot(t)
	encoded, err := EncodeSnapshot(in)
	require.NoError(t, err)
	out, err := DecodeSnapshot(encoded)
	require.NoError(t, err)
	assert.Equal(t, string(friction.RenderMarkdown(in.DigestSnapshot)),
		string(friction.RenderMarkdown(out.DigestSnapshot)),
		"a decoded snapshot re-renders byte-identically")
	require.Len(t, out.Signals, 5)
	assert.Equal(t, in.Signals[0].OccurredAt, out.Signals[0].OccurredAt)
	assert.Equal(t, friction.KindInterruption, out.Signals[3].Kind)
	assert.Equal(t, friction.SubjectDiagnostic, out.Signals[4].SubjectKind)
	assert.Equal(t, "4.2", out.Spend.Total.String(), "scale preserved")
	assert.True(t, out.Signals[1].OccurredAt.IsZero())

	_, err = DecodeSnapshot([]byte("{"))
	require.Error(t, err)
}
