package kanbanclient

import (
	"bytes"
	"strings"
	"testing"
)

// An answer longer than the bound is an error naming it, never a silently
// shortened body (a 1 MiB io.LimitReader did that until 2026-10-29).
func TestReadKanbanAnswerRefusesAnAnswerLongerThanTheBound(t *testing.T) {
	data, err := readKanbanAnswer(bytes.NewReader(make([]byte, maximumKanbanAnswerBytes+1)), "board page")
	if err == nil || data != nil || !strings.Contains(err.Error(), "board page") {
		t.Fatalf("readKanbanAnswer = %d bytes, %v; want an error naming the answer", len(data), err)
	}
}

func TestReadKanbanAnswerReadsAnAnswerOfExactlyTheBound(t *testing.T) {
	data, err := readKanbanAnswer(bytes.NewReader(make([]byte, maximumKanbanAnswerBytes)), "board page")
	if err != nil || len(data) != maximumKanbanAnswerBytes {
		t.Fatalf("readKanbanAnswer = %d bytes, %v", len(data), err)
	}
}
