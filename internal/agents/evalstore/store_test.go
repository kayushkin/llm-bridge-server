package evalstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheDatabaseIsPrivateAndAReadableDirectoryIsRefused(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "data")
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	for path, want := range map[string]os.FileMode{directory: 0o700, filepath.Join(directory, databaseFileName): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), want)
		}
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.MkdirAll(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(open); err == nil {
		t.Fatal("Open accepted a directory others can read")
	}
}

func TestSuiteValidationNamesEveryProblem(t *testing.T) {
	store := openTestStore(t)
	_, err := store.CreateSuite(&Suite{Name: "x", TaskKind: "x", JudgeInstructions: "x", JudgeFailureKinds: []string{"other"},
		OutputSchema: json.RawMessage(`[]`), MaxTokens: 1, JudgeMaxTokens: 1, CheckerName: "no-such-checker"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("CreateSuite = %v, want invalid", err)
	}
	for _, phrase := range []string{`"none" and "no_answer"`, "output_schema", "no-such-checker"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("error %q does not mention %s", err, phrase)
		}
	}
}

func TestSuitesAndCasesHavePrefixedIdsAndListsLeaveBodiesOut(t *testing.T) {
	store := openTestStore(t)
	suite := emailSuite(t, store)
	if suite.ID != "eval_suite_000001" {
		t.Errorf("suite id %s", suite.ID)
	}
	c := addEmailCase(t, store, suite, "one")
	if c.ID != "eval_case_000001" || !c.HasBodies || c.UserPrompt == "" {
		t.Errorf("created case %+v", c)
	}
	if c.Features["cards"] != 2 || c.Features["emails"] != 1 {
		t.Errorf("features computed at import = %v", c.Features)
	}
	listed, err := store.ListCases(suite.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].HasBodies || listed[0].UserPrompt != "" || listed[0].OriginalAnswer != nil {
		t.Errorf("list carries bodies: %+v", listed[0])
	}
	if _, err := store.CreateCase(suite.ID, &Case{SourceKind: SourceKindLLMBridgeOperation, SourceRef: "operation_one", UserPrompt: "again"}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second case from the same source = %v, want conflict", err)
	}
	name := "renamed"
	patched, err := store.PatchSuite(suite.ID, SuitePatch{Name: &name})
	if err != nil || patched.Name != "renamed" || patched.JudgeInstructions != suite.JudgeInstructions {
		t.Errorf("PatchSuite = %+v, %v", patched, err)
	}
}
