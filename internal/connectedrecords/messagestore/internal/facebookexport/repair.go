// Package facebookexport reads the parts of a Facebook data export (JSON
// format) that more than one importer needs.
package facebookexport

import "unicode/utf8"

// Repair undoes the export's encoding: Facebook writes UTF-8 bytes as if
// each were a Latin-1 character. Text that is not in that shape is returned
// as it is.
func Repair(text string) string {
	raw := make([]byte, 0, len(text))
	for _, character := range text {
		if character > 0xFF {
			return text
		}
		raw = append(raw, byte(character))
	}
	repaired := string(raw)
	if !utf8.ValidString(repaired) {
		return text
	}
	return repaired
}
