package filing

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/secrets"
)

// MetadataFingerprint is the Kata issue metadata key that ties an issue to a
// pattern. Filing looks it up before creating, so a pattern filed from any
// archive or by hand earlier maps to one issue.
const MetadataFingerprint = "friction.fingerprint"

// issueContext is what the issue says about the pattern beyond its signal.
type issueContext struct {
	Date      string
	PublicURL string
	Pattern   db.FrictionPattern
	// sessionURL links to the signal's session; buildIssue sets it from the
	// unredacted subject.
	sessionURL string
}

// buildIssue is the create request for sig. The title and body come from a
// copy of sig whose text fields passed through redact, which masks secrets
// and home directories before they leave the machine. Redacting the whole
// fields first keeps truncation and Markdown quoting from hiding a secret
// from the scanner; text a detector already cut loses the end the cut may
// have split.
func buildIssue(sig friction.Signal, ic issueContext) kata.CreateIssue {
	md := map[string]any{
		MetadataFingerprint:      sig.Fingerprint(),
		"friction.kind":          string(sig.Kind),
		"friction.rules_version": friction.RulesVersion,
	}
	if sig.SubjectKind != friction.SubjectDiagnostic {
		ic.sessionURL = friction.SessionURL(ic.PublicURL, sig.SubjectID, sig.Ordinal)
		if ic.sessionURL != "" {
			md["agentsview.session_url"] = ic.sessionURL
		}
	}
	shown := sig
	shown.Text = shareableText(sig.Kind, sig.Text)
	for _, f := range []*string{&shown.SubjectID, &shown.ToolName, &shown.Label, &shown.Evidence} {
		*f = redact(*f)
	}
	return kata.CreateIssue{
		Title:    shown.Title(),
		Body:     issueBody(shown, ic),
		Priority: priority(sig.Kind),
		Labels:   []string{"friction", "friction:" + string(sig.Kind)},
		Metadata: md,
		// Diagnostic identities are distinct by construction, so Kata's
		// look-alike check would only block them.
		ForceNew: sig.SubjectKind == friction.SubjectDiagnostic,
	}
}

// priority ranks direct evidence of wasted work above single failures.
func priority(k friction.Kind) int {
	switch k {
	case friction.KindCorrection, friction.KindPattern:
		return 2
	default:
		return 3
	}
}

func issueBody(sig friction.Signal, ic issueContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Found by the AgentsView Friction Log on %s.\n\n", ic.Date)
	if sig.SubjectKind == friction.SubjectDiagnostic {
		fmt.Fprintf(&b, "- Source: %s\n", oneLine(sig.SubjectID))
	} else {
		fmt.Fprintf(&b, "- Session: %s\n", oneLine(sig.SubjectID))
		if ic.sessionURL != "" {
			fmt.Fprintf(&b, "- Open in AgentsView: %s\n", ic.sessionURL)
		}
	}
	if u := friction.DigestURL(ic.PublicURL, ic.Date); u != "" {
		fmt.Fprintf(&b, "- Digest: %s\n", u)
	}
	if p := ic.Pattern; p.OccurrenceCount > 0 {
		fmt.Fprintf(&b, "- Seen %d times in %d sessions since %s\n",
			p.OccurrenceCount, p.SessionCount, p.FirstSeenDate)
	}
	b.WriteString("\n")
	b.WriteString(detail(sig))
	b.WriteString("\n")
	return b.String()
}

func detail(sig friction.Signal) string {
	switch sig.Kind {
	case friction.KindError:
		return "Tool `" + oneLine(sig.ToolName) + "` failed:\n\n" + quote(sig.Text)
	case friction.KindWorkaround:
		return "Workaround (" + oneLine(sig.Label) + "):\n\n" + quote(sig.Text)
	case friction.KindDeferral:
		return "The agent put work off: \"" + oneLine(sig.Label) + "\"."
	case friction.KindPattern:
		return sig.Text + "\n\nEvidence: " + oneLine(sig.Evidence)
	default:
		return quote(sig.Text)
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func quote(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

var pemFooterRe = regexp.MustCompile(`-----END [^\n-]*-----`)

// shareableText redacts a signal's text. Text a detector cut short is
// redacted first, so every secret the cut left whole is masked, and then
// loses the end the cut may have split: a key block without its complete
// END line, or else the last token.
func shareableText(k friction.Kind, text string) string {
	head, cut := friction.CutByDetector(k, text)
	if !cut {
		return redact(text)
	}
	head = redact(head)
	if i := strings.LastIndex(head, "-----BEGIN "); i >= 0 && !pemFooterRe.MatchString(head[i:]) {
		head = head[:i]
	} else {
		head = head[:max(strings.LastIndexFunc(head, unicode.IsSpace), 0)]
	}
	return strings.TrimRightFunc(head, unicode.IsSpace) + friction.TruncatedMarker
}

var homePathRe = regexp.MustCompile(`(?:/Users|/home)/[^/\s'"` + "`" + `]+|[A-Za-z]:\\Users\\[^\\\s'"]+`)

// escapedKeyRe finds a private key block whose line breaks are written as
// \n escapes, as a key inside a JSON string is.
var escapedKeyRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)

var keyEscapes = strings.NewReplacer(`\r\n`, "\n", `\n`, "\n", `\r`, "\n")

// redact masks secrets with the secret scanner and shortens home directories
// to "~", so issue text carries neither. The scanner rejects a key block
// whose line breaks are escaped, so such a block is decoded and masked when
// the decoded block is a key.
func redact(s string) string {
	s = escapedKeyRe.ReplaceAllStringFunc(s, func(block string) string {
		if !strings.Contains(block, `\n`) && !strings.Contains(block, `\r`) {
			return block
		}
		decoded := keyEscapes.Replace(block)
		if masked := secrets.Redact(decoded); masked != decoded {
			return masked
		}
		return block
	})
	return homePathRe.ReplaceAllString(secrets.Redact(s), "~")
}
