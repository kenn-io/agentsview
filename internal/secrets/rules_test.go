package secrets

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAWSAccessKeyID builds a scanner fixture at runtime so repository push
// protection does not mistake the deliberately credential-shaped test value
// for a usable credential in source control.
func testAWSAccessKeyID() string {
	return strings.Join([]string{"AK", "IA", "7QHWN2DKR4FYPLJM"}, "")
}

func TestDefiniteRules(t *testing.T) {
	cases := []struct {
		name string
		rule string
		text string
		want bool
	}{
		{"github classic", "github-pat",
			"tok ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg x", true},
		{"github fine-grained", "github-pat",
			"github_pat_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4HgN_X2cWp9", true},
		{"slack bot", "slack-token",
			"xoxb-549271836401-fHk7Bm3Pz9Wt5Vx2Yq8Nc", true},
		{"stripe live", "stripe-secret",
			"sk_live_7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm", true},
		{"google api", "google-api-key",
			"AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4H", true},
		{"google api ending dash", "google-api-key",
			"key AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4- end", true},
		{"pem block", "private-key-block",
			"-----BEGIN RSA PRIVATE KEY-----\n" +
				rep("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
				"-----END RSA PRIVATE KEY-----", true},
		{"plain prose", "", "the quick brown fox jumps over", false},
		{"short ghp", "", "ghp_tooShort", false},
		{"openai project", "openai-key",
			"tok sk-proj-Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp end", true},
		{"openai svcacct", "openai-key",
			"tok sk-svcacct-Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp end", true},
		{"openai admin", "openai-key",
			"tok sk-admin-Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp end", true},
		{"openai ending dash", "openai-key",
			"tok sk-proj-Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Y- end", true},
		{"openai ending underscore", "openai-key",
			"tok sk-proj-Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Y_ end", true},
		{"openai at eot", "openai-key",
			"tok sk-proj-Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp", true},
		{"openai trailing-run placeholder", "",
			"tok sk-proj-AAAAAAAAAAAAAAAAAAAAA end", false},
		{"openai low-entropy body", "",
			"tok sk-proj-abababababababababab end", false},
		{"openai short body", "",
			"tok sk-proj-short end", false},
		{"openai bare sk- not matched", "",
			"tok sk-Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1YpA end", false},
		{"gitlab pat", "gitlab-pat",
			"tok glpat-Xa9Kd03Lm5Qp7Rt2Vw8Zb end", true},
		{"gitlab pat ending dash", "gitlab-pat",
			"tok glpat-Xa9Kd03Lm5Qp7Rt2Vw8Z- end", true},
		{"gitlab pat at eot", "gitlab-pat",
			"tok glpat-Xa9Kd03Lm5Qp7Rt2Vw8Zb", true},
		{"gitlab placeholder repeating", "",
			"tok glpat-AAAAAAAAAAAAAAAAAAAA end", false},
		{"gitlab placeholder monotone", "",
			"tok glpat-abcdefghijklmnopqrst end", false},
		{"gitlab short body", "",
			"tok glpat-short end", false},
		{"gitlab trailing-run placeholder", "",
			"tok glpat-Xa9Kd03Lm5Qp7Rt2AAAA end", false},
		{"npm token", "npm-token",
			"tok npm_Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1YpAbCdEf end", true},
		{"npm placeholder repeating", "",
			"tok npm_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA end", false},
		{"npm legacy bare hex not matched", "",
			"tok 0123456789abcdef0123456789abcdef0123 end", false},
		{"npm short body", "",
			"tok npm_short end", false},
		{"npm trailing-run placeholder", "",
			"tok npm_Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1YpAbAAAA end", false},
		{"pypi token 69 tail matches", "pypi-token",
			"tok pypi-AgEIcHlwaS5vcmcCXa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp8Bv4HgNX2cWpQz7MjRv3Ts6Ku0Fy9DnEwPaRbTZ end", true},
		{"pypi token 68 tail rejected", "",
			"tok pypi-AgEIcHlwaS5vcmcCXa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp8Bv4HgNX2cWpQz7MjRv3Ts6Ku0Fy9DnEwPaRbT end", false},
		{"pypi token ending dash 69 tail", "pypi-token",
			"tok pypi-AgEIcHlwaS5vcmcCXa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp8Bv4HgNX2cWpQz7MjRv3Ts6Ku0Fy9DnEwPaRbT- end", true},
		{"pypi placeholder repeating", "",
			"tok pypi-AgEIcHlwaS5vcmcC" + rep("AAAAAAAAA", 8) + " end", false},
		{"pypi trailing-run placeholder", "",
			"tok pypi-AgEIcHlwaS5vcmcCXa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1Yp8Bv4HgNX2cWpQz7MjRv3Ts6Ku0Fy9DnEwPAAAAA end", false},
		{"hf token", "huggingface-token",
			"tok hf_Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1YpAbCd end", true},
		{"hf token longer", "huggingface-token",
			"tok hf_Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1YpAbCdEfGhIj end", true},
		{"hf token at eot", "huggingface-token",
			"tok hf_Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6Hf1YpAbCd", true},
		{"hf placeholder repeating", "",
			"tok hf_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA end", false},
		{"hf placeholder monotone", "",
			"tok hf_abcdefghijklmnopqrstuvwxyzABCD end", false},
		{"hf short body", "",
			"tok hf_short end", false},
		{"hf trailing-run placeholder", "",
			"tok hf_Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6HAAAA end", false},
		// sendgrid-key: format SG.<22 base64url>.<43 base64url>
		// ident22 = "Xa9Kd03Lm5Qp7Rt2Vw8Zb4" (22 chars)
		// secret43 = "yifDLkDmWJ6UuVTAIjvFu7WICPhDeOZIiBOB-Y6sHrF" (43 chars)
		{"sendgrid key", "sendgrid-key",
			"tok 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 end", true},
		{"sendgrid key ending dash", "sendgrid-key",
			"tok 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 end", true},
		{"sendgrid key at eot", "sendgrid-key",
			"tok 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", true},
		{"sendgrid key trailing dot stops at dot", "sendgrid-key",
			"tok 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. end", true},
		// negative cases
		{"sendgrid missing separator", "",
			"tok SG.Xa9Kd03Lm5Qp7Rt2Vw8Zb4yifDLkDmWJ6UuVTAIjvFu7WICPhDeOZIiBOB-Y6sHrF end", false},
		// wrong ident = 20 chars ("Xa9Kd03Lm5Qp7Rt2Vw8Z", drop last 2 from ident22)
		{"sendgrid wrong identifier length", "",
			"tok SG.Xa9Kd03Lm5Qp7Rt2Vw8Z.yifDLkDmWJ6UuVTAIjvFu7WICPhDeOZIiBOB-Y6sHrF end", false},
		// wrong secret = 44 chars (append "S" to secret43)
		{"sendgrid wrong secret length", "",
			"tok SG.Xa9Kd03Lm5Qp7Rt2Vw8Zb4.yifDLkDmWJ6UuVTAIjvFu7WICPhDeOZIiBOB-Y6sHrFS end", false},
		{"sendgrid placeholder repeating", "",
			"tok SG.AAAAAAAAAAAAAAAAAAAAAA.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB end", false},
		// trailing-run secret = first 39 of secret43 + "AAAA" = 43 chars
		// secret43[:39]="yifDLkDmWJ6UuVTAIjvFu7WICPhDeOZIiBOB-Y6" + "AAAA"
		{"sendgrid trailing-run placeholder", "",
			"tok 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 end", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scan(c.text)
			found := ""
			for _, m := range got {
				if m.Rule == c.rule {
					found = m.Rule
				}
			}
			if c.want {
				assert.NotEmpty(t, found,
					"expected rule %q to match %q; got %+v", c.rule, c.text, got)
			} else {
				assert.Empty(t, got,
					"expected no match for %q; got %+v", c.text, got)
			}
		})
	}
}

func TestCandidateRules(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dumm_Sig-Value12345"
	cases := []struct {
		name string
		rule string
		text string
		want bool
	}{
		{"jwt", "jwt", "auth: " + jwt, true},
		{"high entropy assignment", "high-entropy-assignment",
			"SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6", true},
		{"low entropy assignment", "high-entropy-assignment",
			"NAME=aaaaaaaaaaaaaaaaaaaa", false},
		{"short assignment", "high-entropy-assignment", "X=ab12", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scan(c.text)
			found := false
			for _, m := range got {
				if m.Rule == c.rule {
					found = true
					assert.Equal(t, ConfidenceCandidate, m.Confidence,
						"%s confidence", c.rule)
				}
			}
			assert.Equal(t, c.want, found,
				"rule %q match=%v want=%v for %q (got %+v)",
				c.rule, found, c.want, c.text, got)
		})
	}
}

// TestScanDefiniteReturnsOnlyDefinite confirms the inline-sync scan path
// reports definite vendor formats and skips the FP-prone candidate heuristics
// (high-entropy assignments, JWTs, basic-auth URLs) entirely.
func TestScanDefiniteReturnsOnlyDefinite(t *testing.T) {
	// One definite AWS key and one candidate high-entropy assignment.
	text := "aws " + testAWSAccessKeyID() +
		" and SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6"
	full := Scan(text)
	require.Len(t, full, 2,
		"precondition: Scan should report 2 matches (1 definite, 1 candidate)")
	got := ScanDefinite(text)
	require.Len(t, got, 1)
	assert.Equal(t, "aws-access-key", got[0].Rule)
	for _, m := range got {
		assert.Equal(t, ConfidenceDefinite, m.Confidence,
			"ScanDefinite returned non-definite match: %+v", m)
	}
}

// TestScanDefiniteMatchesScanDefiniteSubset confirms ScanDefinite yields the
// same spans (rule, offsets, redaction) that Scan reports for definite rules,
// so findings stored by the inline path and the full scan stay consistent.
func TestScanDefiniteMatchesScanDefiniteSubset(t *testing.T) {
	text := "key " + testAWSAccessKeyID() +
		" tok ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg" +
		" SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6"
	var wantDef []Match
	for _, m := range Scan(text) {
		if m.Confidence == ConfidenceDefinite {
			wantDef = append(wantDef, m)
		}
	}
	got := ScanDefinite(text)
	require.Len(t, got, len(wantDef),
		"ScanDefinite count vs Scan definite count (%+v vs %+v)", got, wantDef)
	for i := range got {
		assert.Equal(t, wantDef[i].Rule, got[i].Rule, "match %d rule differs", i)
		assert.Equal(t, wantDef[i].Start, got[i].Start, "match %d start differs", i)
		assert.Equal(t, wantDef[i].End, got[i].End, "match %d end differs", i)
		assert.Equal(t, wantDef[i].Redacted, got[i].Redacted, "match %d redacted differs", i)
	}
}

// TestDefiniteRulesVersionDistinctFromFull pins the split-versioning contract:
// the inline definite-only scan stamps a version that differs from the full
// ruleset version, so secrets scan --backfill (which treats RulesVersion as
// current) re-scans inline-only sessions to pick up candidate findings.
func TestDefiniteRulesVersionDistinctFromFull(t *testing.T) {
	def := DefiniteRulesVersion()
	full := RulesVersion()
	require.NotEqual(t, full, def,
		"DefiniteRulesVersion must differ from RulesVersion (both %q)", def)
	require.NotEmpty(t, def, "versions must be non-empty")
	require.NotEmpty(t, full, "versions must be non-empty")
	assert.Equal(t, def, DefiniteRulesVersion(), "DefiniteRulesVersion not stable across calls")
	assert.Len(t, def, 64, "DefiniteRulesVersion length: %q", def)
	for _, c := range def {
		require.True(t, isLowerHex(c),
			"DefiniteRulesVersion has non-hex char %q in %q", c, def)
	}
}

func TestRulesVersionStableAndHex(t *testing.T) {
	v1 := RulesVersion()
	v2 := RulesVersion()
	require.Equal(t, v1, v2, "RulesVersion not stable")
	require.Len(t, v1, 64, "RulesVersion length: %q", v1) // sha256 hex
	for _, c := range v1 {
		require.True(t, isLowerHex(c),
			"RulesVersion has non-hex char %q in %q", c, v1)
	}
}

func TestVerify(t *testing.T) {
	// Non-grouped rule: the stored span is the full regex match.
	awsKey := testAWSAccessKeyID()
	awsSrc := "export KEY=" + awsKey + " done"
	s := strings.Index(awsSrc, awsKey)
	e := s + len(awsKey)
	assert.True(t, Verify("aws-access-key", awsSrc, s, e),
		"Verify should accept a valid AWS key at its coordinates")
	assert.False(t, Verify("aws-access-key", awsSrc, 0, 6),
		"Verify should reject coordinates that are not the key")
	assert.False(t, Verify("nonexistent-rule", awsSrc, s, e),
		"Verify should reject an unknown rule")
	assert.False(t, Verify("aws-access-key", awsSrc, s, len(awsSrc)+10),
		"Verify should reject out-of-bounds coordinates")
	// Grouped rule: the stored span is the captured group (the password),
	// not the full URL match. Verify must still accept it.
	urlSrc := "db=postgres://user:s3cretP4ss@host:5432/db"
	ps := strings.Index(urlSrc, "s3cretP4ss")
	pe := ps + len("s3cretP4ss")
	assert.True(t, Verify("basic-auth-url", urlSrc, ps, pe),
		"Verify should accept a grouped finding at its group coordinates")
}

// TestVerifyDetectsChangedSource locks in the core --reveal guarantee: a scan
// produces coordinates, Verify accepts them on the unchanged source, and
// rejects them once the bytes at those coordinates are no longer the secret.
func TestVerifyDetectsChangedSource(t *testing.T) {
	source := "export AWS=" + testAWSAccessKeyID()
	// Seed from canonical Scan (what produces findings and what Verify uses).
	matches := Scan(source)
	require.NotEmpty(t, matches, "expected at least one match in source")
	m := matches[0]
	assert.True(t, Verify(m.Rule, source, m.Start, m.End),
		"Verify should accept unchanged source at [%d,%d)", m.Start, m.End)
	// Same length, but the secret at [Start,End) is replaced by a zero-entropy
	// run that matches no rule, so Verify must reject the stale coordinates.
	changed := source[:m.Start] + strings.Repeat("X", m.End-m.Start)
	assert.False(t, Verify(m.Rule, changed, m.Start, m.End),
		"Verify should reject when the source changed at those coords")
}

// TestVerifyRejectsSuppressedCandidate ensures Verify mirrors canonical Scan,
// not raw scanning: a candidate that overlaps a definite is suppressed by Scan,
// so Verify must reject its coordinates even though scanRaw reports it.
func TestVerifyRejectsSuppressedCandidate(t *testing.T) {
	src := "https://user:sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4@example.com"
	var cand Match
	for _, m := range scanRaw(src) {
		if m.Rule == "basic-auth-url" {
			cand = m
			break
		}
	}
	require.NotEmpty(t, cand.Rule,
		"precondition: scanRaw should report a basic-auth-url candidate")
	assert.False(t, Verify("basic-auth-url", src, cand.Start, cand.End),
		"Verify must reject a candidate that canonical Scan suppresses")
}

// isLowerHex reports whether c is a lowercase hexadecimal digit, the alphabet
// a SHA-256 hex digest is built from.
func isLowerHex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// rep returns s repeated n times (test helper for building token bodies).
func rep(s string, n int) string {
	var out strings.Builder
	for range n {
		out.WriteString(s)
	}
	return out.String()
}

// TestHasRepeatingBlock pins the seed-pattern detector that catches
// placeholders built by repeating a short string. Block size 1 is the
// "ghp_aaaa…" shape; sizes 2..6 cover "A1b2A1b2…", "aB3_xaB3_x…", etc.
func TestHasRepeatingBlock(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool
	}{
		{"single byte dominating", strings.Repeat("a", 36), true},
		{"block size 4 A1b2", strings.Repeat("A1b2", 20), true},
		{"block size 4 a1B2", strings.Repeat("a1B2", 8), true},
		{"block size 5 aB3_x", strings.Repeat("aB3_x", 7), true},
		{"block size 2", strings.Repeat("xy", 10), true},
		{"random body", "7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm", false},
		{"random aws body", "7QHWN2DKR4FYPLJM", false},
		{"random pat body", "8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg", false},
		{"too short", "abcd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, hasRepeatingBlock(c.s))
		})
	}
}

// TestHighEntropyPaddingCapture verifies that the high-entropy-assignment
// rule includes base64 '=' padding in the captured span.
func TestHighEntropyPaddingCapture(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		suffix string // expected last chars of the captured span
	}{
		{"double padding",
			`KEY="Xa9Kd03Lm5Qp7Rt2Vw8Zb4N=="`,
			`==`},
		{"single padding",
			`KEY="Xa9Kd03Lm5Qp7Rt2Vw8Zb4=`,
			`=`},
		{"no padding",
			`KEY="Xa9Kd03Lm5Qp7Rt2Vw8Zb4N"`,
			`N`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scan(c.text)
			var m *Match
			for i := range got {
				if got[i].Rule == "high-entropy-assignment" {
					m = &got[i]
					break
				}
			}
			require.NotNil(t, m, "no high-entropy match in %q; got %+v", c.text, got)
			span := c.text[m.Start:m.End]
			if !strings.HasSuffix(span, c.suffix) {
				t.Errorf("captured span %q does not end with %q",
					span, c.suffix)
			}
		})
	}
}

// TestHighEntropyValueTrimsPaddingBeforeLength verifies that
// highEntropyValue trims trailing '=' before applying the 20-char
// length floor. A 19-char body padded to 21 chars must fail the new
// gate (body length 19 after trim) even though the raw input is 21
// chars — proving the trim runs before the length check.
func TestHighEntropyValueTrimsPaddingBeforeLength(t *testing.T) {
	const body = "A1b2C3d4E5f6G7h8I9j" // 19 chars
	if len(body) != 19 {
		t.Fatalf("test setup: body length = %d, want 19", len(body))
	}
	padded := body + "=="
	if highEntropyValue(padded) {
		t.Errorf("highEntropyValue(%q) = true, want false "+
			"(trimmed body is 19 chars, below the 20-char floor)",
			padded)
	}
}

// TestHighEntropyValueAcceptsPaddedRealBody verifies that the validator
// accepts a real high-entropy body whether or not '=' padding is
// appended.
func TestHighEntropyValueAcceptsPaddedRealBody(t *testing.T) {
	const body = "Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6" // 25 chars, real high entropy
	if !highEntropyValue(body) {
		t.Fatalf("precondition: bare body should pass")
	}
	for _, pad := range []string{"=", "=="} {
		if !highEntropyValue(body + pad) {
			t.Errorf("highEntropyValue(%q) = false, want true",
				body+pad)
		}
	}
}

// TestHasMonotoneRun pins the alphabet/digit-run detector that catches
// placeholders built from sequential ASCII ("abcdef", "1234567890",
// "ZYXWVU"). The 6-char minimum is small enough to catch the dominant
// noise shapes without flagging random secrets that happen to include a
// short run by chance.
func TestHasMonotoneRun(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool
	}{
		{"abcdef", "abcdef", true},
		{"1234567890", "1234567890", true},
		{"ZYXWVU", "ZYXWVU", true},
		{"fedcba", "fedcba", true},
		{"abcde (only 5)", "abcde", false},
		{"random", "7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm", false},
		{"isolated +1 transitions", "549271836401", false},
		{"embedded run", "Xabcdef9", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, hasMonotoneRun(c.s, 6))
		})
	}
}
