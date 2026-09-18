package secrets

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScanFindsAWSAccessKey(t *testing.T) {
	text := "export AWS_KEY=AKIA7QHWN2DKR4FYPLJM then continue"
	got := Scan(text)
	require.Len(t, got, 1)
	m := got[0]
	assert.Equal(t, "aws-access-key", m.Rule)
	assert.Equal(t, ConfidenceDefinite, m.Confidence)
	assert.Equal(t, "AKIA7QHWN2DKR4FYPLJM", text[m.Start:m.End])
	assert.Equal(t, 0, m.Index)
}

func TestScanNoMatch(t *testing.T) {
	assert.Empty(t, Scan("just some ordinary prose with no secrets"))
}

func TestRedactMasksSecretButKeepsContext(t *testing.T) {
	text := "export AWS_KEY=AKIA7QHWN2DKR4FYPLJM then continue"
	got := Redact(text)
	assert.NotContains(t, got, "AKIA7QHWN2DKR4FYPLJM", "Redact leaked the full secret")
	assert.True(t, strings.HasPrefix(got, "export AWS_KEY="), "Redact dropped surrounding context: %q", got)
	assert.True(t, strings.HasSuffix(got, " then continue"), "Redact dropped trailing context: %q", got)
	assert.Contains(t, got, "AKIA…PLJM", "Redact did not use the masked form")
}

func TestRedactNoMatchReturnsInput(t *testing.T) {
	in := "nothing to see here"
	assert.Equal(t, in, Redact(in))
}

func TestScanSuppressesCandidateOverlappingDefinite(t *testing.T) {
	// The basic-auth-url candidate and anthropic-key definite both match
	// inside this URL; only the definite finding should be returned.
	text := "https://user:sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4@example.com"
	got := Scan(text)
	for _, m := range got {
		assert.NotEqual(t, ConfidenceCandidate, m.Confidence,
			"candidate %q not suppressed despite overlapping definite", m.Rule)
	}
	require.NotEmpty(t, got, "expected at least the definite anthropic-key finding")
}

func TestRedactMasksUnionIncludingCandidate(t *testing.T) {
	text := "TOKEN=sk-ant-api03-Nc6Mp1Hj9Bg3Tf5Ds8Lr0E end"
	got := Redact(text)
	assert.NotContains(t, got, "sk-ant-api03-Nc6Mp1Hj9Bg3Tf5Ds8Lr0E", "Redact leaked secret")
}

// TestRedactWindowMasksStraddlingSecret pins the content-search guarantee: a
// secret that extends past the snippet window is still masked. Redacting the
// truncated window directly would see only a fragment (here a PEM block missing
// its END line), fail to match any rule, and leak raw key bytes.
func TestRedactWindowMasksStraddlingSecret(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
		"-----END RSA PRIVATE KEY-----"
	full := "see attached key " + pem + " thanks"
	pemStart := strings.Index(full, "-----BEGIN")
	lo, hi := pemStart-5, pemStart+40 // overlaps the PEM but cuts before END

	// Hazard check: redacting the bare window leaks, because the fragment has
	// no END line for the private-key-block rule to anchor on.
	naive := Redact(full[lo:hi])
	require.Contains(t, naive, "BEGIN RSA PRIVATE KEY",
		"precondition: window should straddle the key")

	got := RedactWindow(full, lo, hi)
	assert.NotContains(t, got, "SECRETKEYMATERIAL", "RedactWindow leaked straddling key material")
	assert.Contains(t, got, "[redacted private key block]", "RedactWindow did not mask the key block")
}

// TestRedactWindowMasksStraddlingGroupedSecret covers grouped rules, whose
// reported span is a capture group (the password / the high-entropy value), not
// the full match. A window covering only the group must still mask it: redacting
// the bare slice would fail to re-detect (the "scheme://user:" or "key=" context
// is gone) and leak the secret.
func TestRedactWindowMasksStraddlingGroupedSecret(t *testing.T) {
	t.Run("high-entropy-assignment", func(t *testing.T) {
		val := "Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6QrStUvWxYz0123"
		full := "export api_key=" + val + " done"
		vs := strings.Index(full, val)
		// Window starts inside the value, past the "api_key=" the rule needs.
		got := RedactWindow(full, vs+5, vs+15)
		assert.NotContains(t, got, val[5:20], "leaked high-entropy value fragment")
	})
	t.Run("basic-auth-url", func(t *testing.T) {
		pw := "Sup3rSecretP4ssw0rd"
		full := "db=postgres://user:" + pw + "@host:5432/app"
		ps := strings.Index(full, pw)
		// Window lands inside the password, past the "://user:" the rule needs.
		got := RedactWindow(full, ps+2, ps+8)
		assert.NotContains(t, got, pw[2:12], "leaked basic-auth password fragment")
	})
}

// TestRedactWindowKeepsContextAndContainedSecrets checks the common path: a
// secret fully inside the window keeps its rule mask, surrounding context
// survives, and a window with no secret is returned verbatim.
func TestRedactWindowKeepsContextAndContainedSecrets(t *testing.T) {
	full := "the key is AKIA7QHWN2DKR4FYPLJM in config"
	got := RedactWindow(full, 0, len(full))
	assert.NotContains(t, got, "AKIA7QHWN2DKR4FYPLJM", "contained secret not masked")
	assert.Contains(t, got, "the key is ", "context not preserved")
	assert.Contains(t, got, " in config", "context not preserved")
	clean := "just some ordinary prose with no secrets at all"
	assert.Equal(t, clean, RedactWindow(clean, 0, len(clean)))
}

func TestRedactNeverLeaksKnownSecrets(t *testing.T) {
	secrets := []string{
		"AKIA7QHWN2DKR4FYPLJM",
		"ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg",
		"xoxb-549271836401-fHk7Bm3Pz9Wt5Vx2Yq8Nc",
		"xoxs-302846159270-xPk9Bm3Wv8Qt5Lz2Yh7Fc",
		"sk_live_7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm",
		"AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4H",
		"AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4-",
		"sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dumm_Sig-Value12345",
	}
	for _, sec := range secrets {
		for _, tmpl := range []string{"%s", "prefix %s suffix", "a=%s\nb=2"} {
			text := fmt.Sprintf(tmpl, sec)
			out := Redact(text)
			assert.NotContains(t, out, sec,
				"Redact leaked %q in template %q -> %q", sec, tmpl, out)
		}
	}
}

func TestScanRedactedNeverEqualsFullSecret(t *testing.T) {
	// One sample per rule, scanned in isolation, so every rule's masked form
	// is exercised. The private-key-block mask is a fixed string and trivially
	// differs from its (multi-line) match, so it is covered by the others.
	samples := []string{
		"k=AKIA7QHWN2DKR4FYPLJM",
		"tok ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg",
		"xoxb-549271836401-fHk7Bm3Pz9Wt5Vx2Yq8Nc",
		"sk_live_7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm",
		"AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4H",
		"sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4",
		"auth: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dumm_Sig-Value12345",
		"https://user:supersecretpw@example.com",
		"SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6",
	}
	for _, text := range samples {
		matches := Scan(text)
		if len(matches) == 0 {
			assert.Failf(t, "no matches", "sample %q", text)
			continue
		}
		for _, m := range matches {
			full := text[m.Start:m.End]
			assert.NotEqual(t, full, m.Redacted,
				"Redacted equals full secret for rule %q: %q", m.Rule, full)
		}
	}
}

func TestBasicAuthURLDetectsPasswordSpan(t *testing.T) {
	text := "db at postgres://admin:Sup3rSecretPw@db.example.com:5432/app"
	var m *Match
	for _, got := range Scan(text) {
		if got.Rule == "basic-auth-url" {
			m = &got
		}
	}
	require.NotNil(t, m, "expected a basic-auth-url candidate; got %+v", Scan(text))
	assert.Equal(t, "Sup3rSecretPw", text[m.Start:m.End])
	assert.Equal(t, ConfidenceCandidate, m.Confidence)
	red := Redact(text)
	// Assert the exact fully-masked form: no password character survives
	// (this fails if the mask is loosened to reveal a suffix) while the
	// surrounding URL context is preserved.
	assert.Contains(t, red, "postgres://admin:…@db.example.com",
		"Redact did not fully mask the password in context")
}

func TestScanJWTNotDuplicatedAsHighEntropy(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dumm_Sig-Value12345"
	got := Scan("auth: " + jwt)
	foundJWT := false
	for _, m := range got {
		assert.NotEqual(t, "high-entropy-assignment", m.Rule,
			"JWT segment reported as high-entropy-assignment: %+v", got)
		if m.Rule == "jwt" {
			foundJWT = true
		}
	}
	assert.True(t, foundJWT, "expected a jwt candidate; got %+v", got)
}
