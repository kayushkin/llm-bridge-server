package memorystore

import (
	"testing"
)

func TestPatternTagger_ErrorDetection(t *testing.T) {
	tagger := NewPatternTagger()
	
	tests := []struct {
		text       string
		shouldHave string
	}{
		{"Fatal error occurred", "error"},
		{"panic: runtime error", "error"},
		{"An exception was thrown", "error"},
		{"Stack trace:\n  at main.go:42", "error"},
		{"Everything is fine", ""},
	}
	
	for _, tt := range tests {
		tags := tagger.Tag(tt.text, "user")
		
		hasError := false
		for _, tag := range tags {
			if tag == "error" {
				hasError = true
				break
			}
		}
		
		if tt.shouldHave == "error" && !hasError {
			t.Errorf("Text %q should be tagged with 'error'", tt.text)
		}
		if tt.shouldHave == "" && hasError {
			t.Errorf("Text %q should NOT be tagged with 'error'", tt.text)
		}
	}
}

func TestPatternTagger_CodeDetection(t *testing.T) {
	tagger := NewPatternTagger()
	
	tests := []struct {
		text       string
		shouldHave string
	}{
		{"```go\nfunc main() {}\n```", "code"},
		{"func TestSomething(t *testing.T) {}", "code"},
		{"type User struct {}", "code"},
		{"package main", "code"},
		{"import \"fmt\"", "code"},
		{"This is just plain text", ""},
	}
	
	for _, tt := range tests {
		tags := tagger.Tag(tt.text, "user")
		
		hasCode := false
		for _, tag := range tags {
			if tag == "code" {
				hasCode = true
				break
			}
		}
		
		if tt.shouldHave == "code" && !hasCode {
			t.Errorf("Text %q should be tagged with 'code'", tt.text)
		}
		if tt.shouldHave == "" && hasCode {
			t.Errorf("Text %q should NOT be tagged with 'code'", tt.text)
		}
	}
}

func TestPatternTagger_FilePathExtraction(t *testing.T) {
	tagger := NewPatternTagger()
	
	tests := []struct {
		text         string
		expectedFile string
	}{
		{"Check the file main.go for errors", "main.go"},
		{"Look at /usr/local/bin/myapp", "myapp"},
		{"Edit src/handler.rs please", "handler.rs"},
		{"The config is in config.toml", "config.toml"},
	}
	
	for _, tt := range tests {
		tags := tagger.Tag(tt.text, "user")
		
		hasFile := false
		for _, tag := range tags {
			if tag == tt.expectedFile {
				hasFile = true
				break
			}
		}
		
		if !hasFile {
			t.Errorf("Text %q should extract file tag %q, got tags: %v", tt.text, tt.expectedFile, tags)
		}
	}
}

func TestPatternTagger_IdentityDetection(t *testing.T) {
	tagger := NewPatternTagger()
	
	tests := []struct {
		text       string
		shouldHave bool
	}{
		{"You are a helpful assistant", true},
		{"Your role is to help users", true},
		{"Your purpose is to answer questions", true},
		{"What is your name?", false},
		{"This is a regular message", false},
	}
	
	for _, tt := range tests {
		tags := tagger.Tag(tt.text, "system")
		
		hasIdentity := false
		for _, tag := range tags {
			if tag == "identity" {
				hasIdentity = true
				break
			}
		}
		
		if hasIdentity != tt.shouldHave {
			t.Errorf("Text %q: hasIdentity=%v, want=%v", tt.text, hasIdentity, tt.shouldHave)
		}
	}
}

func TestPatternTagger_SourceTags(t *testing.T) {
	tagger := NewPatternTagger()
	
	tests := []struct {
		source     string
		shouldHave string
	}{
		{"user", "user"},
		{"assistant", "assistant"},
		{"tool-result", "tool-result"},
		{"memory", "memory"},
		{"system", "system"},
	}
	
	for _, tt := range tests {
		tags := tagger.Tag("test text", tt.source)
		
		hasTag := false
		for _, tag := range tags {
			if tag == tt.shouldHave {
				hasTag = true
				break
			}
		}
		
		if !hasTag {
			t.Errorf("Source %q should produce tag %q", tt.source, tt.shouldHave)
		}
	}
}

func TestPatternTagger_TagWithToolName(t *testing.T) {
	tagger := NewPatternTagger()
	
	tags := tagger.TagWithToolName("Command output: success", "exec")
	
	hasExec := false
	hasToolResult := false
	
	for _, tag := range tags {
		if tag == "exec" {
			hasExec = true
		}
		if tag == "tool-result" {
			hasToolResult = true
		}
	}
	
	if !hasExec {
		t.Error("Should have 'exec' tag")
	}
	if !hasToolResult {
		t.Error("Should have 'tool-result' tag")
	}
}

func TestAutoTag(t *testing.T) {
	tags := AutoTag("Error in main.go", "user")
	
	hasUser := false
	hasError := false
	
	for _, tag := range tags {
		if tag == "user" {
			hasUser = true
		}
		if tag == "error" {
			hasError = true
		}
	}
	
	if !hasUser {
		t.Error("Should have 'user' tag")
	}
	if !hasError {
		t.Error("Should have 'error' tag")
	}
}
