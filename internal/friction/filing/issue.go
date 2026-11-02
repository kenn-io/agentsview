package filing

import (
	"fmt"
	"regexp"
	"strings"

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
// from the scanner.
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
	for _, f := range []*string{&shown.SubjectID, &shown.Text, &shown.ToolName, &shown.Label, &shown.Evidence} {
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
	fmt.Fprintf(&b, "- Kind: %s\n", sig.Kind)
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
		return "The agent called this a " + oneLine(sig.Label) + ":\n\n" + quote(sig.Text)
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

var homePathRe = regexp.MustCompile(`(?:/Users|/home)/[^/\s'"` + "`" + `]+|[A-Za-z]:\\Users\\[^\\\s'"]+`)

// redact masks secrets with the secret scanner and shortens home directories
// to "~", so issue text carries neither.
func redact(s string) string {
	return homePathRe.ReplaceAllString(secrets.Redact(s), "~")
}
