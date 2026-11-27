package osmextract

import (
	"regexp"
	"strings"
)

// streetWordAbbreviations writes the usual words of US street names the way
// both a calendar and OpenStreetMap might: "Street" and "St" are one key.
var streetWordAbbreviations = map[string]string{
	"street": "st", "avenue": "ave", "av": "ave", "boulevard": "blvd", "drive": "dr", "road": "rd", "lane": "ln", "court": "ct",
	"place": "pl", "parkway": "pkwy", "highway": "hwy", "circle": "cir", "terrace": "ter", "square": "sq", "trail": "trl",
	"north": "n", "south": "s", "east": "e", "west": "w", "northeast": "ne", "northwest": "nw", "southeast": "se", "southwest": "sw",
}

func normalizedWords(text string) []string {
	var builder strings.Builder
	for _, character := range strings.ToLower(text) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character > 127 {
			builder.WriteRune(character)
		} else {
			builder.WriteRune(' ')
		}
	}
	return strings.Fields(builder.String())
}

func streetAddressKey(houseNumber, street string) string {
	words := normalizedWords(street)
	for position, word := range words {
		if short, found := streetWordAbbreviations[word]; found {
			words[position] = short
		}
	}
	return strings.ToLower(strings.TrimSpace(houseNumber)) + " " + strings.Join(words, " ")
}

var (
	// streetLinePattern is a part of a location that starts with a house
	// number: "500 E Example Ln".
	streetLinePattern = regexp.MustCompile(`^\s*(\d+[A-Za-z]?)\s+(.+?)\s*$`)
	// unitPattern is what follows the street: "#5", "Suite 100", "Ste B".
	unitPattern = regexp.MustCompile(`(?i)\s+(#\s*\S+|(suite|ste|unit|apt|room|rm|bldg|building|floor|fl)\.?\s+\S+)\s*$`)
	// locationSeparators split a location into its parts.
	locationSeparators = strings.NewReplacer(";", ",", "(", ",", ")", ",", "\n", ",")
)

// Geocoded is a location whose street address was found in an extract.
type Geocoded struct {
	Address Address
	// Venue is what came before the street address: "Some Hall" of "Some
	// Hall, 1 Main St, Town".
	Venue string
}

// Geocode finds a location's street address among the extract's addresses,
// offline. The first part that starts with a house number is the street
// line; of the addresses with that number and street, those whose town is
// named in the location are kept, and the answer stands only when what is
// left lies within agreeMeters of each other: one building, not two towns'
// Main Streets. ok is false when nothing, or nothing single, is found.
func (extract *Extract) Geocode(location string, agreeMeters float64) (Geocoded, bool) {
	var parts []string
	for _, part := range strings.Split(locationSeparators.Replace(location), ",") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	for position, part := range parts {
		match := streetLinePattern.FindStringSubmatch(unitPattern.ReplaceAllString(part, ""))
		if match == nil {
			continue
		}
		candidates := extract.byStreetAddress[streetAddressKey(match[1], match[2])]
		if len(candidates) == 0 {
			continue
		}
		rest := " " + strings.Join(normalizedWords(strings.Join(parts[position+1:], " ")), " ") + " "
		var inTown []int
		for _, candidate := range candidates {
			city := strings.Join(normalizedWords(extract.Addresses[candidate].City), " ")
			if city != "" && strings.Contains(rest, " "+city+" ") {
				inTown = append(inTown, candidate)
			}
		}
		if len(inTown) > 0 {
			candidates = inTown
		}
		first := extract.Addresses[candidates[0]]
		for _, candidate := range candidates[1:] {
			other := extract.Addresses[candidate]
			if distance(first.Latitude, first.Longitude, other.Latitude, other.Longitude) > agreeMeters {
				return Geocoded{}, false
			}
		}
		return Geocoded{Address: first, Venue: strings.Join(parts[:position], ", ")}, true
	}
	return Geocoded{}, false
}
