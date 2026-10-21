package db

// DialogueFingerprintValues preserves message positions and NULL versus empty
// dialogue. It returns nil when every value is NULL so callers can omit the
// field and preserve fingerprints from before dialogue was included.
func DialogueFingerprintValues(messages []Message) []*string {
	var dialogue []*string
	for i, m := range messages {
		if m.DialogueText != nil {
			if dialogue == nil {
				dialogue = make([]*string, len(messages))
			}
			dialogue[i] = m.DialogueText
		}
	}
	return dialogue
}
