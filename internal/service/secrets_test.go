package service_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestHTTPBackendScanSecretsStream(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			f, ok := w.(http.Flusher)
			if !ok {
				assert.Fail(t, "flusher unsupported")
				return
			}
			fmt.Fprint(w, "event: progress\ndata: {\"scanned\":1,\"total\":2}\n\n")
			f.Flush()
			fmt.Fprint(w, "event: summary\n"+
				"data: {\"scanned\":2,\"with_secrets\":1,\"total_findings\":3,"+
				"\"definite_findings\":2,\"candidate_findings\":1}\n\n")
			f.Flush()
		}))
	defer ts.Close()
	svc := servicehttp.NewHTTPBackend(ts.URL, "", false, "")
	var ticks []service.SecretScanProgress
	sum, err := svc.ScanSecrets(t.Context(),
		service.SecretScanInput{Backfill: true},
		func(p service.SecretScanProgress) { ticks = append(ticks, p) })
	require.NoError(t, err)
	require.NotNil(t, sum)
	assert.Equal(t, 2, sum.Scanned)
	assert.Equal(t, 1, sum.WithSecrets)
	assert.Equal(t, 3, sum.TotalFindings)
	assert.Equal(t, 2, sum.DefiniteFindings)
	assert.Equal(t, 1, sum.CandidateFindings)
	assert.NotEmpty(t, ticks)
}

// TestDirectListSecretsConfidenceDefault verifies the list defaults to
// definite-only. Candidates (e.g. high-entropy-assignment) are FP-prone
// investigative material and must be opted into explicitly via confidence
// "candidate" or "all". This mirrors the product meaning of has_secret and
// secret_leak_count, which count definite findings only.
func TestDirectListSecretsConfidenceDefault(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "x1", "proj", func(s *db.Session) {
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		dbtest.UserMsg("x1", 0, "key AKIA7QHWN2DKR4FYPLJM tok=abc123def456ghi789jkl"),
	}))
	require.NoError(t, d.ReplaceSessionSecretFindings(t.Context(), "x1", []db.SecretFinding{
		{
			SessionID: "x1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0,
			MatchStart: 4, MatchEnd: 24, MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
		},
		{
			SessionID: "x1", RuleName: "high-entropy-assignment", Confidence: "candidate",
			LocationKind: "message", MessageOrdinal: 0,
			MatchStart: 29, MatchEnd: 50, MatchIndex: 1, RedactedMatch: "…789jkl",
		},
	}, 1, secrets.RulesVersion()))
	be := service.NewDirectBackend(d, nil)

	cases := []struct {
		name       string
		confidence string
		want       int
		wantRule   string // checked only when want == 1
	}{
		{"default is definite-only", "", 1, "aws-access-key"},
		{"explicit definite", "definite", 1, "aws-access-key"},
		{"candidate opt-in", "candidate", 1, "high-entropy-assignment"},
		{"all shows both", "all", 2, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			page, err := be.ListSecrets(t.Context(),
				service.SecretListFilter{Confidence: tc.confidence, Limit: 50})
			require.NoError(t, err)
			require.Len(t, page.Findings, tc.want)
			if tc.want == 1 {
				assert.Equal(t, tc.wantRule, page.Findings[0].RuleName)
			}
		})
	}
}

// TestDirectListSecretsHidesStaleRulesVersions verifies that list output is
// tied to the current scanner. Stored findings from older rule/fixture-deny
// versions must not keep surfacing after the scanner has learned to suppress
// them; backfill will rewrite the stored rows, but listing should fail closed
// before that happens.
func TestDirectListSecretsHidesStaleRulesVersions(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	token := strings.Join([]string{
		"ghp_", "M7qL8r", "P2sT5u", "V9wX3y",
		"Z6aB1c", "D4eF7g", "H0iJ2k",
	}, "")
	content := "token=" + token
	start := strings.Index(content, token)
	dbtest.SeedSession(t, d, "x1", "proj", func(s *db.Session) {
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		dbtest.UserMsg("x1", 0, content),
	}))
	require.NoError(t, d.ReplaceSessionSecretFindings(t.Context(), "x1", []db.SecretFinding{{
		SessionID: "x1", RuleName: "github-pat", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0,
		MatchStart: start, MatchEnd: start + len(token),
		MatchIndex: 0, RedactedMatch: "ghp_…iJ2k",
	}}, 1, "old-rules"))
	be := service.NewDirectBackend(d, nil)

	page, err := be.ListSecrets(t.Context(),
		service.SecretListFilter{Limit: 50})
	require.NoError(t, err)
	require.Empty(t, page.Findings)
}

func TestDirectScanSecretsReadOnly(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil) // nil engine => read-only
	_, err := be.ScanSecrets(t.Context(),
		service.SecretScanInput{Backfill: true}, nil)
	if !errors.Is(err, db.ErrReadOnly) {
		require.FailNowf(t, "test failed", "ScanSecrets with nil engine = %v, want db.ErrReadOnly", err)
	}
}

// TestDirectListSecretsReveal exercises the reveal guarantee end-to-end: a
// finding whose coordinates still cover the secret is revealed in full, while
// a stale finding (coordinates no longer matching the rule) returns the
// "source changed" marker instead of a value. Redaction is the default.
func TestDirectListSecretsReveal(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	const secret = "AKIA7QHWN2DKR4FYPLJM"
	content := "my key is " + secret + " ok"
	start := strings.Index(content, secret)
	dbtest.SeedSession(t, d, "x1", "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		dbtest.UserMsg("x1", 0, content),
	}))
	require.NoError(t, d.ReplaceSessionSecretFindings(t.Context(), "x1", []db.SecretFinding{
		{
			SessionID: "x1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0,
			MatchStart: start, MatchEnd: start + len(secret),
			MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
		},
		// Stale: coordinates point at non-secret bytes, so Verify fails.
		{
			SessionID: "x1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0,
			MatchStart: 0, MatchEnd: 5,
			MatchIndex: 0, RedactedMatch: "my ke",
		},
	}, 1, secrets.RulesVersion()))
	be := service.NewDirectBackend(d, nil)

	// Default: never the full secret.
	def, err := be.ListSecrets(t.Context(),
		service.SecretListFilter{Limit: 50})
	require.NoError(t, err)
	require.Len(t, def.Findings, 2)
	for _, f := range def.Findings {
		assert.NotContains(t, f.RedactedMatch, secret,
			"default list leaked secret: %q", f.RedactedMatch)
	}

	// Reveal: the valid finding shows the full secret; the stale one is marked.
	rev, err := be.ListSecrets(t.Context(),
		service.SecretListFilter{Reveal: true, Limit: 50})
	require.NoError(t, err)
	require.Len(t, rev.Findings, 2)
	var revealed, marked int
	for _, f := range rev.Findings {
		switch {
		case f.RedactedMatch == secret:
			revealed++
		case strings.Contains(f.RedactedMatch, "source changed"):
			marked++
		}
	}
	assert.Equal(t, 1, revealed, "exactly one finding should reveal the full secret")
	assert.Equal(t, 1, marked, "the stale finding should return the marker")
}
