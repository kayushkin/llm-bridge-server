package fileimport

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every file here is made up in a temporary directory. No real note belongs
// in this repository.

func write(t *testing.T, directory, relative string, content []byte, modified time.Time) {
	t.Helper()
	path := filepath.Join(directory, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func docx(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var document strings.Builder
	document.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, paragraph := range paragraphs {
		// Split each paragraph over two runs, with a tab between, the way Word
		// does, to show runs are joined and a tab is kept.
		first, second, _ := strings.Cut(paragraph, "\t")
		fmt.Fprintf(&document, `<w:p><w:r><w:t>%s</w:t></w:r>`, first)
		if second != "" {
			fmt.Fprintf(&document, `<w:r><w:tab/><w:t xml:space="preserve">%s</w:t></w:r>`, second)
		}
		document.WriteString(`</w:p>`)
	}
	document.WriteString(`</w:body></w:document>`)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	member, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	member.Write([]byte(document.String()))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// minimalPDF is a one-page PDF with one line of text in Helvetica. pdftotext
// rebuilds the cross-reference table when the offsets are off, so they are
// computed here only to keep the file honest.
func minimalPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	crossReference := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, crossReference)
	return out.Bytes()
}

func TestScanReadsEachSupportedFormatAndSkipsTheRest(t *testing.T) {
	directory := t.TempDir()
	location := time.UTC
	modified := time.Date(2021, 6, 1, 15, 4, 5, 0, location)
	write(t, directory, "plain.txt", []byte("Shopping\nmilk\neggs\n"), modified)
	write(t, directory, "folder/dated.md", []byte("# Trip to the coast\n\n2019-08-14\nwe went"), modified)
	write(t, directory, "bom.txt", append([]byte{0xEF, 0xBB, 0xBF}, []byte("With a BOM")...), modified)
	write(t, directory, "utf16.txt", []byte{0xFF, 0xFE, 'H', 0, 'i', 0}, modified)
	write(t, directory, "empty.md", []byte("\n  \n"), modified)
	write(t, directory, "document.docx", docx(t, "Meeting notes", "15 March 2023\tafternoon", "third"), modified)
	write(t, directory, ".hidden.txt", []byte("x"), modified)
	write(t, directory, ".trash/old.txt", []byte("x"), modified)
	write(t, directory, "picture.jpg", []byte{0xFF, 0xD8}, modified)
	_, pdftotextMissing := exec.LookPath("pdftotext")
	write(t, directory, "scan.pdf", minimalPDF("Receipt 2020-01-02"), modified)

	files, skipped, err := Scan(directory, location)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]File{}
	for _, file := range files {
		byPath[file.RelativePath] = file
	}

	plain := byPath["plain.txt"]
	if plain.Title != "Shopping" || plain.Body != "Shopping\nmilk\neggs\n" || plain.DateSource != DateFromModificationTime || !plain.WrittenAt.Equal(modified) {
		t.Errorf("plain.txt = %+v", plain)
	}
	if len(plain.Hash) != 64 {
		t.Errorf("hash %q is not a sha256", plain.Hash)
	}
	dated := byPath["folder/dated.md"]
	if dated.Title != "Trip to the coast" || dated.DateSource != DateFromText || !dated.WrittenAt.Equal(time.Date(2019, 8, 14, 0, 0, 0, 0, location)) {
		t.Errorf("folder/dated.md = %+v", dated)
	}
	if byPath["bom.txt"].Body != "With a BOM" {
		t.Errorf("bom.txt body = %q", byPath["bom.txt"].Body)
	}
	if byPath["utf16.txt"].Body != "Hi" {
		t.Errorf("utf16.txt body = %q", byPath["utf16.txt"].Body)
	}
	if byPath["empty.md"].Title != "empty" {
		t.Errorf("an empty file's title = %q, want its name", byPath["empty.md"].Title)
	}
	document := byPath["document.docx"]
	if document.Body != "Meeting notes\n15 March 2023\tafternoon\nthird" || document.Title != "Meeting notes" ||
		!document.WrittenAt.Equal(time.Date(2023, 3, 15, 0, 0, 0, 0, location)) {
		t.Errorf("document.docx = %+v", document)
	}
	if pdftotextMissing == nil {
		pdf := byPath["scan.pdf"]
		if !strings.Contains(pdf.Body, "Receipt 2020-01-02") || !pdf.WrittenAt.Equal(time.Date(2020, 1, 2, 0, 0, 0, 0, location)) {
			t.Errorf("scan.pdf = %+v", pdf)
		}
	}

	reasons := map[string]string{}
	for _, skip := range skipped {
		reasons[skip.RelativePath] = skip.Reason
	}
	if reasons[".hidden.txt"] != "hidden file" || reasons[".trash/"] != "hidden directory" || reasons["picture.jpg"] != "unsupported extension .jpg" {
		t.Errorf("skipped = %+v", skipped)
	}
	if pdftotextMissing != nil && reasons["scan.pdf"] != "pdftotext is not installed" {
		t.Errorf("with no pdftotext the pdf was not skipped: %+v", skipped)
	}
}

func TestAFileThatCannotBeReadStopsTheScan(t *testing.T) {
	directory := t.TempDir()
	write(t, directory, "fine.txt", []byte("fine"), time.Now())
	write(t, directory, "latin1.txt", []byte{'c', 'a', 'f', 0xE9}, time.Now())
	if _, _, err := Scan(directory, time.UTC); err == nil || !strings.Contains(err.Error(), "latin1.txt") {
		t.Fatalf("Scan = %v, want an error naming latin1.txt", err)
	}
	write(t, directory, "latin1.txt", []byte("now utf-8"), time.Now())
	write(t, directory, "broken.docx", []byte("not a zip"), time.Now())
	if _, _, err := Scan(directory, time.UTC); err == nil || !strings.Contains(err.Error(), "broken.docx") {
		t.Fatalf("Scan = %v, want an error naming broken.docx", err)
	}
}

func TestDateInTextReadsOnlyUnambiguousDatesNearTheTop(t *testing.T) {
	location := time.UTC
	day := func(year int, month time.Month, date int) time.Time {
		return time.Date(year, month, date, 0, 0, 0, 0, location)
	}
	cases := []struct {
		text  string
		want  time.Time
		found bool
	}{
		{"2024-03-15 dentist", day(2024, 3, 15), true},
		{"Notes\n2024/03/15", day(2024, 3, 15), true},
		{"15.03.2024 встреча", day(2024, 3, 15), true},
		{"March 5th, 2022", day(2022, 3, 5), true},
		{"Sat, 5 Nov 2022", day(2022, 11, 5), true},
		{"both 2021-01-02 and 2022-03-04", day(2021, 1, 2), true}, // the first one on the line
		{"03/04/2024 is March or April", time.Time{}, false},
		{"2024-02-30 does not exist", time.Time{}, false},
		{"version 1.2.3", time.Time{}, false},
		{"room 12 2024", time.Time{}, false},
		{"a\nb\nc\nd\ne\n2024-01-01 is the sixth line", time.Time{}, false},
	}
	for _, c := range cases {
		got, found := DateInText(c.text, location)
		if found != c.found || !got.Equal(c.want) {
			t.Errorf("DateInText(%q) = %v %v, want %v %v", c.text, got, found, c.want, c.found)
		}
	}
}

func TestTitleIsCutToTwoHundredCharacters(t *testing.T) {
	long := strings.Repeat("я", 250)
	if got := titleOf(long, "x.txt"); len([]rune(got)) != maximumTitleLength {
		t.Errorf("title is %d characters", len([]rune(got)))
	}
}

// Names in the form of a Samsung Notes export, made up. The title before
// _YYMMDD_HHMMSS is the note's own; "Notes" is what Samsung writes for a note with none.
func TestSamsungExportNameReadsTheTitleAndTimeFromTheFileName(t *testing.T) {
	location := time.FixedZone("UTC+2", 2*3600)
	cases := []struct {
		fileName, wantTitle string
		want                time.Time
		wantFound           bool
	}{
		{"Groceries _240113_194226.txt", "Groceries", time.Date(2024, 1, 13, 19, 42, 26, 0, location), true},
		{"Packing list_240127_090722.txt", "Packing list", time.Date(2024, 1, 27, 9, 7, 22, 0, location), true},
		{"Notes_240921_094127.txt", "", time.Date(2024, 9, 21, 9, 41, 27, 0, location), true},
		{"under_score_title_241016_154516.md", "under_score_title", time.Date(2024, 10, 16, 15, 45, 16, 0, location), true},
		{"Notes_241332_094127.txt", "", time.Time{}, false},
		{"Notes_240921_256127.txt", "", time.Time{}, false},
		{"Notes_2409210_94127.txt", "", time.Time{}, false},
		{"Shopping.txt", "", time.Time{}, false},
	}
	for _, c := range cases {
		title, written, found := SamsungExportName(c.fileName, location)
		if found != c.wantFound || title != c.wantTitle || !written.Equal(c.want) {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)", c.fileName, title, written, found, c.wantTitle, c.want, c.wantFound)
		}
	}
}

// The time in a Samsung file name is the note's own, so it beats a date in
// the text and the mtime an export resets. A titled note keeps that title; an
// untitled one is titled by its first line.
func TestASamsungExportIsDatedAndTitledByItsFileName(t *testing.T) {
	directory := t.TempDir()
	location := time.UTC
	modified := time.Date(2026, 10, 18, 0, 0, 0, 0, location)
	write(t, directory, "Groceries _240113_194226.txt", []byte("bread\nmilk\n"), modified)
	write(t, directory, "Notes_240921_094127.txt", []byte("Water the plants\n2019-08-14\n"), modified)

	files, _, err := Scan(directory, location)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]File{}
	for _, file := range files {
		byPath[file.RelativePath] = file
	}
	titled := byPath["Groceries _240113_194226.txt"]
	if titled.Title != "Groceries" || titled.DateSource != DateFromFileName ||
		!titled.WrittenAt.Equal(time.Date(2024, 1, 13, 19, 42, 26, 0, location)) {
		t.Errorf("titled note = %+v", titled)
	}
	untitled := byPath["Notes_240921_094127.txt"]
	if untitled.Title != "Water the plants" || untitled.DateSource != DateFromFileName ||
		!untitled.WrittenAt.Equal(time.Date(2024, 9, 21, 9, 41, 27, 0, location)) {
		t.Errorf("untitled note = %+v", untitled)
	}
}
