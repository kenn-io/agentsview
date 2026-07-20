package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// rule is one secret detector. group selects a capture group as the
// reported span (0 = whole match); validate optionally gates a match
// (nil = always keep); mask renders the persisted/displayed redaction.
type rule struct {
	name       string
	confidence string
	prefilters []string // literal anchors; empty => always scan
	re         *regexp.Regexp
	group      int
	validate   func(string) bool
	mask       func(string) string
}

var rules = []rule{
	{
		name:       "aws-access-key",
		confidence: ConfidenceDefinite,
		prefilters: []string{"AKIA", "ASIA"},
		re:         regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		validate:   notAWSPlaceholder,
		mask:       func(s string) string { return maskKeepEnds(s, 4, 4) },
	},
	{
		name:       "anthropic-key",
		confidence: ConfidenceDefinite,
		prefilters: []string{"sk-ant-"},
		re:         regexp.MustCompile(`\bsk-ant-[0-9A-Za-z][0-9A-Za-z_\-]{18,}`),
		validate:   notAnthropicKeyPlaceholder,
		mask:       func(s string) string { return maskKeepEnds(s, 7, 4) },
	},
	{
		// openai-key: matches OpenAI's project (sk-proj-), service-account
		// (sk-svcacct-), and admin (sk-admin-) key prefixes. These prefix
		// patterns are scanner knowledge derived from observed token format;
		// no clean upstream announcement URL is cited because none was found.
		// Legacy sk- + 48-char keys are deliberately not matched (no anchor
		// that doesn't collide with sk-ant-).
		name:       "openai-key",
		confidence: ConfidenceDefinite,
		prefilters: []string{"sk-proj-", "sk-svcacct-", "sk-admin-"},
		re: regexp.MustCompile(
			`\b(sk-(?:proj|svcacct|admin)-[0-9A-Za-z_\-]{20,})` +
				`(?:[^0-9A-Za-z_\-]|$)`),
		group:    1,
		validate: notOpenAIKeyPlaceholder,
		mask:     maskOpenAIKey,
	},
	{
		// gitlab-pat: GitLab personal/project access tokens. Prefix per
		// https://docs.gitlab.com/security/tokens/token_prefixes/ — glpat-
		// is documented as the PAT prefix; minimum body length is 20.
		name:       "gitlab-pat",
		confidence: ConfidenceDefinite,
		prefilters: []string{"glpat-"},
		re: regexp.MustCompile(
			`\b(glpat-[0-9A-Za-z_\-]{20,})(?:[^0-9A-Za-z_\-]|$)`),
		group:    1,
		validate: notGitLabPATPlaceholder,
		mask:     func(s string) string { return maskKeepEnds(s, 6, 4) },
	},
	{
		// npm-token: npm access tokens since the 2021 format change. Per
		// https://github.blog/2021-09-23-announcing-npms-new-access-token-format/
		// the format is npm_ + 36 base62 chars (30 base62 payload + 6 base62
		// CRC). Legacy classic tokens (bare 36-char hex, no prefix) are
		// deliberately not matched — they would need a separate rule with no
		// anchor and a high false-positive rate.
		name:       "npm-token",
		confidence: ConfidenceDefinite,
		prefilters: []string{"npm_"},
		re:         regexp.MustCompile(`\bnpm_[0-9A-Za-z]{36}\b`),
		validate:   notNPMTokenPlaceholder,
		mask:       func(s string) string { return maskKeepEnds(s, 4, 4) },
	},
	{
		// pypi-token: PyPI API tokens. Per https://docs.pypi.org/api/secrets/
		// the documented format is pypi-[A-Za-z0-9_-]{85,}. The literal
		// "AgEIcHlwaS5vcmcC" is the base64 of the macaroon protocol-version
		// and URL bytes that every PyPI token starts with — a much stronger
		// anchor than just pypi-. The 16-char anchor counts toward the
		// 85-char minimum, so the variable tail must be ≥69 chars.
		name:       "pypi-token",
		confidence: ConfidenceDefinite,
		prefilters: []string{"pypi-"},
		re: regexp.MustCompile(
			`\b(pypi-AgEIcHlwaS5vcmcC[0-9A-Za-z_\-]{69,})` +
				`(?:[^0-9A-Za-z_\-]|$)`),
		group:    1,
		validate: notPyPITokenPlaceholder,
		mask:     func(s string) string { return maskKeepEnds(s, 21, 4) },
	},
	{
		// huggingface-token: Hugging Face access tokens. Prefix hf_ +
		// base62 body, ≥30 chars empirically (token lengths vary by
		// generation, but observed minimum is well above 30).
		name:       "huggingface-token",
		confidence: ConfidenceDefinite,
		prefilters: []string{"hf_"},
		re:         regexp.MustCompile(`\bhf_[0-9A-Za-z]{30,}\b`),
		validate:   notHuggingFaceTokenPlaceholder,
		mask:       func(s string) string { return maskKeepEnds(s, 3, 4) },
	},
	{
		// sendgrid-key: SendGrid API keys. Format SG.<22 base64url>.<43
		// base64url> — two fixed-length segments separated by a literal '.'.
		name:       "sendgrid-key",
		confidence: ConfidenceDefinite,
		prefilters: []string{"SG."},
		re: regexp.MustCompile(
			`\b(SG\.[0-9A-Za-z_\-]{22}\.[0-9A-Za-z_\-]{43})` +
				`(?:[^0-9A-Za-z_\-]|$)`),
		group:    1,
		validate: notSendGridKeyPlaceholder,
		mask:     func(s string) string { return maskKeepEnds(s, 3, 4) },
	},
	{
		name:       "basic-auth-url",
		confidence: ConfidenceCandidate,
		prefilters: []string{"://"},
		re:         regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@]+:([^\s:/@]+)@[^\s/]+`),
		group:      1, // mask the password, not the whole URL
		mask:       func(s string) string { return maskKeepEnds(s, 0, 0) },
	},
	{
		name:       "github-pat",
		confidence: ConfidenceDefinite,
		prefilters: []string{"ghp_", "github_pat_"},
		re:         regexp.MustCompile(`\b(?:ghp_[0-9A-Za-z]{36}|github_pat_[0-9A-Za-z_]{40,})\b`),
		validate:   notGitHubPATPlaceholder,
		mask:       func(s string) string { return maskKeepEnds(s, 4, 4) },
	},
	{
		name:       "slack-token",
		confidence: ConfidenceDefinite,
		prefilters: []string{"xoxb-", "xoxa-", "xoxp-", "xoxr-", "xoxs-"},
		re:         regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z]{10,}(?:-[0-9A-Za-z]+)*`),
		validate:   notSlackTokenPlaceholder,
		mask:       func(s string) string { return maskKeepEnds(s, 5, 4) },
	},
	{
		name:       "stripe-secret",
		confidence: ConfidenceDefinite,
		prefilters: []string{"sk_live_", "rk_live_"},
		re:         regexp.MustCompile(`\b[sr]k_live_[0-9A-Za-z]{16,}\b`),
		validate:   notStripeSecretPlaceholder,
		mask:       func(s string) string { return maskKeepEnds(s, 8, 4) },
	},
	{
		name:       "google-api-key",
		confidence: ConfidenceDefinite,
		prefilters: []string{"AIza"},
		// Capture the key in group 1 and require a non-body terminator (or
		// end of text) rather than a trailing \b, so keys ending in '-'
		// still match instead of being silently skipped.
		re:       regexp.MustCompile(`\b(AIza[0-9A-Za-z_\-]{35})(?:[^0-9A-Za-z_\-]|$)`),
		group:    1,
		validate: notGoogleAPIKeyPlaceholder,
		mask:     func(s string) string { return maskKeepEnds(s, 4, 4) },
	},
	{
		name:       "private-key-block",
		confidence: ConfidenceDefinite,
		prefilters: []string{"-----BEGIN"},
		re: regexp.MustCompile(
			`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		validate: notTrivialPEMBody,
		mask:     func(string) string { return "[redacted private key block]" },
	},
	{
		name:       "jwt",
		confidence: ConfidenceCandidate,
		prefilters: []string{"eyJ"},
		re: regexp.MustCompile(
			`\beyJ[0-9A-Za-z_\-]+\.[0-9A-Za-z_\-]+\.[0-9A-Za-z_\-]+`),
		mask: func(s string) string { return maskKeepEnds(s, 3, 0) },
	},
	{
		// Known false positives: filesystem paths and URLs can clear the
		// entropy gate because the value charset includes '/'. Accepted for a
		// candidate rule; do not lower the threshold to chase these.
		name:       "high-entropy-assignment",
		confidence: ConfidenceCandidate,
		prefilters: []string{"=", ":"},
		re: regexp.MustCompile(
			`(?i)\b[a-z][a-z0-9_]{2,}\s*[=:]\s*['"]?([A-Za-z0-9+/_\-]{20,}={0,2})['"]?`),
		group:    1,
		validate: highEntropyValue,
		mask:     func(s string) string { return maskKeepEnds(s, 0, 4) },
	},
}

// definiteRules is the well-anchored vendor-format subset of rules, computed
// once at load. ScanDefinite uses it for the fast inline-sync path.
var definiteRules = filterByConfidence(rules, ConfidenceDefinite)

func filterByConfidence(rs []rule, confidence string) []rule {
	out := make([]rule, 0, len(rs))
	for _, r := range rs {
		if r.confidence == confidence {
			out = append(out, r)
		}
	}
	return out
}

// shannonEntropy returns the per-byte Shannon entropy (bits) of s.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// highEntropyValue gates the high-entropy-assignment rule: the value must
// be long enough and random-looking to plausibly be a secret. Trailing '='
// padding is trimmed before the length and entropy checks so padding bytes
// cannot inflate the length gate or dilute entropy.
func highEntropyValue(s string) bool {
	body := strings.TrimRight(s, "=")
	return len(body) >= 20 && shannonEntropy(body) >= 3.5
}

// hasRepeatingBlock reports whether s is dominated by a short repeating
// seed pattern. This catches the dominant placeholder shapes that leak
// from agent transcripts: a single byte repeating ("aaaa…"), a short
// block repeating ("A1b2A1b2…", "aB3_xaB3_x…"). Real high-entropy
// secrets don't have this structure.
//
// Block size 1 is checked by single-byte frequency: any byte that
// covers ≥75% of s is a hit. Block sizes 2..6 try the leading block at
// each phase alignment; placeholders are typically unsalted repeats of
// the seed so phase 0 catches the well-known shapes. The 75% coverage
// threshold tolerates a stray salt character (a single byte slipped in
// or trimmed off) without losing detection.
func hasRepeatingBlock(s string) bool {
	n := len(s)
	if n < 8 {
		return false
	}
	var freq [256]int
	for i := range n {
		freq[s[i]]++
	}
	for _, f := range freq {
		if f*4 >= n*3 {
			return true
		}
	}
	for blockSize := 2; blockSize <= 6 && blockSize*3 <= n; blockSize++ {
		for phase := 0; phase < blockSize && phase+blockSize <= n; phase++ {
			block := s[phase : phase+blockSize]
			coverage := 0
			for i := phase; i+blockSize <= n; i += blockSize {
				if s[i:i+blockSize] == block {
					coverage += blockSize
				}
			}
			if coverage*4 >= n*3 {
				return true
			}
		}
	}
	return false
}

// hasMonotoneRun reports whether s contains a run of >= minRun bytes
// that step monotonically by +1 or -1 in ASCII codepoint. Placeholders
// are typically built from alphabet/digit runs ("abcdef", "1234567890",
// "ZYXWVU", "fedcba"); real secrets are random and don't produce these.
func hasMonotoneRun(s string, minRun int) bool {
	if len(s) < minRun {
		return false
	}
	run := 1
	var dir int
	for i := 1; i < len(s); i++ {
		d := int(s[i]) - int(s[i-1])
		if d != 1 && d != -1 {
			run = 1
			dir = 0
			continue
		}
		if dir == 0 || dir == d {
			dir = d
			run++
			if run >= minRun {
				return true
			}
		} else {
			run = 2
			dir = d
		}
	}
	return false
}

// bodyLooksRandom reports whether s plausibly contains real key
// material. It runs the cheap structural checks (Shannon entropy,
// repeating short blocks, sequential alphabet/digit runs) and rejects
// strings that fail any one. Each rule's validator strips the
// well-known vendor prefix (e.g. "ghp_") before calling so the fixed
// prefix doesn't drag entropy down or hide a body-level pattern.
//
// Callers with short fixed-length bodies (AWS access keys at 16 chars)
// should not use this function: Shannon entropy ≥3.5 is too tight a
// gate at 16 chars and would reject real random bodies that land
// ≤10 distinct chars by birthday luck. Those validators call
// bodyHasNoPlaceholderShape directly so only the structural checks
// (which scale to any length) apply.
func bodyLooksRandom(s string) bool {
	if shannonEntropy(s) < 3.5 {
		return false
	}
	return bodyHasNoPlaceholderShape(s)
}

// bodyHasNoPlaceholderShape runs the structural checks
// (hasRepeatingBlock, hasMonotoneRun) without the Shannon entropy
// gate. AWS validators use it because their body is exactly 16 chars
// and the entropy threshold would over-reject; every other definite
// rule has a body ≥19 chars where bodyLooksRandom's entropy check is
// still a useful filter against placeholders that pass the structural
// checks.
func bodyHasNoPlaceholderShape(s string) bool {
	if hasRepeatingBlock(s) {
		return false
	}
	if hasMonotoneRun(s, 6) {
		return false
	}
	return true
}

// notAWSPlaceholder rejects AWS access key IDs that look like
// placeholders. It first dismisses the canonical AWS-docs body
// ("IOSFODNN7EXAMPLE" and EXAMPL[0-9] variants) by substring, then runs
// the structural checks on the 16-char body that follows AKIA/ASIA.
// The 6–8 char EXAMPL/IOSFODNN markers have a vanishing chance of
// occurring in a real 16-char body (~3.5e-13 for IOSFODNN in random
// uppercase), so the substring gate is safe for a definite rule.
//
// Shannon entropy is deliberately not applied: a random 16-char
// base32 body can have ≤10 distinct chars by birthday luck and fall
// below 3.5 bits.
func notAWSPlaceholder(s string) bool {
	if strings.Contains(s, "EXAMPL") || strings.Contains(s, "IOSFODNN") {
		return false
	}
	body := strings.TrimPrefix(s, "AKIA")
	body = strings.TrimPrefix(body, "ASIA")
	return bodyHasNoPlaceholderShape(body)
}

// notAnthropicKeyPlaceholder rejects Anthropic API keys whose body
// (after "sk-ant-") fails structural checks: a 4-rune trailing repeat
// (sk-ant-...AAAA), a single byte dominating the body, a repeating
// short block, or a long sequential ASCII run.
func notAnthropicKeyPlaceholder(s string) bool {
	if !notTrailingRunRepeat(s, 4) {
		return false
	}
	return bodyLooksRandom(strings.TrimPrefix(s, "sk-ant-"))
}

// notOpenAIKeyPlaceholder rejects OpenAI keys whose body (after the
// matched sk-proj- / sk-svcacct- / sk-admin- prefix) fails the structural
// checks or shows a trailing-run repeat.
func notOpenAIKeyPlaceholder(s string) bool {
	if !notTrailingRunRepeat(s, 4) {
		return false
	}
	body := s
	for _, p := range []string{"sk-svcacct-", "sk-admin-", "sk-proj-"} {
		if after, ok := strings.CutPrefix(body, p); ok {
			body = after
			break
		}
	}
	return bodyLooksRandom(body)
}

// maskOpenAIKey preserves the matched OpenAI prefix length when redacting.
// sk-proj- (8), sk-admin- (9), and sk-svcacct- (11) differ in length, so a
// fixed offset would truncate the longer prefixes mid-word in the display.
// Longest-first ordering is defensive in case a future prefix shares a
// shorter prefix with a current one (no such case exists today).
func maskOpenAIKey(s string) string {
	for _, p := range []string{"sk-svcacct-", "sk-admin-", "sk-proj-"} {
		if strings.HasPrefix(s, p) {
			return maskKeepEnds(s, len(p), 4)
		}
	}
	panic("openai-key: regex matched without known prefix; rule and " +
		"maskOpenAIKey are out of sync")
}

// notGitLabPATPlaceholder rejects GitLab PATs whose body (after "glpat-")
// fails structural checks or shows a trailing-run repeat. Catches the
// placeholder shapes agents emit when illustrating PAT format:
// "glpat-AAAA…", "glpat-Xa9Kd…AAAA".
func notGitLabPATPlaceholder(s string) bool {
	if !notTrailingRunRepeat(s, 4) {
		return false
	}
	return bodyLooksRandom(strings.TrimPrefix(s, "glpat-"))
}

// notNPMTokenPlaceholder rejects npm tokens whose body fails structural
// checks or shows a trailing-run repeat. The fixed 36-char body uses
// bodyHasNoPlaceholderShape (no entropy gate) like AWS's 16-char body —
// a long enough random base62 body can legitimately have ≤10 distinct
// chars by birthday luck and fall below 3.5 bits.
func notNPMTokenPlaceholder(s string) bool {
	if !notTrailingRunRepeat(s, 4) {
		return false
	}
	return bodyHasNoPlaceholderShape(strings.TrimPrefix(s, "npm_"))
}

// notPyPITokenPlaceholder rejects PyPI tokens whose macaroon payload
// (after the full pypi-AgEIcHlwaS5vcmcC anchor) fails structural checks
// or shows a trailing-run repeat. Catches placeholder shapes agents emit:
// "pypi-AgEI…AAAA", "pypi-…AAAA".
func notPyPITokenPlaceholder(s string) bool {
	if !notTrailingRunRepeat(s, 4) {
		return false
	}
	return bodyLooksRandom(strings.TrimPrefix(s, "pypi-AgEIcHlwaS5vcmcC"))
}

// notHuggingFaceTokenPlaceholder rejects HF tokens whose body fails
// structural checks or shows a trailing-run repeat. Catches placeholder
// shapes agents emit: "hf_AAAA…", "hf_Xa9Kd…AAAA".
func notHuggingFaceTokenPlaceholder(s string) bool {
	if !notTrailingRunRepeat(s, 4) {
		return false
	}
	return bodyLooksRandom(strings.TrimPrefix(s, "hf_"))
}

// notSendGridKeyPlaceholder rejects SendGrid keys whose segments fail
// structural checks. The 22-char identifier uses bodyHasNoPlaceholderShape
// (short body, like AWS); the 43-char secret uses notTrailingRunRepeat +
// bodyLooksRandom to catch the same trailing-run placeholder shape that
// affects the other variable-length-body definite-tier validators.
func notSendGridKeyPlaceholder(s string) bool {
	body := strings.TrimPrefix(s, "SG.")
	ident, secret, ok := strings.Cut(body, ".")
	if !ok {
		return false
	}
	if !bodyHasNoPlaceholderShape(ident) {
		return false
	}
	if !notTrailingRunRepeat(secret, 4) {
		return false
	}
	return bodyLooksRandom(secret)
}

// notGitHubPATPlaceholder rejects GitHub PATs whose body (after "ghp_"
// or "github_pat_") fails structural checks. Catches the noisy shapes
// agents emit when illustrating PAT format: "ghp_aaaa…",
// "github_pat_A1b2A1b2…".
func notGitHubPATPlaceholder(s string) bool {
	body := strings.TrimPrefix(s, "ghp_")
	body = strings.TrimPrefix(body, "github_pat_")
	return bodyLooksRandom(body)
}

// notStripeSecretPlaceholder rejects Stripe live secrets whose body
// fails structural checks. Strips the "sk_live_" / "rk_live_" prefix
// first so it doesn't anchor a coverage check on the fixed prefix.
func notStripeSecretPlaceholder(s string) bool {
	body := strings.TrimPrefix(s, "sk_live_")
	body = strings.TrimPrefix(body, "rk_live_")
	return bodyLooksRandom(body)
}

// notGoogleAPIKeyPlaceholder rejects Google API keys whose body (after
// "AIza") fails structural checks. Catches the "AIza" + "aB3_xaB3_x…"
// pattern agents emit when explaining the key format.
func notGoogleAPIKeyPlaceholder(s string) bool {
	return bodyLooksRandom(strings.TrimPrefix(s, "AIza"))
}

// notSlackTokenPlaceholder rejects Slack tokens that look like the
// well-known placeholder forms. It dismisses the canonical "0123"
// suffix (the Slack docs convention), then runs the structural checks
// on the part after the "xox?-" prefix so the workspace ID and token
// body are both subject to the repeating-block and sequential-run
// detectors.
func notSlackTokenPlaceholder(s string) bool {
	if strings.HasSuffix(s, "0123") {
		return false
	}
	if !notTrailingRunRepeat(s, 4) {
		return false
	}
	body := s
	if len(body) >= 5 && strings.HasPrefix(body, "xox") {
		body = body[4:] // strip "xox?-"
	}
	return bodyLooksRandom(body)
}

// notTrailingRunRepeat rejects strings whose final n characters are a
// single repeated byte (...AAAA, ...0000). A 4-byte tail repeat has
// roughly (1/62)^3 ≈ 4e-6 odds of occurring naturally in a random
// base62 secret — acceptable false-negative loss for a rule that
// otherwise produces noise on every transcript quoting a doc
// placeholder.
func notTrailingRunRepeat(s string, n int) bool {
	if len(s) < n {
		return true
	}
	last := s[len(s)-1]
	for i := len(s) - 2; i >= len(s)-n; i-- {
		if s[i] != last {
			return true
		}
	}
	return false
}

// notTrivialPEMBody rejects PEM blocks whose payload is too short or
// too far from base64 to plausibly hold a real private key. The
// shortest real key material is PKCS#8 Ed25519, whose body is a
// single 64-character base64 line; the gate requires 48 non-whitespace
// payload bytes (leaving margin) AND ≥99% of them to be valid base64
// alphabet. The byte-length gate catches the "BEGIN ... key bytes
// here ... END" examples agents emit; the base64 purity gate catches
// docs/diffs that quote the BEGIN/END markers around prose or
// hand-written placeholders (".../(2-3 lines of base64)", "+" string
// concatenation, etc.). A real PEM body after header stripping is
// pure base64, so the threshold is set just below 100% to leave
// margin for whitespace handling edge cases without admitting any
// stray punctuation.
//
// Legacy encrypted PEM keys carry a "Proc-Type"/"DEK-Info" header
// block before the body; those lines contain ":", ",", and "-" which
// would tank the purity ratio if they were measured. The gate skips
// the header block (everything up to the first blank line) when one
// is present so it's measured only over the actual base64 payload.
func notTrivialPEMBody(s string) bool {
	const minBody = 48
	const beginMarker = "-----"
	first := strings.Index(s, beginMarker)
	if first < 0 {
		return true
	}
	headerEnd := strings.Index(s[first+len(beginMarker):], beginMarker)
	if headerEnd < 0 {
		return true
	}
	bodyStart := first + len(beginMarker) + headerEnd + len(beginMarker)
	endIdx := strings.LastIndex(s, "-----END")
	if endIdx <= bodyStart {
		return true
	}
	payload := stripPEMHeaders(s[bodyStart:endIdx])
	nonWS, b64 := 0, 0
	for _, c := range payload {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		nonWS++
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			b64++
		}
	}
	if nonWS < minBody {
		return false
	}
	return b64*100 >= nonWS*99
}

// stripPEMHeaders removes the legacy encrypted-PEM header block
// ("Proc-Type: 4,ENCRYPTED" / "DEK-Info: AES-128-CBC,...") from the
// start of body, returning the base64 payload. The header block is a
// sequence of "Name: Value" lines terminated by a blank line; modern
// PKCS#8 encrypted keys have no headers, so the function is a no-op
// in that case. A markdown diff that happens to wrap BEGIN/END
// markers around prose does not start with a "Name: Value" line, so
// stripPEMHeaders leaves it alone (its purity is measured over the
// full content and fails the 90% gate on its own).
func stripPEMHeaders(body string) string {
	body = strings.TrimLeft(body, " \t\r\n")
	if !looksLikePEMHeaderStart(body) {
		return body
	}
	if _, after, ok := strings.Cut(body, "\n\n"); ok {
		return after
	}
	if _, after, ok := strings.Cut(body, "\r\n\r\n"); ok {
		return after
	}
	return body
}

// looksLikePEMHeaderStart reports whether body begins with a "Name:"
// header line in the form RFC 1421 allows: letters and hyphens before
// the colon, on the first line of the body.
func looksLikePEMHeaderStart(body string) bool {
	eol := strings.IndexAny(body, "\r\n")
	if eol < 0 {
		eol = len(body)
	}
	colon := strings.Index(body[:eol], ":")
	if colon <= 0 {
		return false
	}
	for i := range colon {
		c := body[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c != '-' {
			return false
		}
	}
	return true
}

// rulesAlgorithmVersion bumps when matching *logic* changes (e.g. a new
// validate gate). Pattern edits are detected automatically because the
// regexes are folded into the hash; validate functions are not, so adding
// a new gate must bump this constant by hand. Mirrors db.ClassifierHash.
//
// v2: added doc-placeholder and trailing-run-repeat validators
// (AKIA*EXAMPL*, sk-ant-…AAAA, trivial PEM bodies, xoxb-…0123).
//
// v3: added structural bodyLooksRandom check (entropy + repeating
// blocks + sequential ASCII runs) and wired it into every definite
// rule, including github-pat, stripe-secret, and google-api-key which
// previously had no validator. Also tightened the PEM gate to require
// base64-purity of the body and lowered minBody to 48 so PKCS#8
// Ed25519 (64-char body) is still detected; the gate skips legacy
// "Proc-Type"/"DEK-Info" header lines before measuring purity. Shannon
// entropy is only applied to bodies ≥24 bytes via bodyLooksRandom;
// AWS validators call bodyHasNoPlaceholderShape so the 16-char base32
// body doesn't trip the gate by birthday-paradox luck.
//
// v4: added agentsviewTestFixtures deny-list. Scan and ScanDefinite drop
// matches from agentsview's own test files, so a development conversation that
// recorded a fixture doesn't report it as a definite leak on subsequent scans.
//
// v5: store and match fixture deny-list entries by SHA-256 hash and include
// those hashes in RulesVersion without committing the raw secret-shaped values.
//
// v6: added six well-anchored vendor rules (openai-key, gitlab-pat,
// npm-token, pypi-token, huggingface-token, sendgrid-key); widened the
// high-entropy-assignment capture group to include trailing base64 '='
// padding so the reported span matches the literal value, and updated
// highEntropyValue to trim trailing '=' before measuring entropy/length
// so the padding doesn't push a borderline body below the 3.5-bit floor.
//
// v7: no matching change — the scan stamp's meaning strengthened. Transcript
// mutations now revoke the stamp in the same write, so a current stamp
// attests the scan saw the entire current transcript (recall extraction's
// privacy boundary depends on this). Stamps written by earlier binaries can
// cover less than they claim when an append's deferred rescan failed or was
// interrupted, and they can arrive later from machines still running old
// binaries, so they must read as stale by value everywhere; a one-time
// local migration could not invalidate either case.
const rulesAlgorithmVersion = 7

// Verify reports whether the named rule still produces a finding at exactly
// [start:end) within source. Used by --reveal to confirm a stored finding's
// coordinates still resolve to the same secret before printing a full value.
// It re-runs the canonical Scan (the same function that produces findings, so
// overlap suppression is applied identically) and matches grouped rules by
// their captured-group span.
func Verify(ruleName, source string, start, end int) bool {
	if start < 0 || end > len(source) || start >= end {
		return false
	}
	for _, m := range Scan(source) {
		if m.Rule == ruleName && m.Start == start && m.End == end {
			return true
		}
	}
	return false
}

// RulesVersion is a stable hex SHA-256 over the algorithm version and the full
// ruleset (names, confidences, regexes, prefilters). It is the version a full
// Scan stamps. It changes when the ruleset changes, so persisted findings can
// be invalidated and rescanned.
func RulesVersion() string {
	return rulesVersion("full", rules)
}

// DefiniteRulesVersion is the version stamped by the definite-only inline-sync
// scan. It is deliberately distinct from RulesVersion (a "definite" scope tag,
// plus a hash over only the definite rules) so secrets scan --backfill — which
// treats RulesVersion as current — re-scans sessions that received only the
// fast inline scan, letting an explicit scan add candidate findings. A later
// inline resync re-stamps this version, dropping those candidates by design.
func DefiniteRulesVersion() string {
	return rulesVersion("definite", definiteRules)
}

// ActiveRulesVersions are the persisted scan versions that the current binary
// considers fresh for listing. Full scans stamp RulesVersion; inline sync scans
// stamp DefiniteRulesVersion. Both incorporate the fixture deny-list hashes.
func ActiveRulesVersions() []string {
	full := RulesVersion()
	definite := DefiniteRulesVersion()
	if full == definite {
		return []string{full}
	}
	return []string{full, definite}
}

// rulesVersion hashes the algorithm version, a scope tag, and the given rules.
// The scope tag guarantees the full and definite versions never collide even
// if their rule lists were identical.
func rulesVersion(scope string, rs []rule) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\n%s\n", rulesAlgorithmVersion, scope)
	for i := range rs {
		r := &rs[i]
		fmt.Fprintf(h, "R\x1f%s\x1f%s\x1f%d\x1f%s\n",
			r.name, r.confidence, r.group, r.re.String())
		for _, p := range r.prefilters {
			fmt.Fprintf(h, "P\x1f%s\n", p)
		}
	}
	for _, fixtureHash := range sortedAgentsviewFixtureHashes() {
		fmt.Fprintf(h, "F\x1f%s\n", fixtureHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}
