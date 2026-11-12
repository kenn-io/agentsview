package secrets

import (
	"encoding/json"
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

// TestRedactKeepsAssetReferences pins that agentsview's own content-addressed
// image references survive masking. "asset:" reads like a key and the SHA-256
// path clears the high-entropy gate, but the reference names a local file by
// its hash and carries no secret; masking it breaks every exported image.
func TestRedactKeepsAssetReferences(t *testing.T) {
	ref := "asset://ae4b3280e56e2faf83f414a6e3dabe9d5fbe18976544c05fed121accb85b53fc.png"
	for _, text := range []string{
		`{"image_ref":"` + ref + `"}`,
		"![Image: image/png, 3 bytes](" + ref + ")",
	} {
		assert.Equal(t, text, Redact(text))
		assert.Equal(t, text, RedactWindow(text, 0, len(text)))
	}
	// A value that is not a hex digest after "asset:" is still masked.
	notRef := "asset:dGhpc0lzQVJlYWxTZWNyZXRWYWx1ZTEyMzQ1Njc4OQ end"
	assert.NotEqual(t, notRef, Redact(notRef))
}

// Text can hold escaped string literals, such as the preview AgentsView
// stores for input Codex sends to a terminal. The "n" of a "\n" escape removes
// a key's leading word boundary, and a key block's "\n" escapes fail the PEM
// body check; masking must catch both and keep the escapes around them.
func TestRedactMasksSecretsInEscapedText(t *testing.T) {
	keyBlock := `-----BEGIN RSA PRIVATE KEY-----\n` +
		strings.Repeat(`MIIBSECRETKEYMATERIAL0123456789ABCDEF\n`, 5) +
		`-----END RSA PRIVATE KEY-----`
	for name, tc := range map[string]struct{ in, want string }{
		"key after an escaped newline": {
			in:   "$ stdin -> 7\n" + `cat creds\nAKIA7QHWN2DKR4FYPLJM\n`,
			want: "$ stdin -> 7\n" + `cat creds\nAKIA…PLJM\n`,
		},
		"key after an escaped tab": {
			in:   `key:\tghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg`,
			want: `key:\tghp_…v4Hg`,
		},
		"escaped key block": {
			in:   `cat id_rsa\n` + keyBlock + `\n`,
			want: `cat id_rsa\n[redacted private key block]\n`,
		},
		"escaped backslash before a letter": {
			in:   `path C:\\nAKIA7QHWN2DKR4FYPLJM`,
			want: `path C:\\nAKIA7QHWN2DKR4FYPLJM`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Redact(tc.in))
		})
	}
}

// TestRedactJSONMasksSecretsHiddenByEscapes covers secrets that serialized
// JSON hides from a plain-text scan: the "n" of an escaped newline sits
// against a key and removes its word boundary, and a key block's "\n"
// escapes fail the PEM body check. Decoding each string first exposes both.
func TestRedactJSONMasksSecretsHiddenByEscapes(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
		"-----END RSA PRIVATE KEY-----"
	for name, decoded := range map[string]string{
		"key after newline": "creds:\nAKIA7QHWN2DKR4FYPLJM\n",
		"key block":         "cat id_rsa\n" + pem + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{
				"type": "tool_result", "content": decoded, "n": 1,
			})
			require.NoError(t, err)

			got := RedactJSON(string(encoded))
			var out map[string]any
			require.NoError(t, json.Unmarshal([]byte(got), &out), "masked JSON must stay valid: %s", got)
			assert.Equal(t, Redact(decoded), out["content"])
			assert.NotContains(t, got, "SECRETKEYMATERIAL")
			assert.NotContains(t, got, "AKIA7QHWN2DKR4FYPLJM")
			assert.Equal(t, "tool_result", out["type"])
			assert.EqualValues(t, 1, out["n"])
		})
	}
}

func TestRedactJSONNestedAndPlainText(t *testing.T) {
	// Tool input often holds a JSON document inside a JSON string.
	inner, err := json.Marshal(map[string]string{"content": "x\nAKIA7QHWN2DKR4FYPLJM"})
	require.NoError(t, err)
	outer, err := json.Marshal(map[string]string{"input": string(inner)})
	require.NoError(t, err)
	assert.NotContains(t, RedactJSON(string(outer)), "AKIA7QHWN2DKR4FYPLJM")

	// Text that is not JSON gets the plain-text scan.
	text := "export AWS_KEY=AKIA7QHWN2DKR4FYPLJM"
	assert.Equal(t, Redact(text), RedactJSON(text))
	// JSON with nothing to mask is returned byte for byte.
	clean := `{"a": "b",  "c": [1, 2]}`
	assert.Equal(t, clean, RedactJSON(clean))
}

// TestRedactJSONKeepsMaskingOnImperfectJSON covers JSON the strict decoder
// rejects: duplicate keys, which agent tool arguments can carry, and a record
// cut off mid-write, as the last line of a session log being written can be.
// Masking already decided for complete strings must survive, and an escaped
// key in the unfinished string is still masked.
func TestRedactJSONKeepsMaskingOnImperfectJSON(t *testing.T) {
	escaped := `creds:\nAKIA7QHWN2DKR4FYPLJM`
	for name, tc := range map[string]struct{ in, want string }{
		"duplicate keys": {
			in:   `{"content":"` + escaped + `","content":"done"}`,
			want: `{"content":"creds:\nAKIA…PLJM","content":"done"}`,
		},
		"truncated record": {
			in:   `{"content":"` + escaped + `","more":"unfinish`,
			want: `{"content":"creds:\nAKIA…PLJM","more":"unfinish`,
		},
		"key in the unfinished string": {
			in:   `{"content":"done","more":"` + escaped + ` and more`,
			want: `{"content":"done","more":"creds:\nAKIA…PLJM and more`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactJSON(tc.in))
			assert.Equal(t, tc.want+"\n", RedactJSONLines(tc.in+"\n"))
		})
	}
}

func TestRedactJSONLines(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
		"-----END RSA PRIVATE KEY-----"
	key, err := json.Marshal(pem)
	require.NoError(t, err)
	jsonl := `{"type":"user","content":"hi"}` + "\n" +
		`{"type":"tool_result","content":"creds:\nAKIA7QHWN2DKR4FYPLJM"}` + "\n" +
		`{"type":"tool_result","content":` + string(key) + `}` + "\n" +
		`{"type":"partial` + "\n"
	got := RedactJSONLines(jsonl)
	assert.NotContains(t, got, "AKIA7QHWN2DKR4FYPLJM")
	assert.NotContains(t, got, "SECRETKEYMATERIAL")
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 5, "line structure is kept")
	assert.Equal(t, `{"type":"user","content":"hi"}`, lines[0])
	for _, line := range lines[1:3] {
		assert.True(t, json.Valid([]byte(line)), "masked line stays valid JSON: %s", line)
	}
	assert.Equal(t, `{"type":"partial`, lines[3], "a truncated line passes through")

	// A Markdown history is masked whole, so a key block across lines is caught.
	markdown := "# aider chat\n\n> cat id_rsa\n" + pem + "\n"
	assert.NotContains(t, RedactJSONLines(markdown), "SECRETKEYMATERIAL")
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
