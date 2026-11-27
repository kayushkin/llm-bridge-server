// Package takeout reads Google Takeout's records of what someone did online:
// My Activity's HTML pages, YouTube's watch and search history (the same
// HTML), Chrome's History.json and Chrome's Bookmarks.html. It turns each
// record into an activitystore.ImportActivity and decides nothing else.
package takeout

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// zoneOffsets are the time-zone abbreviations My Activity ends a time with,
// as fixed offsets in hours. Takeout writes every time in the zone the
// account had when the export was made, with that moment's abbreviation, and
// applies the abbreviation's offset to every date in the file, summer or
// winter: a file exported in summer says PDT on a January search, and means
// UTC-7. So an abbreviation is read as its fixed offset, never as a zone with
// daylight saving. An abbreviation two zones share (IST) is refused.
var zoneOffsets = map[string]float64{
	"UTC": 0, "GMT": 0, "WET": 0, "WEST": 1, "BST": 1, "CET": 1, "CEST": 2, "EET": 2, "EEST": 3, "MSK": 3,
	"JST": 9, "KST": 9, "AEST": 10, "AEDT": 11, "NZST": 12, "NZDT": 13,
	"HST": -10, "AKST": -9, "AKDT": -8, "PST": -8, "PDT": -7, "MST": -7, "MDT": -6, "CST": -6, "CDT": -5, "EST": -5, "EDT": -4,
	"AST": -4, "ADT": -3, "NST": -3.5, "NDT": -2.5,
}

var numericZone = regexp.MustCompile(`^(?:GMT|UTC)([+-])(\d{1,2})(?::?(\d{2}))?$`)

// myActivityTime matches a whole My Activity time: "Oct 26, 2026, 7:01:10 PM
// PDT", with a narrow or plain space before AM or PM.
var myActivityTime = regexp.MustCompile(`^[A-Z][a-z]{2,3} \d{1,2}, \d{4}, \d{1,2}:\d{2}:\d{2} [AP]M [A-Z]{2,5}(?:[+-]\d{1,2}(?::?\d{2})?)?$`)

func normalizeSpaces(text string) string {
	text = strings.NewReplacer(" ", " ", " ", " ", " ", " ").Replace(text)
	return strings.Join(strings.Fields(text), " ")
}

// IsMyActivityTime says whether a line of a card is its time.
func IsMyActivityTime(text string) bool {
	return myActivityTime.MatchString(normalizeSpaces(text))
}

// zoneOffsetSeconds reads a zone abbreviation or GMT+h:mm as seconds east of
// UTC.
func zoneOffsetSeconds(zone string) (int64, error) {
	if hours, found := zoneOffsets[zone]; found {
		return int64(hours * 3600), nil
	}
	if match := numericZone.FindStringSubmatch(zone); match != nil {
		hours, _ := strconv.Atoi(match[2])
		minutes := 0
		if match[3] != "" {
			minutes, _ = strconv.Atoi(match[3])
		}
		offset := int64(hours*3600 + minutes*60)
		if match[1] == "-" {
			offset = -offset
		}
		return offset, nil
	}
	return 0, fmt.Errorf("time zone %q is not one this reader knows; add its fixed offset to zoneOffsets", zone)
}

// ParseMyActivityTime reads a My Activity time as epoch seconds, UTC.
func ParseMyActivityTime(text string) (int64, error) {
	normalized := normalizeSpaces(text)
	if !myActivityTime.MatchString(normalized) {
		return 0, fmt.Errorf("%q is not a My Activity time", text)
	}
	cut := strings.LastIndex(normalized, " ")
	clock, zone := normalized[:cut], normalized[cut+1:]
	offset, err := zoneOffsetSeconds(zone)
	if err != nil {
		return 0, err
	}
	clock = strings.Replace(clock, "Sept ", "Sep ", 1)
	local, err := time.ParseInLocation("Jan 2, 2006, 3:04:05 PM", clock, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", text, err)
	}
	return local.Unix() - offset, nil
}
