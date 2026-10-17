package db

import (
	"regexp"
	"strings"
)

// imageToken matches an image as stored. Browser sync cannot download image
// files, so it stores "[image unavailable]"; a ZIP export stores a reference
// to the copied file.
var imageToken = regexp.MustCompile(`!?\[image unavailable\]|!\[image\]\([^)]*\)`)

func isImagePlaceholder(token string) bool {
	return strings.Contains(token, "image unavailable")
}

// ReconcileImageContent compares archived and incoming message text. Text
// outside image tokens must match exactly. Two image tokens match when they
// name the same image, or when either is a placeholder: a placeholder archived
// for a concrete reference is filled (fill), and a placeholder incoming for an
// archived reference keeps the archived reference. Two different concrete
// references do not match.
func ReconcileImageContent(archived, incoming string) (same, fill bool) {
	aTokens := imageToken.FindAllStringIndex(archived, -1)
	bTokens := imageToken.FindAllStringIndex(incoming, -1)
	if len(aTokens) != len(bTokens) {
		return false, false
	}
	aPos, bPos := 0, 0
	for i := range aTokens {
		if archived[aPos:aTokens[i][0]] != incoming[bPos:bTokens[i][0]] {
			return false, false
		}
		a := archived[aTokens[i][0]:aTokens[i][1]]
		b := incoming[bTokens[i][0]:bTokens[i][1]]
		aPos, bPos = aTokens[i][1], bTokens[i][1]
		aPlaceholder, bPlaceholder := isImagePlaceholder(a), isImagePlaceholder(b)
		switch {
		case a == b, aPlaceholder && bPlaceholder, !aPlaceholder && bPlaceholder:
		case aPlaceholder && !bPlaceholder:
			fill = true
		default:
			return false, false
		}
	}
	if archived[aPos:] != incoming[bPos:] {
		return false, false
	}
	return true, fill
}

// MergeImageContent returns the archived text with each image placeholder
// that incoming replaces by a concrete reference filled in. Every other
// archived image is kept, so an upgrade in one place does not discard an
// archived reference elsewhere in the same message. ok is false when the
// texts do not reconcile.
func MergeImageContent(archived, incoming string) (string, bool) {
	if same, _ := ReconcileImageContent(archived, incoming); !same {
		return "", false
	}
	aTokens := imageToken.FindAllStringIndex(archived, -1)
	bTokens := imageToken.FindAllStringIndex(incoming, -1)
	var out strings.Builder
	aPos := 0
	for i := range aTokens {
		out.WriteString(archived[aPos:aTokens[i][0]])
		a := archived[aTokens[i][0]:aTokens[i][1]]
		b := incoming[bTokens[i][0]:bTokens[i][1]]
		if isImagePlaceholder(a) && !isImagePlaceholder(b) {
			out.WriteString(b)
		} else {
			out.WriteString(a)
		}
		aPos = aTokens[i][1]
	}
	out.WriteString(archived[aPos:])
	return out.String(), true
}

// ExtendImageContent reconciles archived text with the longer incoming text
// that completes it. Image tokens inside the archived text are matched as in
// ReconcileImageContent and merged as in MergeImageContent, and whatever the
// incoming text adds after the archived text is appended. This covers a
// message that gained a concrete image and more text in the same update. ok is
// false when the archived text is empty or the texts do not reconcile.
func ExtendImageContent(archived, incoming string) (string, bool) {
	if archived == "" {
		return "", false
	}
	aTokens := imageToken.FindAllStringIndex(archived, -1)
	bTokens := imageToken.FindAllStringIndex(incoming, -1)
	if len(bTokens) < len(aTokens) {
		return "", false
	}
	var out strings.Builder
	aPos, bPos := 0, 0
	for i := range aTokens {
		if archived[aPos:aTokens[i][0]] != incoming[bPos:bTokens[i][0]] {
			return "", false
		}
		out.WriteString(archived[aPos:aTokens[i][0]])
		a := archived[aTokens[i][0]:aTokens[i][1]]
		b := incoming[bTokens[i][0]:bTokens[i][1]]
		aPos, bPos = aTokens[i][1], bTokens[i][1]
		aPlaceholder, bPlaceholder := isImagePlaceholder(a), isImagePlaceholder(b)
		switch {
		case a == b, aPlaceholder && bPlaceholder, !aPlaceholder && bPlaceholder:
			out.WriteString(a)
		case aPlaceholder && !bPlaceholder:
			out.WriteString(b)
		default:
			return "", false
		}
	}
	tail, rest := archived[aPos:], incoming[bPos:]
	if !strings.HasPrefix(rest, tail) {
		return "", false
	}
	out.WriteString(rest)
	return out.String(), true
}

// IsImagePlaceholderUpgrade reports whether incoming differs from stored only
// by replacing image placeholders with concrete image references.
func IsImagePlaceholderUpgrade(stored, incoming string) bool {
	same, fill := ReconcileImageContent(stored, incoming)
	return same && fill
}
