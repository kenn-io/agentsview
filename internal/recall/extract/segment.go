// Package extract derives distillation work units from session transcripts.
//
// A Segmenter turns a session's messages into an ordered list of units, each
// destined for one model call. Derivation must be deterministic: resume
// cursors index into the unit list, and entry identity embeds the unit index,
// so replaying a session after a restart or upgrade must yield the same units
// in the same order. A segmenter's name and parameters identify its output
// alongside the prompt and model configuration, so different segmentation
// strategies build separate, comparable corpora instead of mixing.
package extract

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// PromptRole names the kind of prompt a unit should be distilled with.
type PromptRole string

const (
	// RoleIntent marks a unit carrying what the user asked for.
	RoleIntent PromptRole = "intent"
	// RoleAction marks a unit carrying what the assistant did.
	RoleAction PromptRole = "action"
	// RoleGeneric marks a unit for strategies that do not distinguish
	// speaker roles and use a single prompt.
	RoleGeneric PromptRole = "generic"
)

// Message is the minimal transcript row a segmenter consumes. Callers adapt
// their storage rows to it; ordinals must be the session's message ordinals
// so unit evidence ranges point back into the transcript.
type Message struct {
	Ordinal  int
	Role     string
	Content  string
	IsSystem bool
}

// Unit is one model call's worth of transcript, with the ordinal range it
// covers as evidence provenance.
type Unit struct {
	Role         PromptRole
	Text         string
	OrdinalStart int
	OrdinalEnd   int
}

// Segmenter derives units from a session deterministically. Name and Params
// identify the strategy and its knobs; both become part of the extraction
// configuration's identity. PromptRoles declares which prompt kinds the
// strategy emits so prompt resolution can be validated up front.
//
// Versioning contract: any change that alters the derived units for a
// transcript whose units could previously commit MUST change Name or
// Params, so the generation fingerprint changes and the corpus rebuilds
// under a new identity instead of mixing derivations. A change confined to
// units that could never commit (for example ranges the evidence window
// refuses) keeps committed output derivation-identical: the sequential
// cursor cannot pass an uncommittable unit, so affected sessions hold no
// entries, and their next visit re-derives a different units digest and
// rebuilds from scratch.
type Segmenter interface {
	Name() string
	Params() map[string]any
	PromptRoles() []PromptRole
	Units(messages []Message) []Unit
}

// TurnsV1 segments at user/assistant turn granularity: each non-system user
// message becomes one intent unit, and each run of assistant messages between
// user messages is packed into action units of at most MaxWindowChars
// characters (counted as Unicode code points). A single assistant message
// larger than the budget becomes its own oversized unit; the extraction
// client is responsible for splitting text that exceeds the model context.
type TurnsV1 struct {
	MaxWindowChars int
}

// Name implements Segmenter.
func (TurnsV1) Name() string { return "turns-v1" }

// Params implements Segmenter.
func (s TurnsV1) Params() map[string]any {
	return map[string]any{"max_window_chars": s.MaxWindowChars}
}

// PromptRoles implements Segmenter.
func (TurnsV1) PromptRoles() []PromptRole {
	return []PromptRole{RoleIntent, RoleAction}
}

// Units implements Segmenter.
// VisibleContents returns the trimmed content of each message that becomes
// model-visible unit content, in transcript order. The outbound secret scan
// aggregates exactly these so it sees what the endpoint does — system rows,
// unsupported roles, and empty rows are dropped from both the units and the
// scan through the shared visibleContent predicate.
func VisibleContents(messages []Message) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		if content, ok := visibleContent(message); ok {
			out = append(out, content)
		}
	}
	return out
}

// visibleContent reports whether message contributes model-visible unit
// content and returns its trimmed text. Units and VisibleContents share it so
// the segmenter and the outbound scan cannot disagree about what is sent. It
// does not decide run contiguity: a skipped row still occupies its ordinal,
// which Units tracks separately.
func visibleContent(message Message) (string, bool) {
	if message.IsSystem {
		return "", false
	}
	if message.Role != "user" && message.Role != "assistant" {
		return "", false
	}
	content := strings.TrimSpace(message.Content)
	if content == "" {
		return "", false
	}
	return content, true
}

func (s TurnsV1) Units(messages []Message) []Unit {
	var units []Unit
	var run []ordinalBlock
	prevOrdinal := -1
	for _, message := range messages {
		// Ingest filtering can drop rows after ordinals are assigned, so
		// the stored transcript may skip ordinals. Evidence provenance
		// requires gap-free ranges, so no unit may span a missing row:
		// flush the current run at every discontinuity. Skipped system and
		// empty rows below still occupy their ordinals, so they keep a run
		// contiguous. This split stays within the versioning contract
		// above: it only changes units that spanned a missing ordinal,
		// which the evidence window always refused to commit.
		if prevOrdinal >= 0 && message.Ordinal != prevOrdinal+1 {
			units = packRun(run, s.MaxWindowChars, units)
			run = nil
		}
		prevOrdinal = message.Ordinal
		content, ok := visibleContent(message)
		if !ok {
			continue
		}
		switch message.Role {
		case "user":
			units = packRun(run, s.MaxWindowChars, units)
			run = nil
			units = append(units, Unit{
				Role: RoleIntent,
				Text: fmt.Sprintf("USER MESSAGE (ordinal %d):\n%s",
					message.Ordinal, content),
				OrdinalStart: message.Ordinal,
				OrdinalEnd:   message.Ordinal,
			})
		case "assistant":
			run = append(run, ordinalBlock{
				ordinal: message.Ordinal,
				text: fmt.Sprintf("[%d] ASSISTANT:\n%s",
					message.Ordinal, content),
			})
		}
	}
	return packRun(run, s.MaxWindowChars, units)
}

type ordinalBlock struct {
	ordinal int
	text    string
}

// packRun appends action units built from consecutive assistant blocks,
// starting a new unit whenever adding a block would exceed maxChars. Length
// is counted in code points so the boundary decisions match regardless of
// how the text is encoded.
func packRun(blocks []ordinalBlock, maxChars int, units []Unit) []Unit {
	var current []ordinalBlock
	currentChars := 0
	flush := func() {
		if len(current) == 0 {
			return
		}
		texts := make([]string, 0, len(current))
		for _, block := range current {
			texts = append(texts, block.text)
		}
		units = append(units, Unit{
			Role:         RoleAction,
			Text:         strings.Join(texts, "\n\n"),
			OrdinalStart: current[0].ordinal,
			OrdinalEnd:   current[len(current)-1].ordinal,
		})
	}
	for _, block := range blocks {
		blockChars := utf8.RuneCountInString(block.text)
		if len(current) > 0 && currentChars+blockChars > maxChars {
			flush()
			current = nil
			currentChars = 0
		}
		current = append(current, block)
		currentChars += blockChars + 2
	}
	flush()
	return units
}
