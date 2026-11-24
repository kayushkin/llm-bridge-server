package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kayushkin/tool-store/schema"
)

// ReadFile returns a tool that reads file contents.
// Supports reading multiple files at once via the "paths" parameter.
func ReadFile() Impl {
	type input struct {
		Path   string   `json:"path"`
		Paths  []string `json:"paths"`
		Offset int      `json:"offset"`
		Limit  int      `json:"limit"`
	}
	return Impl{
		Name:        "read_files",
		Description: "Read file contents. Use 'path' for a single file, or 'paths' (array) to read multiple files at once — prefer batching reads to save turns. For large files, use offset (1-indexed line) and limit (max lines).",
		InputSchema: schema.Props([]string{}, map[string]any{
			"path":   schema.Str("Path to a single file"),
			"paths":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Multiple file paths to read at once (preferred for batching)"},
			"offset": schema.Integer("Line number to start from (1-indexed, optional, single file only)"),
			"limit":  schema.Integer("Maximum number of lines to return (optional, single file only)"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			// Collect all paths to read.
			paths := in.Paths
			if in.Path != "" {
				if len(paths) == 0 {
					paths = []string{in.Path}
				} else {
					// Both specified — prepend single path.
					paths = append([]string{in.Path}, paths...)
				}
			}
			if len(paths) == 0 {
				return "error: provide 'path' or 'paths'", nil
			}

			// Single file with offset/limit support.
			if len(paths) == 1 {
				return readSingleFile(paths[0], in.Offset, in.Limit), nil
			}

			// Multiple files — batch read.
			var results []string
			for _, p := range paths {
				content := readSingleFile(p, 0, 0)
				results = append(results, fmt.Sprintf("=== %s ===\n%s", p, content))
			}
			return strings.Join(results, "\n\n"), nil
		},
	}
}

// maxWholeFileReadBytes caps a read that asked for no particular range. It is
// a second, independent limit from the line window in schema.TruncateFileRead:
// a file can be short enough to return whole and still be enormous, if its
// lines are.
const maxWholeFileReadBytes = 100_000

// readSingleFile reads one file with optional offset/limit.
//
// The footer it appends is a claim the rest of the system acts on — inber's
// read cache parses "[complete file — N lines]" and then serves a stub for
// every later read of that path, including the offset/limit read the
// truncation notice tells the model to make. So the footer states what this
// function actually returned, reported by the code that did each cut. It must
// never be re-derived by counting lines in the output: that count also counts
// the truncation banner, and it cannot see a cut made earlier in this
// function.
func readSingleFile(path string, offset, limit int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: %s", err)
	}

	allLines := schema.FileLines(string(data))
	totalLines := len(allLines)

	// Manual offset/limit pagination — the caller named a range and knows it
	// is getting part of the file.
	if offset > 0 || limit > 0 {
		start := 0
		if offset > 0 {
			start = offset - 1
		}
		if start >= totalLines {
			return fmt.Sprintf("offset %d beyond file length (%d lines)", offset, totalLines)
		}
		end := totalLines
		if limit > 0 && start+limit < end {
			end = start + limit
		}
		content := strings.Join(allLines[start:end], "\n")

		// Tell the model what range it got and what remains.
		if end < totalLines {
			content += fmt.Sprintf("\n\n[showing lines %d-%d of %d. Use offset=%d to continue]", start+1, end, totalLines, end+1)
		} else {
			content += fmt.Sprintf("\n\n[showing lines %d-%d of %d — end of file]", start+1, end, totalLines)
		}
		return content
	}

	// Whole-file read. Two limits can cut it short and the caller asked for
	// neither, so track each one rather than inferring afterwards.
	content := string(data)
	droppedBytes := 0
	keptBytes := len(content)
	if len(content) > maxWholeFileReadBytes {
		// The cut lands on a rune boundary, so it can keep up to three bytes
		// fewer than the cap. Report what was actually kept rather than the
		// cap, or the banner overstates the read by those bytes.
		kept := schema.TruncateAtRuneBoundary(content, maxWholeFileReadBytes)
		keptBytes = len(kept)
		droppedBytes = len(content) - keptBytes
		content = kept + "\n... (truncated)"
	}
	content, cut := schema.TruncateFileRead(content)

	switch {
	case droppedBytes > 0:
		// The byte cap fired, so the result stops mid-file and, if the line
		// window fired too, has a hole in it as well. Neither end of it can
		// honestly be given as a line range, so report bytes.
		content += fmt.Sprintf("\n\n[partial read — %d of %d bytes of a %d-line file. Use offset/limit to read specific sections]",
			keptBytes, len(data), totalLines)
	case cut.Truncated():
		content += fmt.Sprintf("\n\n[partial read — lines 1-%d and %d-%d of %d. Use offset/limit to read the lines between]",
			cut.KeptFirst, totalLines-cut.KeptLast+1, totalLines, totalLines)
	default:
		// Complete file — explicitly say so to prevent re-reads.
		content += fmt.Sprintf("\n\n[complete file — %d lines]", totalLines)
	}

	return content
}

// WriteFile returns a tool that creates or overwrites files.
func WriteFile() Impl {
	// Content is a pointer so an absent "content" key is distinguishable from an
	// explicit empty one. Discriminating this union on the zero value read
	// {"path":"x"} as a request to write empty bytes, so a schema-valid call
	// truncated an existing file and reported "wrote 0 bytes" as success.
	type fileEntry struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
	}
	type input struct {
		Path    string      `json:"path"`
		Content *string     `json:"content"`
		Files   []fileEntry `json:"files"`
	}
	return Impl{
		Name:        "write_files",
		Description: "Create or overwrite files. Use files[] to write multiple at once. Always pair writes with a build/test — writes are deterministic, don't waste a turn on just writes.",
		InputSchema: schema.Props([]string{}, map[string]any{
			"path":    schema.Str("Path to a single file to write"),
			"content": schema.Str("Content for the single file"),
			"files":   map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"path": schema.Str("File path"), "content": schema.Str("File content")}, "required": []string{"path", "content"}}, "description": "Multiple files to write at once (preferred for batching)"},
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			// Collect all files to write.
			files := in.Files
			if in.Path != "" {
				files = append([]fileEntry{{Path: in.Path, Content: in.Content}}, files...)
			}
			if len(files) == 0 {
				return "error: provide 'path'+'content' or 'files'", nil
			}

			var results []string
			for _, f := range files {
				// An entry that never sent "content" is an incomplete call, not a
				// request for an empty file. Refuse it before touching the path;
				// "content":"" is the way to ask for a truncation on purpose.
				if f.Content == nil {
					results = append(results, fmt.Sprintf("error: %s: no \"content\" key, refusing to write (send \"content\":\"\" to truncate deliberately)", f.Path))
					continue
				}
				if err := os.MkdirAll(filepath.Dir(f.Path), 0755); err != nil {
					results = append(results, fmt.Sprintf("error creating directory for %s: %s", f.Path, err))
					continue
				}
				if err := os.WriteFile(f.Path, []byte(*f.Content), 0644); err != nil {
					results = append(results, fmt.Sprintf("error writing %s: %s", f.Path, err))
					continue
				}
				results = append(results, fmt.Sprintf("wrote %d bytes to %s", len(*f.Content), f.Path))
			}
			return strings.Join(results, "\n"), nil
		},
	}
}

// EditFile returns a tool that does exact string replacement in files.
func EditFile() Impl {
	type editEntry struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	type input struct {
		Path    string      `json:"path"`
		OldText string      `json:"old_text"`
		NewText string      `json:"new_text"`
		Edits   []editEntry `json:"edits"`
	}
	return Impl{
		Name:        "edit_files",
		Description: "Make exact text replacements. Use edits[] for multiple files at once. Always pair with verification (build, grep, read) — edits are deterministic.",
		InputSchema: schema.Props([]string{}, map[string]any{
			"path":     schema.Str("Path to a single file to edit"),
			"old_text": schema.Str("Exact text to find and replace"),
			"new_text": schema.Str("New text to replace the old text with"),
			"edits":    map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"path": schema.Str("File path"), "old_text": schema.Str("Text to find"), "new_text": schema.Str("Replacement text")}, "required": []string{"path", "old_text", "new_text"}}, "description": "Multiple edits to apply at once (preferred for batching)"},
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			// Collect all edits.
			edits := in.Edits
			if in.Path != "" {
				edits = append([]editEntry{{Path: in.Path, OldText: in.OldText, NewText: in.NewText}}, edits...)
			}
			if len(edits) == 0 {
				return "error: provide 'path'+'old_text'+'new_text' or 'edits'", nil
			}

			var results []string
			for _, e := range edits {
				data, err := os.ReadFile(e.Path)
				if err != nil {
					results = append(results, fmt.Sprintf("error: %s", err))
					continue
				}
				content := string(data)

				count := strings.Count(content, e.OldText)
				if count == 0 {
					results = append(results, fmt.Sprintf("error: old_text not found in %s", e.Path))
					continue
				}
				if count > 1 {
					results = append(results, fmt.Sprintf("error: old_text matches %d times in %s — must be unique", count, e.Path))
					continue
				}

				newContent := strings.Replace(content, e.OldText, e.NewText, 1)
				if err := os.WriteFile(e.Path, []byte(newContent), 0644); err != nil {
					results = append(results, fmt.Sprintf("error writing %s: %s", e.Path, err))
					continue
				}
				results = append(results, fmt.Sprintf("edited %s", e.Path))
			}
			return strings.Join(results, "\n"), nil
		},
	}
}

// maxWalkedEntries caps how many entries a recursive listing will walk before
// it gives up. maxListedEntries caps how many of them are shown. They are two
// different limits and the gap between them is where a listing stops being
// able to describe itself: past the first, the tool no longer knows the size
// of the tree, and past the second it is not showing what it does know.
const (
	maxWalkedEntries = 1000
	maxListedEntries = 50
)

// ListFiles returns a tool that lists directory contents.
func ListFiles() Impl {
	type input struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	return Impl{
		Name: "list_files",
		// The description said this respected .gitignore. It reads no ignore
		// file of any kind — the recursive walk skips dot-directories and
		// nothing else — so a recursive listing of a Node repo walked
		// node_modules in full and spent the whole cap inside it. Saying what
		// it skips is the honest version; see the sibling ripgrep tool, whose
		// identical-sounding claim is true because rg does it natively.
		Description: "List files and directories at a path. Use recursive=true for a tree listing. A recursive listing skips dot-directories (.git, .venv) and reads no ignore file, so vendored trees like node_modules are walked in full — give a narrower path to stay out of them.",
		InputSchema: schema.Props([]string{"path"}, map[string]any{
			"path":      schema.Str("Directory path to list"),
			"recursive": schema.Bool("List recursively (default: false)"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}
			if in.Path == "" {
				in.Path = "."
			}

			if !in.Recursive {
				entries, err := os.ReadDir(in.Path)
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				var lines []string
				for _, e := range entries {
					name := e.Name()
					if e.IsDir() {
						name += "/"
					}
					lines = append(lines, name)
				}
				// ReadDir returns the whole directory, so this listing is
				// drawn from a population that was counted in full.
				return schema.TruncateList(lines, maxListedEntries, schema.CompletePopulation(len(lines))), nil
			}

			var lines []string
			walkStoppedAtCap := false
			filepath.WalkDir(in.Path, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if len(lines) >= maxWalkedEntries {
					walkStoppedAtCap = true
					return filepath.SkipAll
				}
				name := d.Name()
				if d.IsDir() && strings.HasPrefix(name, ".") && path != in.Path {
					return filepath.SkipDir
				}
				rel, _ := filepath.Rel(in.Path, path)
				if rel == "." {
					return nil
				}
				if d.IsDir() {
					rel += "/"
				}
				lines = append(lines, rel)
				return nil
			})

			// The cap fact is passed through as population rather than pushed
			// onto the end of the list. As a list item it was entry number
			// 1001 of 1001, which put it past the display cut every time, so
			// the one notice telling the reader the tree was larger than the
			// walk is exactly the line that never survived to be read.
			population := schema.CompletePopulation(len(lines))
			if walkStoppedAtCap {
				population = schema.PopulationStoppedEarly(len(lines))
			}
			return schema.TruncateList(lines, maxListedEntries, population), nil
		},
	}
}
