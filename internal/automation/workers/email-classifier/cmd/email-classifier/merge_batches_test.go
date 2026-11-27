package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// actionsForTest returns one action that makes a card a task and one that
// leaves it a bucket, from the vocabulary in force.
func actionsForTest(t *testing.T) (task, bucket string) {
	t.Helper()
	if err := selectVocabulary("personal"); err != nil {
		t.Fatal(err)
	}
	for _, action := range active.Actions {
		if active.ActionsNeedingWork[action] && task == "" {
			task = action
		}
		if !active.ActionsNeedingWork[action] && bucket == "" {
			bucket = action
		}
	}
	if task == "" || bucket == "" {
		t.Fatalf("vocabulary %s has no task action or no bucket action", active.Name)
	}
	return task, bucket
}

func TestOnlyCardsAMergeCouldTouchAreAskedAbout(t *testing.T) {
	task, bucket := actionsForTest(t)
	state := &boardState{Cards: map[string]*cardState{
		"bucket":   {ID: "bucket", ColumnName: columnNoAction, Action: bucket},
		"task":     {ID: "task", ColumnName: columnActionNeeded, Action: task},
		"finished": {ID: "finished", ColumnName: columnActionCompleted, Action: bucket},
	}}
	cards := cardsAMergeCouldTouch(state, nil)
	if len(cards) != 1 || cards[0].ID != "bucket" {
		t.Fatalf("asked about %v", cards)
	}
}

func TestCardsAreAskedAboutFiftyAtATimeWithTheWholeBoardEachTime(t *testing.T) {
	var cards []*cardState
	for index := 0; index < 120; index++ {
		cards = append(cards, &cardState{ID: fmt.Sprintf("card-%03d", index), Title: "t"})
	}
	var mutex sync.Mutex
	var prompts []string
	var inFlight, mostInFlight atomic.Int32
	groups, failures := proposeMergesInBatches(context.Background(), "[WHOLE BOARD]\n", cards, func(_ context.Context, prompt string) (*mergeReport, error) {
		now := inFlight.Add(1)
		for {
			seen := mostInFlight.Load()
			if now <= seen || mostInFlight.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		mutex.Lock()
		prompts = append(prompts, prompt)
		mutex.Unlock()
		return &mergeReport{Merges: []mergeGroup{{CardIDs: []string{"x", "y"}}}}, nil
	})
	if len(failures) != 0 || len(prompts) != 3 || len(groups) != 3 {
		t.Fatalf("%d calls, %d groups, failures %v", len(prompts), len(groups), failures)
	}
	sizes := map[int]int{}
	for _, prompt := range prompts {
		if !strings.HasPrefix(prompt, "[WHOLE BOARD]") {
			t.Fatalf("a call did not carry the whole board:\n%s", prompt)
		}
		sizes[strings.Count(prompt, "- card_id=")]++
	}
	if sizes[50] != 2 || sizes[20] != 1 {
		t.Fatalf("batch sizes %v, want two of 50 and one of 20", sizes)
	}
	if mostInFlight.Load() > mergeCallsAtOnce {
		t.Fatalf("%d calls ran at once, more than %d", mostInFlight.Load(), mergeCallsAtOnce)
	}
}

func TestAFailedBatchIsReportedAndTheOthersStillCount(t *testing.T) {
	var cards []*cardState
	for index := 0; index < 60; index++ {
		cards = append(cards, &cardState{ID: fmt.Sprintf("card-%03d", index)})
	}
	groups, failures := proposeMergesInBatches(context.Background(), "", cards, func(_ context.Context, prompt string) (*mergeReport, error) {
		if strings.Contains(prompt, "card-055") {
			return nil, errors.New("operation timed out")
		}
		return &mergeReport{Merges: []mergeGroup{{CardIDs: []string{"a", "b"}}}}, nil
	})
	if len(groups) != 1 || len(failures) != 1 || failures[0].batchNumber != 2 {
		t.Fatalf("groups %v failures %+v", groups, failures)
	}
}

func TestGroupsSharingACardAreJoined(t *testing.T) {
	combined, refused := combineMergeGroups([]mergeGroup{
		{CardIDs: []string{"a", "b"}, SurvivorTitle: "Payment receipts", Category: "finance", Reason: "both receipts"},
		{CardIDs: []string{"b", "c"}, SurvivorTitle: "Payment receipts", Category: "finance", Reason: "receipts again"},
		{CardIDs: []string{"c", "d"}, SurvivorTitle: "Receipts", Category: "finance"},
		{CardIDs: []string{"x", "y"}, SurvivorCardID: "x", Reason: "newsletters"},
	})
	if len(refused) != 0 || len(combined) != 2 {
		t.Fatalf("combined %+v refused %v", combined, refused)
	}
	first, second := combined[0], combined[1]
	if strings.Join(first.CardIDs, ",") != "a,b,c,d" || first.SurvivorTitle != "Payment receipts" || first.Reason != "both receipts / receipts again" {
		t.Fatalf("first %+v", first)
	}
	if second.SurvivorCardID != "x" || strings.Join(second.CardIDs, ",") != "x,y" {
		t.Fatalf("second %+v", second)
	}
}

func TestCallsNamingDifferentSurvivorsForOneBucketAreRefused(t *testing.T) {
	combined, refused := combineMergeGroups([]mergeGroup{
		{CardIDs: []string{"a", "b"}, SurvivorCardID: "a"},
		{CardIDs: []string{"b", "c"}, SurvivorCardID: "c"},
		{CardIDs: []string{"p", "q"}, SurvivorCardID: "p"},
		{CardIDs: []string{"q", "r"}, SurvivorCardID: "p"},
	})
	if len(combined) != 1 || combined[0].SurvivorCardID != "p" || strings.Join(combined[0].CardIDs, ",") != "p,q,r" {
		t.Fatalf("combined %+v", combined)
	}
	if len(refused) != 1 || !strings.Contains(refused[0], "a, c") {
		t.Fatalf("refused %v", refused)
	}
}
