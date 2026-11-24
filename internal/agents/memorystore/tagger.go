package memorystore

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Tagger generates tags for chunks based on content
type Tagger interface {
	Tag(text string, source string) []string
}

// PatternTagger implements pattern-based tagging
type PatternTagger struct {
	errorPatterns    []*regexp.Regexp
	codePatterns     []*regexp.Regexp
	filePathPatterns []*regexp.Regexp
}

// NewPatternTagger creates a new pattern-based tagger
func NewPatternTagger() *PatternTagger {
	return &PatternTagger{
		errorPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\berror\b`),
			regexp.MustCompile(`(?i)\bpanic\b`),
			regexp.MustCompile(`(?i)\bfatal\b`),
			regexp.MustCompile(`(?i)\bstack trace\b`),
			regexp.MustCompile(`(?i)\bexception\b`),
			regexp.MustCompile(`at \w+\.\w+\([^)]+:\d+\)`), // Stack trace line
		},
		codePatterns: []*regexp.Regexp{
			regexp.MustCompile("```"),                  // Code blocks
			regexp.MustCompile(`(?m)^\s*func\s+\w+`),  // Go functions
			regexp.MustCompile(`(?m)^\s*type\s+\w+`),  // Go types
			regexp.MustCompile(`(?m)^\s*package\s+\w+`), // Go package
			regexp.MustCompile(`(?m)^\s*import\s+`),   // Imports
		},
		filePathPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?:^|[\s])([a-zA-Z0-9_\-./]+\.[a-z]{1,4})(?:[\s]|$)`), // file.ext
			regexp.MustCompile(`(?:^|[\s])(/[a-zA-Z0-9_\-./]+)(?:[\s]|$)`),            // /absolute/path
		},
	}
}

// Tag generates tags for the given text and source
func (t *PatternTagger) Tag(text string, source string) []string {
	tags := make(map[string]bool)
	
	// Source-based tags
	switch source {
	case "user":
		tags["user"] = true
	case "assistant":
		tags["assistant"] = true
	case "tool-result":
		tags["tool-result"] = true
	case "memory":
		tags["memory"] = true
	case "system":
		tags["system"] = true
	}
	
	// Identity detection
	if strings.Contains(strings.ToLower(text), "you are") ||
		strings.Contains(strings.ToLower(text), "your role") ||
		strings.Contains(strings.ToLower(text), "your purpose") {
		tags["identity"] = true
	}
	
	// Error detection
	for _, pattern := range t.errorPatterns {
		if pattern.MatchString(text) {
			tags["error"] = true
			break
		}
	}
	
	// Code detection
	for _, pattern := range t.codePatterns {
		if pattern.MatchString(text) {
			tags["code"] = true
			break
		}
	}
	
	// File path extraction
	filePaths := t.extractFilePaths(text)
	for _, path := range filePaths {
		// Tag with the filename
		filename := filepath.Base(path)
		if filename != "" && filename != "." {
			tags[filename] = true
		}
	}
	
	// Convert map to slice
	result := make([]string, 0, len(tags))
	for tag := range tags {
		result = append(result, tag)
	}
	
	return result
}

// TagWithToolName tags tool results with the tool name
func (t *PatternTagger) TagWithToolName(text string, toolName string) []string {
	tags := t.Tag(text, "tool-result")
	
	// Add tool name as a tag
	tags = append(tags, toolName)
	
	return tags
}

// extractFilePaths finds file paths in text
func (t *PatternTagger) extractFilePaths(text string) []string {
	var paths []string
	seen := make(map[string]bool)
	
	for _, pattern := range t.filePathPatterns {
		matches := pattern.FindAllStringSubmatch(text, -1)
		for _, match := range matches {
			if len(match) > 1 {
				path := strings.TrimSpace(match[1])
				if !seen[path] && isValidPath(path) {
					paths = append(paths, path)
					seen[path] = true
				}
			}
		}
	}
	
	return paths
}

// isValidPath performs basic validation on extracted paths
func isValidPath(path string) bool {
	// Ignore very short paths
	if len(path) < 3 {
		return false
	}
	
	// Ignore common words that look like extensions
	lowerPath := strings.ToLower(path)
	invalidExtensions := []string{".com", ".org", ".net", ".io"}
	for _, ext := range invalidExtensions {
		if strings.HasSuffix(lowerPath, ext) {
			return false
		}
	}
	
	return true
}

// AutoTag is a convenience function that creates a tagger and tags text
func AutoTag(text string, source string) []string {
	tagger := NewPatternTagger()
	return tagger.Tag(text, source)
}