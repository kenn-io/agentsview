package filing

import (
	"fmt"
	"hash/fnv"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	LabelBase, LabelRecurred = "friction", "friction:recurred"
	RetryLookbackCapDays     = 14
	MaxLastErrorBytes        = 1024
	maxIdempotencyKeyRunes   = 240
	maxBackoff               = 24 * time.Hour
)

// RunContext carries the digest date and URLs into bodies. ForceNew is the
// explicit "file a new issue anyway" request (friction file --force-new).
type RunContext struct {
	Date, DigestURL, PublicURL, RunID string
	ForceNew                          bool
	// InlineLedger is set only while the caller already holds the review lock.
	InlineLedger bool
}

// IdempotencyKey is jilog's slug (trackers/kata.rs idempotency_key): every
// Unicode whitespace rune becomes '-', clamped to 240 runes.
func IdempotencyKey(title string) string {
	out := make([]rune, 0, len(title))
	for _, r := range title {
		if len(out) == maxIdempotencyKeyRunes {
			break
		}
		if unicode.IsSpace(r) {
			r = '-'
		}
		out = append(out, r)
	}
	return string(out)
}

// Priority ports signal_priority (errors are unreviewed, so P3) and adds the
// agentsview kinds (spec §6.8).
func Priority(k friction.Kind) int {
	switch k {
	case friction.KindCorrection, friction.KindPattern, friction.KindFrustration:
		return 2
	default:
		return 3
	}
}

func Labels(k friction.Kind) []string { return []string{LabelBase, LabelBase + ":" + string(k)} }

// ForceNew bypasses Kata's look-alike gate only for diagnostic subjects,
// whose identities are distinct by construction (spec §13.3).
func ForceNew(sig friction.Signal) bool { return sig.SubjectKind == friction.SubjectDiagnostic }

func DefaultKinds() []friction.Kind {
	return []friction.Kind{friction.KindCorrection, friction.KindError, friction.KindWorkaround, friction.KindDeferral, friction.KindPattern}
}

func oneLine(s string) string { return strings.NewReplacer("\n", " ", "\r", " ").Replace(s) }

func kindSpecific(sig friction.Signal) string {
	switch sig.Kind {
	case friction.KindError:
		return "Tool: " + sig.ToolName + "\nMessage: " + sig.Text
	case friction.KindWorkaround:
		return "Pattern: " + sig.Label + "\nContext: " + sig.Text
	case friction.KindDeferral:
		return sig.Label
	case friction.KindInterruption:
		if sig.Ordinal != nil {
			return fmt.Sprintf("User interrupted the agent at message %d.", *sig.Ordinal)
		}
		return "User interrupted the agent."
	default: // correction, pattern, frustration
		return sig.Text
	}
}

// Body adapts jilog's build_body (trackers/kata.rs:564-606). The digest-path
// line becomes URL lines built only from public_url.
func Body(sig friction.Signal, run RunContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Detected by agentsview Friction Log on %s.\n\n## Source\n", run.Date)
	fmt.Fprintf(&b, "- Session: %s\n- Kind: %s\n", sig.SubjectID, sig.Kind)
	if sig.Dims.Seat != "" {
		fmt.Fprintf(&b, "- Seat: %s\n", oneLine(sig.Dims.Seat))
	}
	if sig.Dims.Agent != "" {
		fmt.Fprintf(&b, "- Agent: %s\n", oneLine(sig.Dims.Agent))
	}
	if sig.Dims.Machine != "" {
		fmt.Fprintf(&b, "- Machine: %s\n", oneLine(sig.Dims.Machine))
	}
	if sig.SubjectKind != friction.SubjectDiagnostic {
		if u := SessionURL(run.PublicURL, sig.SubjectID, sig.Ordinal); u != "" {
			fmt.Fprintf(&b, "- Link: %s\n", u)
		}
	}
	if run.DigestURL != "" {
		fmt.Fprintf(&b, "- Digest: %s\n", run.DigestURL)
	}
	b.WriteString("\n## Signal\n")
	b.WriteString(kindSpecific(sig))
	return b.String()
}

func publicBase(publicURL string) (*url.URL, bool) {
	if strings.TrimSpace(publicURL) == "" {
		return nil, false
	}
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, false
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u, true
}

// SessionURL mirrors the browser router (servicehttp sessionWebURL,
// internal/servicehttp/http.go:1422-1443): the agent prefix and the opaque
// id are separate path segments.
func SessionURL(publicURL, sessionID string, ordinal *int) string {
	base, ok := publicBase(publicURL)
	if !ok || sessionID == "" {
		return ""
	}
	prefix, rest, found := strings.Cut(sessionID, ":")
	path := url.PathEscape(prefix)
	if found {
		path += "/" + url.PathEscape(rest)
	}
	out := strings.TrimRight(base.String(), "/") + "/sessions/" + path
	if ordinal != nil {
		out += fmt.Sprintf("?msg=%d", *ordinal)
	}
	return out
}

// DigestURL is the canonical digest page (spec §9.2), or "" without public_url.
// Deliberately unlike review.DigestURL ("friction:<date>" for summary JSON): issue bodies omit the Digest line when public_url is unset (spec §13.3).
func DigestURL(publicURL, date string) string {
	base, ok := publicBase(publicURL)
	if !ok {
		return ""
	}
	return strings.TrimRight(base.String(), "/") + "/friction/" + date
}

var homePathRe = regexp.MustCompile(`(?:/Users|/home)/[^/\s'"` + "`" + `]+|[A-Za-z]:\\Users\\[^\\\s'"]+`)

// DefaultRedact masks secrets with the secret_findings scanner and contracts
// home-directory prefixes to "~" so issue text carries no absolute home paths.
func DefaultRedact(s string) string {
	return homePathRe.ReplaceAllString(secrets.Redact(s), "~")
}

// Metadata is the create-time metadata (spec §13.3).
func Metadata(sig friction.Signal, run RunContext, instance string) map[string]any {
	m := map[string]any{
		"friction.fingerprint":   sig.Fingerprint(),
		"friction.kind":          string(sig.Kind),
		"friction.rules_version": friction.RulesVersion,
		"agentsview.session_id":  sig.SubjectID,
		"agentsview.instance":    instance,
	}
	if sig.SubjectKind != friction.SubjectDiagnostic {
		if u := SessionURL(run.PublicURL, sig.SubjectID, sig.Ordinal); u != "" {
			m["agentsview.session_url"] = u
		}
	}
	return m
}

// Backoff is 1h, 2h, 4h … capped at 24h, plus a deterministic per-fingerprint
// jitter below 10% so a burst of failures spreads out.
func Backoff(attempts int, fingerprint string) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	base := maxBackoff
	if attempts <= 5 {
		base = time.Hour << (attempts - 1)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(fingerprint))
	return base + time.Duration(h.Sum64()%uint64(base/10))
}

func boundError(s string) string { return stringutil.SafeTruncate(s, MaxLastErrorBytes) }
