package textutil

// OrNone is text, or "none" when it is empty, for a message that names a
// value which may be missing.
func OrNone(text string) string {
	if text == "" {
		return "none"
	}
	return text
}
