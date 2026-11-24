// Package fileimport reads a directory of exported notes (.txt, .md, .docx,
// and .pdf when pdftotext is installed) into entries for journal-store's
// POST /imports. It takes plain files, and reads one thing Samsung Notes
// adds: the note's title and time in the file name of an export
// ("Groceries _240113_194226.txt").
package fileimport

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// File is one file read and ready to import.
type File struct {
	// RelativePath is the path under the directory given, with forward
	// slashes. It is the import's source_ref, so moving the directory does
	// not make every file new.
	RelativePath string
	Title        string
	Body         string
	WrittenAt    time.Time
	// DateSource says where WrittenAt came from.
	DateSource DateSource
	// Hash is the sha256 of the file's bytes: the import's source_hash. An
	// unchanged file is skipped on the next run.
	Hash string
}

// Skipped is a file the scan did not read, and why.
type Skipped struct {
	RelativePath string
	Reason       string
}

// DateSource is where a file's WrittenAt came from, most trusted first.
type DateSource string

const (
	// DateFromFileName is the time Samsung Notes writes into an export's
	// file name. It is the note's own time, so it wins over the other two.
	DateFromFileName DateSource = "file name"
	// DateFromText is the first date in the file's first lines.
	DateFromText DateSource = "date in text"
	// DateFromModificationTime is the file's mtime. An export or a copy
	// resets it, so it is used only when nothing better exists.
	DateFromModificationTime DateSource = "file mtime"
)

// samsungUntitledNote is the title Samsung Notes puts in an export's file
// name when the note has none. Such a note is titled by its first line.
const samsungUntitledNote = "Notes"

// samsungExportFileName matches the stem of a Samsung Notes export:
// the note's title, then _YYMMDD_HHMMSS.
var samsungExportFileName = regexp.MustCompile(`^(.*)_(\d{2})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})$`)

// SamsungExportName reads the title and time from a Samsung Notes export's
// file name. The title is "" when the name is Samsung's default for a note
// with none. found is false when the name is not in that form or its digits
// are not a real date and time.
func SamsungExportName(fileName string, location *time.Location) (title string, written time.Time, found bool) {
	match := samsungExportFileName.FindStringSubmatch(strings.TrimSuffix(fileName, filepath.Ext(fileName)))
	if match == nil {
		return "", time.Time{}, false
	}
	var numbers [6]int
	for i := range numbers {
		numbers[i], _ = strconv.Atoi(match[i+2])
	}
	written = time.Date(2000+numbers[0], time.Month(numbers[1]), numbers[2], numbers[3], numbers[4], numbers[5], 0, location)
	if written.Year() != 2000+numbers[0] || int(written.Month()) != numbers[1] || written.Day() != numbers[2] ||
		written.Hour() != numbers[3] || written.Minute() != numbers[4] || written.Second() != numbers[5] {
		return "", time.Time{}, false
	}
	title = strings.TrimSpace(match[1])
	if title == samsungUntitledNote {
		title = ""
	}
	return title, written, true
}

// maximumTitleLength caps a title taken from a first line, in characters.
const maximumTitleLength = 200

// datedLinesToSearch is how many non-empty lines from the top are searched
// for a date.
const datedLinesToSearch = 5

// Scan reads every file under root, in path order. A file with an extension
// it cannot read is returned in skipped. A file it should be able to read and
// cannot is an error, and the whole scan stops: an import that quietly leaves
// a note behind is worse than one that does not start.
func Scan(root string, location *time.Location) ([]File, []Skipped, error) {
	_, pdftotextErr := exec.LookPath("pdftotext")
	var files []File
	var skipped []Skipped
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				skipped = append(skipped, Skipped{RelativePath: relative + "/", Reason: "hidden directory"})
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") {
			skipped = append(skipped, Skipped{RelativePath: relative, Reason: "hidden file"})
			return nil
		}
		if !entry.Type().IsRegular() {
			skipped = append(skipped, Skipped{RelativePath: relative, Reason: "not a regular file"})
			return nil
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension == ".pdf" && pdftotextErr != nil {
			skipped = append(skipped, Skipped{RelativePath: relative, Reason: "pdftotext is not installed"})
			return nil
		}
		if extension != ".txt" && extension != ".md" && extension != ".docx" && extension != ".pdf" {
			skipped = append(skipped, Skipped{RelativePath: relative, Reason: "unsupported extension " + extension})
			return nil
		}
		file, err := Read(path, relative, location)
		if err != nil {
			return fmt.Errorf("%s: %w", relative, err)
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	return files, skipped, nil
}

// Read turns one file into a File.
func Read(path, relativePath string, location *time.Location) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return File{}, err
	}
	var text string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".txt", ".md":
		text, err = decodeText(raw)
	case ".docx":
		text, err = docxText(raw)
	case ".pdf":
		text, err = pdfText(path)
	default:
		err = fmt.Errorf("no reader for %s", filepath.Ext(path))
	}
	if err != nil {
		return File{}, err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	digest := sha256.Sum256(raw)
	file := File{
		RelativePath: relativePath,
		Title:        titleOf(text, filepath.Base(path)),
		Body:         text,
		WrittenAt:    info.ModTime().In(location),
		DateSource:   DateFromModificationTime,
		Hash:         hex.EncodeToString(digest[:]),
	}
	if title, written, found := SamsungExportName(filepath.Base(path), location); found {
		file.WrittenAt, file.DateSource = written, DateFromFileName
		if title != "" {
			file.Title = cutTitle(title)
		}
	} else if date, found := DateInText(text, location); found {
		file.WrittenAt, file.DateSource = date, DateFromText
	}
	return file, nil
}

// decodeText reads UTF-8, with or without a byte-order mark, and UTF-16 with
// one. Anything else is an error rather than text with replacement marks in
// it.
func decodeText(raw []byte) (string, error) {
	switch {
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		raw = raw[3:]
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}), bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		return decodeUTF16(raw)
	}
	if !utf8.Valid(raw) {
		return "", errors.New("not UTF-8 or UTF-16 text; convert it before importing")
	}
	return string(raw), nil
}

func decodeUTF16(raw []byte) (string, error) {
	littleEndian := raw[0] == 0xFF
	raw = raw[2:]
	if len(raw)%2 != 0 {
		return "", errors.New("UTF-16 text with an odd number of bytes")
	}
	units := make([]uint16, len(raw)/2)
	for index := range units {
		if littleEndian {
			units[index] = uint16(raw[2*index]) | uint16(raw[2*index+1])<<8
		} else {
			units[index] = uint16(raw[2*index])<<8 | uint16(raw[2*index+1])
		}
	}
	return string(utf16.Decode(units)), nil
}

// docxText reads the paragraphs of word/document.xml: the text runs, tabs and
// line breaks in each, one paragraph per line. Tables, headers, footnotes and
// images are not read, which is enough for a note app's export.
func docxText(raw []byte) (string, error) {
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("not a .docx (zip) file: %w", err)
	}
	var document io.ReadCloser
	for _, member := range archive.File {
		if member.Name == "word/document.xml" {
			if document, err = member.Open(); err != nil {
				return "", err
			}
			break
		}
	}
	if document == nil {
		return "", errors.New("a .docx with no word/document.xml")
	}
	defer document.Close()

	const wordNamespace = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	decoder := xml.NewDecoder(document)
	var paragraphs []string
	var current strings.Builder
	inText := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("word/document.xml: %w", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Space != wordNamespace {
				continue
			}
			switch element.Name.Local {
			case "t":
				inText = true
			case "tab":
				current.WriteByte('\t')
			case "br", "cr":
				current.WriteByte('\n')
			}
		case xml.EndElement:
			if element.Name.Space != wordNamespace {
				continue
			}
			switch element.Name.Local {
			case "t":
				inText = false
			case "p":
				paragraphs = append(paragraphs, current.String())
				current.Reset()
			}
		case xml.CharData:
			if inText {
				current.Write(element)
			}
		}
	}
	return strings.Join(paragraphs, "\n"), nil
}

// pdfText runs pdftotext, keeping the layout of lines. A PDF with no text
// layer (a scan) gives an empty body, which is an error: it needs OCR, and an
// empty note would hide that.
func pdfText(path string) (string, error) {
	var stderr bytes.Buffer
	command := exec.Command("pdftotext", "-enc", "UTF-8", "-layout", path, "-")
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("pdftotext: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	text := strings.TrimRight(strings.ReplaceAll(string(out), "\f", "\n"), "\n")
	if strings.TrimSpace(text) == "" {
		return "", errors.New("pdftotext found no text: the PDF is probably a scan and needs OCR")
	}
	return text, nil
}

// titleOf is the first non-empty line, without markdown heading marks, cut
// to maximumTitleLength characters; or the file name without its extension
// when the text has no line with anything on it.
func titleOf(text, fileName string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line == "" {
			continue
		}
		return cutTitle(line)
	}
	return strings.TrimSuffix(fileName, filepath.Ext(fileName))
}

// cutTitle cuts a title to maximumTitleLength characters.
func cutTitle(title string) string {
	if utf8.RuneCountInString(title) > maximumTitleLength {
		return string([]rune(title)[:maximumTitleLength])
	}
	return title
}

var monthNumbers = map[string]time.Month{
	"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3, "april": 4, "apr": 4,
	"may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7, "august": 8, "aug": 8,
	"september": 9, "sep": 9, "sept": 9, "october": 10, "oct": 10, "november": 11, "nov": 11,
	"december": 12, "dec": 12,
}

var (
	// 2024-03-15, 2024.03.15, 2024/03/15: year first is never ambiguous.
	yearFirstDate = regexp.MustCompile(`\b(\d{4})[-./](\d{1,2})[-./](\d{1,2})\b`)
	// 15.03.2024: day first with dots, as most of Europe and Russia write it.
	// Slashes are not read: 03/04/2024 is March or April depending on who wrote it.
	dayFirstDottedDate = regexp.MustCompile(`\b(\d{1,2})\.(\d{1,2})\.(\d{4})\b`)
	// March 15, 2024 / Mar 15 2024
	monthNameFirstDate = regexp.MustCompile(`(?i)\b([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	// 15 March 2024
	dayFirstMonthNameDate = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+([a-z]{3,9})\.?,?\s+(\d{4})\b`)
)

// DateInText finds the first date in the first datedLinesToSearch non-empty
// lines and returns midnight of that day in location. Only forms that read
// one way are recognized; a date that does not exist (2024-02-30) is not one.
func DateInText(text string, location *time.Location) (time.Time, bool) {
	searched := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if searched == datedLinesToSearch {
			break
		}
		searched++
		if date, found := dateInLine(line, location); found {
			return date, true
		}
	}
	return time.Time{}, false
}

func dateInLine(line string, location *time.Location) (time.Time, bool) {
	type candidate struct {
		at               int
		year, month, day string
		monthName        string
	}
	var candidates []candidate
	for _, match := range yearFirstDate.FindAllStringSubmatchIndex(line, -1) {
		candidates = append(candidates, candidate{at: match[0], year: line[match[2]:match[3]], month: line[match[4]:match[5]], day: line[match[6]:match[7]]})
	}
	for _, match := range dayFirstDottedDate.FindAllStringSubmatchIndex(line, -1) {
		candidates = append(candidates, candidate{at: match[0], day: line[match[2]:match[3]], month: line[match[4]:match[5]], year: line[match[6]:match[7]]})
	}
	for _, match := range monthNameFirstDate.FindAllStringSubmatchIndex(line, -1) {
		candidates = append(candidates, candidate{at: match[0], monthName: line[match[2]:match[3]], day: line[match[4]:match[5]], year: line[match[6]:match[7]]})
	}
	for _, match := range dayFirstMonthNameDate.FindAllStringSubmatchIndex(line, -1) {
		candidates = append(candidates, candidate{at: match[0], day: line[match[2]:match[3]], monthName: line[match[4]:match[5]], year: line[match[6]:match[7]]})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].at < candidates[j].at })
	for _, found := range candidates {
		// strconv, not fmt.Sscan: Sscan reads "08" as an octal number and fails.
		year, yearErr := strconv.Atoi(found.year)
		day, dayErr := strconv.Atoi(found.day)
		if yearErr != nil || dayErr != nil {
			continue
		}
		var monthNumber int
		if found.monthName != "" {
			month, known := monthNumbers[strings.ToLower(found.monthName)]
			if !known {
				continue
			}
			monthNumber = int(month)
		} else {
			parsed, err := strconv.Atoi(found.month)
			if err != nil {
				continue
			}
			monthNumber = parsed
		}
		if year < 1900 || year > 2100 || monthNumber < 1 || monthNumber > 12 || day < 1 || day > 31 {
			continue
		}
		date := time.Date(year, time.Month(monthNumber), day, 0, 0, 0, 0, location)
		if date.Day() != day || int(date.Month()) != monthNumber {
			continue // 2024-02-30 rolled over into March: not a real date
		}
		return date, true
	}
	return time.Time{}, false
}
