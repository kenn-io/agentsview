// Package nanoclaw resolves NanoClaw cell sessions to their persona and
// channel, applies the cell's trust filter, and removes NanoClaw's message
// envelopes. It ports the parts of jilog's NanoclawReader
// (crates/jilog-review/src/readers/nanoclaw.rs at 9e8e094, MIT) that still
// apply once agentsview has archived the transcripts as Claude sessions.
package nanoclaw

import (
	"regexp"
	"strings"
)

// messageRE is nanoclaw.rs:595 verbatim. Go's \b is ASCII-only, which is
// exact here: it follows the ASCII tag name "message".
var messageRE = regexp.MustCompile(`(?s)<message\b[^>]*>(.*?)</message>`)

// xmlEntities is xml_unescape's order (nanoclaw.rs:609-616): &amp; last, so
// "&amp;lt;" becomes "&lt;" and not "<". The replacements run one after
// another like Rust's chained replace; strings.Replacer would not.
var xmlEntities = [][2]string{
	{"&lt;", "<"},
	{"&gt;", ">"},
	{"&quot;", `"`},
	{"&#39;", "'"},
	{"&apos;", "'"},
	{"&amp;", "&"},
}

func xmlUnescape(s string) string {
	for _, e := range xmlEntities {
		s = strings.ReplaceAll(s, e[0], e[1])
	}
	return s
}

// UnwrapEnvelope extracts the human text from a NanoClaw envelope: the
// bodies of every <message …>…</message> element, each trimmed and
// XML-unescaped, empty ones dropped, joined with "\n". Input without a
// <message> element is returned trimmed (canary pings, plain prompts).
func UnwrapEnvelope(s string) string {
	matches := messageRE.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return strings.TrimSpace(s)
	}
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		if text := xmlUnescape(strings.TrimSpace(m[1])); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}
