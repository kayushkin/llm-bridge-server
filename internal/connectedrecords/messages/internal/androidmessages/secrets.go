package androidmessages

import "regexp"

// A login code: a code word near a run of 4 to 8 digits (or, after the word,
// capitals with two digits or more), either way round, across lines.
var (
	codeWord   = `\b(?:code|passcode|pin|otp|one[- ]time|verification|verify|2fa|two[- ]factor|security code|login|log in|sign[- ]in)\b`
	loginCode  = regexp.MustCompile(`(?i)` + codeWord + `[\s\S]{0,80}?(?:\b\d[\d -]{2,8}\d\b|\b(?-i:[A-Z0-9]*\d[A-Z0-9]*\d[A-Z0-9]*)\b)|\b\d{4,8}\b[\s\S]{0,80}?` + codeWord)
	passwordIs = regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|pw|wifi pass(?:word)?)\b\s*(?:is|:|=|-)\s*\S{4,}`)
)

// SecretKind says what a message's text is when it should be left out of a
// transcript: "a login code", "a password", or "" when it is neither.
func SecretKind(text string) string {
	switch {
	case passwordIs.MatchString(text):
		return "a password"
	case loginCode.MatchString(text):
		return "a login code"
	}
	return ""
}
