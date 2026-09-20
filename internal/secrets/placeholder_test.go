package secrets

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRejectsAWSDocsPlaceholders pins the new aws-access-key validator: every
// AKIA/ASIA key that AWS itself documents as the example value, plus the
// EXAMPL[0-9] variants used in test fixtures, must be skipped. These are the
// values that flood agentsview's stored findings on any session that
// discusses IAM or scans a sample, and they are the user-reported false
// positives the validator exists to eliminate.
func TestRejectsAWSDocsPlaceholders(t *testing.T) {
	placeholders := []string{
		// AWS canonical example from the IAM docs.
		"AKIAIOSFODNN7EXAMPLE",
		// EXAMPL[0-9] variants used in test fixtures across the ecosystem.
		"AKIAIOSFODNN7EXAMPL1",
		"AKIAIOSFODNN7EXAMPL2",
		"AKIAIOSFODNN7EXAM123",
		// ASIA (session) variant.
		"ASIAIOSFODNN7EXAMPLE",
	}
	for _, p := range placeholders {
		t.Run(p, func(t *testing.T) {
			text := "key=" + p + " end"
			for _, m := range Scan(text) {
				assert.NotEqual(t, "aws-access-key", m.Rule,
					"aws-access-key matched placeholder %q (mask=%q)",
					p, m.Redacted)
			}
		})
	}
}

// TestAcceptsRealAWSKeys confirms the filters do not over-reach: AKIA keys
// whose body neither contains the doc markers nor matches a structural
// placeholder shape (single byte dominating, repeating short block,
// monotone alphabet/digit run) must still match.
func TestAcceptsRealAWSKeys(t *testing.T) {
	realLooking := []string{
		"AKIA7QHWN2DKR4FYPLJM",
		"AKIA3VBMK8XJZ6WPCNQH",
		"ASIA5GTKD7RPYNXQVMBL",
	}
	for _, k := range realLooking {
		t.Run(k, func(t *testing.T) {
			text := "key=" + k + " end"
			found := false
			for _, m := range Scan(text) {
				if m.Rule == "aws-access-key" {
					found = true
				}
			}
			assert.True(t, found, "aws-access-key did not match real-looking key %q", k)
		})
	}
}

// TestRejectsAnthropicPlaceholders pins the anthropic-key validator: keys
// whose final four characters repeat are dropped. These dominate
// agentsview's own historical findings (sk-ant-...AAAA, ...BBBB) and the
// public test corpora.
func TestRejectsAnthropicPlaceholders(t *testing.T) {
	placeholders := []string{
		"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAA",
		"sk-ant-api03-BBBBBBBBBBBBBBBBBBBBBB",
		"sk-ant-api03-CCCCCCCCCCCCCCCCCCCCCC",
		"sk-ant-api03-" + strings.Repeat("Z", 24),
		"sk-ant-api03-XyZ1aB2cD3eF4gH5iJ60000",
	}
	for _, p := range placeholders {
		t.Run(p, func(t *testing.T) {
			text := "TOKEN=" + p + " end"
			for _, m := range Scan(text) {
				assert.NotEqual(t, "anthropic-key", m.Rule,
					"anthropic-key matched placeholder %q (mask=%q)",
					p, m.Redacted)
			}
		})
	}
}

// TestAcceptsRealAnthropicKeys confirms the suffix-repeat filter does not
// drop high-entropy keys that happen to share a single trailing character.
func TestAcceptsRealAnthropicKeys(t *testing.T) {
	realLooking := []string{
		"sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4",
		"sk-ant-api03-Nc6Mp1Hj9Bg3Tf5Ds8Lr0E",
		// Only the last char repeats; not enough to trigger the filter.
		"sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8ZbE",
	}
	for _, k := range realLooking {
		t.Run(k, func(t *testing.T) {
			text := "TOKEN=" + k + " end"
			found := false
			for _, m := range Scan(text) {
				if m.Rule == "anthropic-key" {
					found = true
				}
			}
			assert.True(t, found, "anthropic-key did not match real-looking key %q", k)
		})
	}
}

// TestRejectsSlackPlaceholders pins the slack-token validator: tokens
// ending in the "0123" canonical fake suffix or a 4-character repeat are
// dropped. The "0123" suffix is the Slack docs convention for fake tokens
// and the dominant noise pattern in agentsview's stored findings.
func TestRejectsSlackPlaceholders(t *testing.T) {
	placeholders := []string{
		"xoxb-123456789012-abcdefABCDEF0123",
		"xoxs-123456789012-abcdefABCDEF0123",
		"xoxr-123456789012-abcdefABCDEF0123",
		"xoxb-123456789012-abcdefABCDEAAAA",
	}
	for _, p := range placeholders {
		t.Run(p, func(t *testing.T) {
			text := "TOKEN=" + p + " end"
			for _, m := range Scan(text) {
				assert.NotEqual(t, "slack-token", m.Rule,
					"slack-token matched placeholder %q (mask=%q)",
					p, m.Redacted)
			}
		})
	}
}

// TestAcceptsRealSlackTokens confirms the filter does not drop tokens
// whose body passes the structural checks (no dominant byte, no short
// repeating block, no monotone alphabet/digit run).
func TestAcceptsRealSlackTokens(t *testing.T) {
	realLooking := []string{
		"xoxb-549271836401-fHk7Bm3Pz9Wt5Vx2Yq8Nc",
		"xoxs-302846159270-xPk9Bm3Wv8Qt5Lz2Yh7Fc",
	}
	for _, k := range realLooking {
		t.Run(k, func(t *testing.T) {
			text := "TOKEN=" + k + " end"
			found := false
			for _, m := range Scan(text) {
				if m.Rule == "slack-token" {
					found = true
				}
			}
			assert.True(t, found, "slack-token did not match real-looking token %q", k)
		})
	}
}

// TestRejectsTrivialPEMBodies pins the private-key-block validator: PEM
// blocks whose body is too short to plausibly contain real key material are
// skipped. Agents emit these as illustrative examples ("BEGIN ... key bytes
// here ... END") and they were producing definite findings on hundreds of
// sessions.
func TestRejectsTrivialPEMBodies(t *testing.T) {
	cases := []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIBjunk\n-----END RSA PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----\n<key bytes here>\n-----END PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----\nshort\n-----END EC PRIVATE KEY-----",
	}
	for _, p := range cases {
		t.Run(p[:40], func(t *testing.T) {
			for _, m := range Scan(p) {
				assert.NotEqual(t, "private-key-block", m.Rule,
					"private-key-block matched trivial body %q", p)
			}
		})
	}
}

// TestAcceptsRealisticPEMBodies confirms the body-length gate lets through
// PEM blocks with body lengths typical of real key material.
func TestAcceptsRealisticPEMBodies(t *testing.T) {
	body := strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5)
	for _, label := range []string{"RSA", "EC", ""} {
		header := "PRIVATE KEY"
		if label != "" {
			header = label + " PRIVATE KEY"
		}
		t.Run(header, func(t *testing.T) {
			text := "-----BEGIN " + header + "-----\n" + body +
				"-----END " + header + "-----"
			found := false
			for _, m := range Scan(text) {
				if m.Rule == "private-key-block" {
					found = true
				}
			}
			assert.True(t, found, "private-key-block did not match realistic PEM (%s)", header)
		})
	}
}

// TestAcceptsPKCS8Ed25519PEM pins the lower minBody threshold: a real
// PKCS#8 Ed25519 private key has a 64-byte base64 body (`openssl
// genpkey -algorithm ED25519` output), well below the 150-byte floor
// the gate originally enforced. The body-length floor must be small
// enough to let this shape through while still rejecting the trivial
// "MIIBjunk"-style placeholders.
func TestAcceptsPKCS8Ed25519PEM(t *testing.T) {
	// Body is the literal 64-char base64 output of openssl on a real
	// Ed25519 key; the value itself is not sensitive (regenerated).
	text := "-----BEGIN PRIVATE KEY-----\n" +
		"MC4CAQAwBQYDK2VwBCIEIHNhJUCu8VvJCV4O++0jHhjsfn4SwMjf3+3zctpGdZMe\n" +
		"-----END PRIVATE KEY-----"
	found := false
	for _, m := range Scan(text) {
		if m.Rule == "private-key-block" {
			found = true
		}
	}
	assert.True(t, found, "private-key-block did not match PKCS#8 Ed25519 PEM (64-byte body)")
}

// TestAcceptsEncryptedPEMWithHeaders pins the header-skipping behavior:
// a legacy encrypted PEM private key carries a "Proc-Type"/"DEK-Info"
// header block before the body. Those lines contain non-base64
// characters (":", ",", "-") that would tank the purity ratio if
// measured; the gate must skip them and measure purity only on the
// base64 payload.
func TestAcceptsEncryptedPEMWithHeaders(t *testing.T) {
	body := strings.Repeat(
		"MIIBSECRETKEYMATERIAL0123456789ABCDEFGHIJKLMNOPQRSTUV\n", 3)
	text := "-----BEGIN RSA PRIVATE KEY-----\n" +
		"Proc-Type: 4,ENCRYPTED\n" +
		"DEK-Info: AES-128-CBC,A1B2C3D4E5F60718293A4B5C6D7E8F90\n" +
		"\n" +
		body +
		"-----END RSA PRIVATE KEY-----"
	found := false
	for _, m := range Scan(text) {
		if m.Rule == "private-key-block" {
			found = true
		}
	}
	assert.True(t, found,
		"private-key-block did not match encrypted PEM with Proc-Type/DEK-Info headers")
}

// TestAcceptsAWSKeysWithRepeatedChars pins the entropy-skip on short
// bodies: a random 16-char base32 body can plausibly have ≤10 distinct
// characters by birthday luck and fall under 3.5 bits of Shannon
// entropy. Applying entropy unconditionally would drop format-valid
// AWS keys; the gate must rely only on structural checks at this
// length.
func TestAcceptsAWSKeysWithRepeatedChars(t *testing.T) {
	// Each fixture is AKIA + 16 body chars with at most 8 distinct
	// characters (Shannon entropy ≈ 3.0 bits, below the 3.5 gate) but
	// no dominant byte, no repeating short block, and no monotone
	// alphabet/digit run.
	repeatedCharKeys := []string{
		"AKIAQQHHKKBB22NN77XX",
		"AKIAMMKKJJRRPPNN22FF",
		"ASIAFFHHKKBB22NN77XX",
	}
	for _, key := range repeatedCharKeys {
		t.Run(key, func(t *testing.T) {
			found := false
			for _, m := range Scan("key=" + key + " end") {
				if m.Rule == "aws-access-key" {
					found = true
				}
			}
			assert.True(t, found,
				"aws-access-key did not match low-entropy real-looking key %q", key)
		})
	}
}

// TestFixtureDenyListSuppressesAgentsviewFixtures pins the production
// deny-list path: when EnableFixtureDeny has been called, Scan drops matches
// whose span hashes to agentsview's own test fixtures (the values that flood
// scans of development conversations recording the test source). Tests in this
// package leave the deny off by default so they can verify positive rule paths
// against the same fixtures.
func TestFixtureDenyListSuppressesAgentsviewFixtures(t *testing.T) {
	disableFixtureDenyForTest(func(f func()) { t.Cleanup(f) })
	EnableFixtureDeny()
	t.Cleanup(func() { fixtureDenyEnabled.Store(false) })

	fixtures := []struct {
		name string
		text string
	}{
		{"aws", "key=AKIA7QHWN2DKR4FYPLJM end"},
		{"anthropic", "TOKEN=sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4 done"},
		{"slack", "TOKEN=xoxb-549271836401-fHk7Bm3Pz9Wt5Vx2Yq8Nc done"},
		{"github_pat", "tok ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg"},
		{"stripe", "key=sk_live_7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm done"},
		{"google_api", "key=AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4H end"},
		{"ed25519_pem", "-----BEGIN PRIVATE KEY-----\n" +
			"MC4CAQAwBQYDK2VwBCIEIHNhJUCu8VvJCV4O++0jHhjsfn4SwMjf3+3zctpGdZMe\n" +
			"-----END PRIVATE KEY-----"},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			for _, m := range Scan(f.text) {
				assert.NotEqual(t, ConfidenceDefinite, m.Confidence,
					"deny-list let through fixture %q: rule=%s mask=%s",
					f.text, m.Rule, m.Redacted)
			}
		})
	}
}

// TestFixtureDenyListExcludesTranscriptOnlyLeakHashes verifies that production
// fixture suppression does not hide arbitrary secret-shaped values just because
// they appeared in an agentsview development transcript. Only committed source
// fixtures belong in the hash deny-list.
func TestFixtureDenyListExcludesTranscriptOnlyLeakHashes(t *testing.T) {
	transcriptOnlyLeakHashes := []string{
		"bdcb1b818a7ba4b3c5c7c421b9c6279beb34df45f7abab1503c6d150533ad642",
		"c1589e1eece8695f238be33923fcd4cbc845b34fb792ef34f9b698beffbd5324",
		"df26a79256a73d9b7c014e9ea73372498e70a6072d22c0386ed3c6edea9817ac",
	}
	for _, h := range transcriptOnlyLeakHashes {
		_, ok := agentsviewTestFixtureHashes[h]
		assert.False(t, ok, "transcript-only leak hash %s must not be fixture-denied", h)
	}
}

// TestFixtureDenyListOffByDefault locks in the test-vs-production
// default: ordinary unit tests must see fixture matches (otherwise
// every test that uses a fixture would need a deny-disable call).
func TestFixtureDenyListOffByDefault(t *testing.T) {
	matches := Scan("key=AKIA7QHWN2DKR4FYPLJM end")
	found := false
	for _, m := range matches {
		if m.Rule == "aws-access-key" {
			found = true
		}
	}
	assert.True(t, found, "default behavior should allow fixture matches; got %+v", matches)
}

// TestRejectsPEMDocsPlaceholders pins the tighter base64-purity gate
// (≥99%) against the agentsview-docs leaks: an illustrative body with
// "...", "(2-3 lines of base64)", or string-concat operators inside
// the BEGIN/END markers is not a real key and must not be reported.
// These appeared as definite findings on the docs sessions until the
// 90% → 99% tightening.
func TestRejectsPEMDocsPlaceholders(t *testing.T) {
	cases := []string{
		"-----BEGIN PRIVATE KEY-----\n" +
			"MIGTAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBHkwdwIBAQQg...\n" +
			"(2-3 lines of base64)\n" +
			"-----END PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----\n" +
			"MIIEowIBAAKCAQEAthisisaverylongkeyblockwithlotsofbase64data\" +" + "\n" +
			"\t\t\"" +
			"-----END RSA PRIVATE KEY-----",
	}
	for i, p := range cases {
		t.Run(p[:40]+"#"+itoa(i), func(t *testing.T) {
			for _, m := range Scan(p) {
				assert.NotEqual(t, "private-key-block", m.Rule,
					"private-key-block matched docs placeholder: %q", m.Redacted)
			}
		})
	}
}

// itoa renders a small int for subtest names.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return ""
}

// TestRejectsPEMInMarkdownDiff pins the PEM base64-purity gate: a markdown
// diff containing the literal text "-----BEGIN PRIVATE KEY-----" and
// "-----END PRIVATE KEY-----" with prose, tables, and pipe characters
// between them must not produce a finding. These leak in when an agent
// helps the user edit secrets-management documentation.
func TestRejectsPEMInMarkdownDiff(t *testing.T) {
	// Body deliberately includes pipes, hyphens, and prose — the characters
	// markdown tables and bullet lists rely on but base64 doesn't permit.
	body := strings.Repeat(
		"| `APPLE_API_KEY` | `ABC123DEF0` | The 10-character Key ID |\n", 4)
	text := "-----BEGIN PRIVATE KEY-----\n" + body +
		"-----END PRIVATE KEY-----"
	for _, m := range Scan(text) {
		assert.NotEqual(t, "private-key-block", m.Rule,
			"private-key-block matched markdown diff: %q", m.Redacted)
	}
}

// TestRejectsRepeatingBlockPlaceholders pins the structural placeholder
// gate (bodyLooksRandom). Every match below is shaped like a real secret
// but is built by repeating a short seed pattern — the dominant noise
// shape across agent transcripts. None should be reported.
func TestRejectsRepeatingBlockPlaceholders(t *testing.T) {
	placeholders := map[string]string{
		"github-pat":     "ghp_" + strings.Repeat("a", 36),
		"github-pat-fg":  "github_pat_" + strings.Repeat("A1b2", 20),
		"stripe-secret":  "sk_live_" + strings.Repeat("a1B2", 8),
		"google-api-key": "AIza" + strings.Repeat("aB3_x", 7),
	}
	for name, p := range placeholders {
		t.Run(name, func(t *testing.T) {
			text := "TOKEN=" + p + " end"
			for _, m := range Scan(text) {
				if m.Rule == "github-pat" || m.Rule == "stripe-secret" ||
					m.Rule == "google-api-key" {
					assert.Failf(t, "repeating-block placeholder matched", "%s matched %q (mask=%q)",
						m.Rule, p, m.Redacted)
				}
			}
		})
	}
}

// TestRejectsSequentialRunPlaceholders pins the structural placeholder
// gate against the second-most-common shape: alphabet or digit runs
// inside a token body (xoxs-…-abcdefABCDEF012345, AKIAabcdefghijklmnop).
// The body's monotone-run detector catches these.
func TestRejectsSequentialRunPlaceholders(t *testing.T) {
	placeholders := map[string]string{
		"aws-access-key":        "AKIAZYXWVUTSRQPONMLK", // alphabet desc
		"aws-access-key-digits": "AKIA12345678ABCDEFGH", // 12345678 + ABCDEFGH
		"slack-token":           "xoxs-1234567890-abcdefABCDEF012345",
	}
	for name, p := range placeholders {
		t.Run(name, func(t *testing.T) {
			text := "TOKEN=" + p + " end"
			for _, m := range Scan(text) {
				if m.Rule == "aws-access-key" || m.Rule == "slack-token" {
					assert.Failf(t, "sequential-run placeholder matched", "%s matched %q (mask=%q)",
						m.Rule, p, m.Redacted)
				}
			}
		})
	}
}
