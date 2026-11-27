package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// vcardProperty is one content line of a vCard, unfolded: "item1.TEL;TYPE=CELL:+1 202 555 0100"
// is group "item1", name "TEL", params {"TYPE": ["CELL"]}, raw value "+1 202 555 0100".
type vcardProperty struct {
	Group  string
	Name   string
	Params map[string][]string
	Value  string
}

// vcard is one BEGIN:VCARD … END:VCARD block.
type vcard struct {
	Version    string
	Properties []vcardProperty
}

// parseVCards reads every card in r. It accepts versions 3.0 and 4.0, which
// both write UTF-8 text with backslash escapes. A 2.1 card, which may carry
// quoted-printable and another charset, is refused rather than misread.
func parseVCards(r io.Reader) ([]vcard, error) {
	lines, err := unfoldLines(r)
	if err != nil {
		return nil, err
	}
	var cards []vcard
	var current *vcard
	for number, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		property, err := parseContentLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number+1, err)
		}
		switch {
		case property.Name == "BEGIN" && strings.EqualFold(property.Value, "VCARD"):
			if current != nil {
				return nil, fmt.Errorf("line %d: BEGIN:VCARD inside a card", number+1)
			}
			current = &vcard{}
		case property.Name == "END" && strings.EqualFold(property.Value, "VCARD"):
			if current == nil {
				return nil, fmt.Errorf("line %d: END:VCARD outside a card", number+1)
			}
			if current.Version != "3.0" && current.Version != "4.0" {
				return nil, fmt.Errorf("line %d: vCard version %q: only 3.0 and 4.0 are read", number+1, current.Version)
			}
			cards = append(cards, *current)
			current = nil
		case current == nil:
			return nil, fmt.Errorf("line %d: %s outside a card", number+1, property.Name)
		case property.Name == "VERSION":
			current.Version = strings.TrimSpace(property.Value)
		default:
			current.Properties = append(current.Properties, property)
		}
	}
	if current != nil {
		return nil, fmt.Errorf("the last card has no END:VCARD")
	}
	return cards, nil
}

// unfoldLines splits r into content lines, joining a line that starts with a
// space or tab onto the one before it (RFC 6350 section 3.2).
func unfoldLines(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var lines []string
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += line[1:]
			continue
		}
		lines = append(lines, line)
	}
	return lines, scanner.Err()
}

// parseContentLine splits "group.NAME;PARAM=a,b:value". The value keeps its
// escapes; textValue and structuredValue undo them.
func parseContentLine(line string) (vcardProperty, error) {
	colon := indexOutsideQuotes(line, ':')
	if colon < 0 {
		return vcardProperty{}, fmt.Errorf("no ':' in %q", line)
	}
	head, value := line[:colon], line[colon+1:]
	parts := splitOutsideQuotes(head, ';')
	property := vcardProperty{Params: map[string][]string{}, Value: value}
	name := parts[0]
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		property.Group, name = name[:dot], name[dot+1:]
	}
	property.Name = strings.ToUpper(strings.TrimSpace(name))
	if property.Name == "" {
		return vcardProperty{}, fmt.Errorf("no property name in %q", line)
	}
	for _, parameter := range parts[1:] {
		key, values, found := strings.Cut(parameter, "=")
		key = strings.ToUpper(strings.TrimSpace(key))
		if !found {
			// vCard 2.1 writes a bare type: "TEL;CELL:…".
			property.Params["TYPE"] = append(property.Params["TYPE"], key)
			continue
		}
		for _, one := range splitOutsideQuotes(values, ',') {
			property.Params[key] = append(property.Params[key], strings.Trim(one, `"`))
		}
	}
	if encodings := property.Params["ENCODING"]; len(encodings) > 0 && property.Name != "PHOTO" && property.Name != "LOGO" && property.Name != "SOUND" && property.Name != "KEY" {
		return vcardProperty{}, fmt.Errorf("%s has ENCODING=%s, which this reader does not decode", property.Name, strings.Join(encodings, ","))
	}
	return property, nil
}

func indexOutsideQuotes(text string, separator byte) int {
	quoted := false
	for index := 0; index < len(text); index++ {
		switch text[index] {
		case '"':
			quoted = !quoted
		case separator:
			if !quoted {
				return index
			}
		}
	}
	return -1
}

func splitOutsideQuotes(text string, separator byte) []string {
	var parts []string
	for {
		index := indexOutsideQuotes(text, separator)
		if index < 0 {
			return append(parts, text)
		}
		parts = append(parts, text[:index])
		text = text[index+1:]
	}
}

// textValue undoes the escapes of a TEXT value: \n, \, \; and \\.
func textValue(raw string) string {
	var out strings.Builder
	for index := 0; index < len(raw); index++ {
		if raw[index] == '\\' && index+1 < len(raw) {
			index++
			switch raw[index] {
			case 'n', 'N':
				out.WriteByte('\n')
			default:
				out.WriteByte(raw[index])
			}
			continue
		}
		out.WriteByte(raw[index])
	}
	return out.String()
}

// structuredValue splits a value on unescaped separators (';' for N and ORG,
// ',' for NICKNAME and CATEGORIES) and unescapes each part.
func structuredValue(raw string, separator byte) []string {
	var parts []string
	var current strings.Builder
	for index := 0; index < len(raw); index++ {
		if raw[index] == '\\' && index+1 < len(raw) {
			current.WriteByte(raw[index])
			current.WriteByte(raw[index+1])
			index++
			continue
		}
		if raw[index] == separator {
			parts = append(parts, textValue(current.String()))
			current.Reset()
			continue
		}
		current.WriteByte(raw[index])
	}
	return append(parts, textValue(current.String()))
}
