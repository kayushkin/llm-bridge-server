// Package contentquery reads the text `adb shell content query --uri …`
// prints: one record per "Row: N " line, each "column=value" pair joined by
// ", ", NULL for a null.
//
// A value may hold ", " and newlines, so a pair boundary is not simply ", ".
// Every row of one query has the same columns in the same order, so this
// reader learns the column list once (the list most rows agree on) and then
// cuts each row at ", <next column>=" in turn.
package contentquery

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Row is one record: column to value. A NULL column is absent.
type Row map[string]string

// Get is the column's value, "" when it is NULL.
func (row Row) Get(column string) string { return row[column] }

var rowStart = regexp.MustCompile(`(?m)^Row: \d+ `)

var columnToken = regexp.MustCompile(`(?:^|, )([A-Za-z_][A-Za-z0-9_]*)=`)

// ReadFile reads one content query output file.
func ReadFile(path string) ([]Row, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rows, err := Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rows, nil
}

// Parse reads the text of one query. Text with no rows is no rows (a table
// the phone had empty prints "No result found.").
func Parse(text string) ([]Row, error) {
	records := splitRecords(text)
	if len(records) == 0 {
		return nil, nil
	}
	columns := agreedColumns(records)
	rows := make([]Row, 0, len(records))
	for index, record := range records {
		row, err := cut(record, columns)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", index, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// splitRecords is each record's text after its "Row: N " prefix, without the
// line break that ends it.
func splitRecords(text string) []string {
	starts := rowStart.FindAllStringIndex(text, -1)
	records := make([]string, 0, len(starts))
	for index, start := range starts {
		end := len(text)
		if index+1 < len(starts) {
			end = starts[index+1][0]
		}
		records = append(records, strings.TrimRight(text[start[1]:end], "\r\n"))
	}
	return records
}

// agreedColumns is the column list most records show. A record whose value
// happens to hold ", name=" shows one column too many, and is outvoted.
func agreedColumns(records []string) []string {
	votes := map[string]int{}
	lists := map[string][]string{}
	best := ""
	for _, record := range records {
		var columns []string
		for _, match := range columnToken.FindAllStringSubmatch(record, -1) {
			columns = append(columns, match[1])
		}
		key := strings.Join(columns, ",")
		votes[key]++
		lists[key] = columns
		if votes[key] > votes[best] {
			best = key
		}
	}
	return lists[best]
}

// cut splits one record by the known columns, in order: each value runs to
// the first ", <next column>=" after it.
func cut(record string, columns []string) (Row, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("no columns")
	}
	if !strings.HasPrefix(record, columns[0]+"=") {
		return nil, fmt.Errorf("does not start with %s=", columns[0])
	}
	row := Row{}
	position := len(columns[0]) + 1
	for index, column := range columns {
		end := len(record)
		next := ""
		if index+1 < len(columns) {
			next = ", " + columns[index+1] + "="
			found := strings.Index(record[position:], next)
			if found < 0 {
				return nil, fmt.Errorf("no %q after column %s", next, column)
			}
			end = position + found
		}
		if value := record[position:end]; value != "NULL" {
			row[column] = value
		}
		position = end + len(next)
	}
	return row, nil
}

// blockHeader is the line an exporter writes before each message's rows when
// it runs one query per message: "== msg 5843".
var blockHeader = regexp.MustCompile(`(?m)^== msg (\d+)\s*$`)

// ReadBlocksFile reads a file of one query per message, each headed
// "== msg <id>", into each id's rows.
func ReadBlocksFile(path string) (map[string][]Row, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blocks, err := ParseBlocks(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return blocks, nil
}

// ParseBlocks reads the text of one query per message, each headed
// "== msg <id>". Every block must hold rows of the same columns.
func ParseBlocks(text string) (map[string][]Row, error) {
	headers := blockHeader.FindAllStringSubmatchIndex(text, -1)
	if len(headers) == 0 {
		return nil, fmt.Errorf("no \"== msg <id>\" headers")
	}
	if strings.TrimSpace(text[:headers[0][0]]) != "" {
		return nil, fmt.Errorf("text before the first \"== msg\" header")
	}
	var all []string
	owners := []string{}
	for index, header := range headers {
		end := len(text)
		if index+1 < len(headers) {
			end = headers[index+1][0]
		}
		id := text[header[2]:header[3]]
		for _, record := range splitRecords(text[header[1]:end]) {
			all = append(all, record)
			owners = append(owners, id)
		}
	}
	blocks := map[string][]Row{}
	if len(all) == 0 {
		return blocks, nil
	}
	columns := agreedColumns(all)
	for index, record := range all {
		row, err := cut(record, columns)
		if err != nil {
			return nil, fmt.Errorf("msg %s: %w", owners[index], err)
		}
		blocks[owners[index]] = append(blocks[owners[index]], row)
	}
	return blocks, nil
}
