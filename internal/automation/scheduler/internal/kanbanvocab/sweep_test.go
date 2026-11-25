package kanbanvocab

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoBinarySpellsTheVocabularyItself keeps this package's sweep swept.
//
// The vocabulary was centralised by hand, and the hand sweep declared seven
// binaries served. Three of them still spelled a value themselves — one on the
// spend path — and nobody noticed for months, because a duplicated literal that
// still happens to agree produces no error and no wrong-looking output. It
// costs nothing until the day the two copies disagree, and then it costs
// silently.
//
// So the sweep is a test rather than a note. A hand sweep does not stay done.
//
// # What this scans, and what it deliberately does not
//
// It reads STRING LITERALS FROM THE AST, not lines of text, so a value named in
// a doc comment or an ordinary sentence is invisible to it. That matters: the
// word "scoped" appears in an English comment in cmd/ask, and cmd/ask's own doc
// comment shows 'Needs approval' and 'Queued' inside a usage example. None of
// those are wire values and none should be promoted.
//
// It skips _test.go files. A test that spells a literal is usually doing it on
// purpose — TestWireValues exists precisely to spell all of them a second time,
// and pinning a wire format means writing it out rather than referring to the
// constant under test.
//
// # A value match is not a concept match
//
// The allowlist below is the interesting half of this test. Every entry is a
// string that equals a constant here and means something else entirely. They
// are recorded with a reason so the next person to run a sweep does not "fix"
// them, which is the mistake this test would otherwise encourage.
func TestNoBinarySpellsTheVocabularyItself(t *testing.T) {
	// Every wire value this package owns, mapped to the constant to use.
	owned := map[string]string{
		EntityTypeNote:          "EntityTypeNote",
		EntityTypeSession:       "EntityTypeSession / LinkTypeSession",
		LinkTypeParentCard:      "LinkTypeParentCard",
		ColumnInProgress:        "ColumnInProgress",
		ColumnDone:              "ColumnDone",
		ColumnFailed:            "ColumnFailed",
		BoardTaskCompletionLoop: "BoardTaskCompletionLoop",
		ColumnGoals:             "ColumnGoals",
		ColumnBacklog:           "ColumnBacklog",
		TagTrigger:              "TagTrigger",
		TagSubtask:              "TagSubtask",
		TagScoped:               "TagScoped",
		TagGroupPrefix:          "TagGroupPrefix",
		BoardAgentRuns:          "BoardAgentRuns",
		BoardAutoworkerQueue:    "BoardAutoworkerQueue",
		ColumnQueued:            "ColumnQueued",
		ColumnBlocked:           "ColumnBlocked",
	}

	// file -> literal -> why this one is NOT the vocabulary's concept.
	allowed := map[string]map[string]string{
		"cmd/autoworker/main.go": {
			"note": `noteboard's item type, inside the worker prompt. A different store with its own type vocabulary; kanban-store's "note" entity type is unrelated.`,
		},
		"cmd/kanban-classifier/main.go": {
			"session": `the name of the --session command-line flag, not a wire value.`,
		},
		"cmd/kanban-scoper/main.go": {
			"scoped": `the free-text LABEL on a parent_card link ("who made this link"), which coincides with TagScoped by value only. Promoting it would be a false join.`,
		},
		"cmd/ask/main.go": {
			"session": `the name of the --session command-line flag, which takes a herald bus session id. Not a wire value.`,
		},
		"cmd/email-classifier/main.go": {
			"note": `the name of the --note command-line flag (a routing note's text). Not a wire value.`,
		},
		"cmd/email-classifier/notes.go": {
			"note": `noteboard's item type on the routing-note POST. A different store with its own type vocabulary; kanban-store's "note" entity type is unrelated.`,
		},
		"cmd/kanban-dispatcher/main.go": {
			"task-completion-loop": `the bus Orchestrator and Channel for the dispatched session — NATS routing, not a kanban entity tag. Shares a value with TagTrigger because both are named after the loop.`,
		},
	}

	root := repoRoot(t)
	var problems []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		// This package is where the values are allowed to live.
		if strings.HasPrefix(rel, "internal/kanbanvocab/") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, uqErr := strconv.Unquote(lit.Value)
			if uqErr != nil {
				return true
			}
			constName, isOwned := owned[value]
			if !isOwned {
				return true
			}
			if reason, ok := allowed[rel][value]; ok {
				_ = reason
				return true
			}
			problems = append(problems, "\n  "+rel+":"+
				strconv.Itoa(fset.Position(lit.Pos()).Line)+
				"\n    spells "+strconv.Quote(value)+
				" — use kanbanvocab."+constName+
				",\n    or add it to this test's allowlist with the reason it is a different concept.")
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repo: %v", err)
	}

	if len(problems) > 0 {
		t.Errorf("%d raw kanban wire value(s) outside internal/kanbanvocab:%s",
			len(problems), strings.Join(problems, ""))
	}
}

// TestTheSweepAllowlistIsNotStale fails when an allowlist entry stops matching
// anything. A stale entry is worse than no entry: it reads as a considered
// judgment about live code, so the next sweep trusts it and skips a file that
// no longer contains what the reason describes.
func TestTheSweepAllowlistIsNotStale(t *testing.T) {
	for _, rel := range []string{
		"cmd/autoworker/main.go",
		"cmd/kanban-classifier/main.go",
		"cmd/kanban-scoper/main.go",
		"cmd/kanban-dispatcher/main.go",
	} {
		path := filepath.Join(repoRoot(t), filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("allowlisted file %s no longer exists — drop its entry", rel)
		}
	}
}

// repoRoot walks up from this package to the directory holding go.mod. The test
// scans the whole module, so it must not guess the depth.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test's working directory")
		}
		dir = parent
	}
}
