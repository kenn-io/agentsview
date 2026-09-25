package friction

import (
	"crypto/sha256"
	"encoding/hex"
)

const titleContextRunes = 80

// Title is the deterministic issue title and fingerprint input.
// Text and labels are limited to 80 runes without an ellipsis.
func (s Signal) Title() string {
	switch s.Kind {
	case KindCorrection:
		return "[friction/correction] " + s.SubjectID + ": " + TruncateRunes(s.Text, titleContextRunes)
	case KindError:
		return "[friction/error] " + s.ToolName + ": " + TruncateRunes(s.Text, titleContextRunes)
	case KindWorkaround:
		return "[friction/workaround] " + s.Label + ": " + TruncateRunes(s.Text, titleContextRunes)
	case KindPattern:
		return "[friction/pattern] " + s.SubjectID + ": " + TruncateRunes(s.Text, titleContextRunes)
	case KindDeferral:
		return "[friction/deferral] " + s.SubjectID + ": " + TruncateRunes(s.Label, titleContextRunes)
	case KindFrustration:
		return "[friction/frustration] " + s.SubjectID + ": " + TruncateRunes(s.Text, titleContextRunes)
	case KindInterruption:
		return "[friction/interruption] " + s.SubjectID
	default:
		return "[friction/" + string(s.Kind) + "] " + s.SubjectID
	}
}

// Fingerprint is the full SHA-256 hash of the deterministic title.
func (s Signal) Fingerprint() string {
	sum := sha256.Sum256([]byte(s.Title()))
	return "fl1:" + hex.EncodeToString(sum[:])
}
