package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawparity"
	"hegel.dev/go/hegel"
)

// A waiver must never mask unfinished work or uncertain identity. The oracle
// enumerates independent blockers rather than calling Report.Passes.
func TestRawActivationGateModel(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		names := []string{"differs", "ambiguous", "unmatchable", "equal", "raw_only", "legacy_only"}
		counts := rawparity.Counts{}
		for _, name := range names {
			if hegel.Draw(ht, hegel.Booleans()) {
				counts[name] = hegel.Draw(ht, hegel.Integers(1, 100))
			}
		}
		ready := rawparity.Readiness{}
		for _, n := range []*int{&ready.Projected, &ready.Pending, &ready.Failed, &ready.Unsupported} {
			if hegel.Draw(ht, hegel.Booleans()) {
				*n = hegel.Draw(ht, hegel.Integers(1, 100))
			}
		}
		accept := hegel.Draw(ht, hegel.Booleans())
		mode := []RawEmbeddingMode{RawEmbeddingsUnknown, RawEmbeddingsDisabled, RawEmbeddingsConfigured}[hegel.Draw(ht, hegel.Integers(0, 2))]
		report := rawparity.Report{Counts: counts, Readiness: []rawparity.Readiness{ready}, Pass: hegel.Draw(ht, hegel.Booleans())}
		before := map[string]int{}
		for k, v := range counts {
			before[k] = v
		}
		want := mode == RawEmbeddingsDisabled && ready.Projected > 0 && ready.Pending == 0 && ready.Failed == 0 && ready.Unsupported == 0 && counts["ambiguous"] == 0 && counts["unmatchable"] == 0 && (accept || counts["differs"] == 0)
		err := checkRawActivationGate(report, accept, mode)
		require.Equal(ht, want, err == nil)
		require.EqualValues(ht, before, report.Counts, "audit counts must never be rewritten by a waiver")
	}, hegel.WithTestCases(2000))
}

func TestRawActivationGateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, blocker string
		accept        bool
		mode          RawEmbeddingMode
		projected     int
		want          string
	}{
		{"accepted content differences", "differs", true, RawEmbeddingsDisabled, 1, ""},
		{"content waiver required", "differs", false, RawEmbeddingsDisabled, 1, "parity"},
		{"pending", "pending", true, RawEmbeddingsDisabled, 1, "parity"},
		{"failed", "failed", true, RawEmbeddingsDisabled, 1, "parity"},
		{"unsupported", "unsupported", true, RawEmbeddingsDisabled, 1, "parity"},
		{"ambiguous", "ambiguous", true, RawEmbeddingsDisabled, 1, "parity"},
		{"unmatchable", "unmatchable", true, RawEmbeddingsDisabled, 1, "parity"},
		{"empty", "", true, RawEmbeddingsDisabled, 0, "parity"},
		{"unknown", "", true, RawEmbeddingsUnknown, 1, "unknown"},
		{"configured", "", true, RawEmbeddingsConfigured, 1, "coverage"},
		{"invalid mode", "", true, RawEmbeddingMode("invalid"), 1, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rawparity.Report{Counts: rawparity.Counts{}, Readiness: []rawparity.Readiness{{Projected: tc.projected}}}
			switch tc.blocker {
			case "pending":
				r.Readiness[0].Pending = 1
			case "failed":
				r.Readiness[0].Failed = 1
			case "unsupported":
				r.Readiness[0].Unsupported = 1
			default:
				r.Counts[tc.blocker] = 1
			}
			err := checkRawActivationGate(r, tc.accept, tc.mode)
			if tc.want == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.want)
			}
		})
	}
}
