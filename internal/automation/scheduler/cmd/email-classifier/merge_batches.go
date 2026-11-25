package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ============================ merge pass in batches ============================
//
// One call over the whole board stopped fitting: at 180 cards it ran past five
// minutes, and its answer grows with the board until it is cut off at the
// output limit. So each call still reads the whole board — a duplicate can
// only be seen against everything else — but is asked about fifty cards, and
// proposes only groups holding at least one of them. The calls' groups are
// then combined into one set before anything is applied.

const (
	// mergeCardsPerCall is how many cards one call is asked about.
	mergeCardsPerCall = 50
	// mergeCallsAtOnce bounds the calls in flight. llm-bridge-server runs four
	// operations at a time; more would only queue there.
	mergeCallsAtOnce = 4
)

// mergeBatchInstructions is added to the system prompt for a batched call.
const mergeBatchInstructions = `

You are shown EVERY card on the board, and then a shorter list of CARDS TO CONSIDER. Propose only groups that contain at least one card to consider. A group may include any other card on the board. Other calls are asked about the other cards, so do not look for groups that contain none of yours.`

// cardsAMergeCouldTouch are the cards worth asking about: applyMerge refuses a
// group holding a finished card, a task or an agent-held card, so asking about
// those spends tokens on answers that are then refused. They stay in the
// digest, as context.
func cardsAMergeCouldTouch(state *boardState, gate *sessionGate) []*cardState {
	var cards []*cardState
	for _, c := range state.Cards {
		if c.ColumnName == columnActionCompleted || active.ActionsNeedingWork[c.Action] {
			continue
		}
		if owned, _ := gate.agentOwns(c); owned {
			continue
		}
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].ID < cards[j].ID })
	return cards
}

// mergeBatchPrompt is one call's user prompt: the whole board, then its cards.
func mergeBatchPrompt(digest string, batch []*cardState) string {
	var b strings.Builder
	b.WriteString(digest)
	b.WriteString("\n[CARDS TO CONSIDER IN THIS CALL]\n")
	for _, c := range batch {
		fmt.Fprintf(&b, "- card_id=%s | %s\n", c.ID, c.Title)
	}
	return b.String()
}

// askAboutMerges makes one merge call with a user prompt.
type askAboutMerges func(ctx context.Context, prompt string) (*mergeReport, error)

// mergeBatchFailure is one call that did not answer.
type mergeBatchFailure struct {
	batchNumber int
	err         error
}

// proposeMergesInBatches asks about cards in batches of mergeCardsPerCall,
// mergeCallsAtOnce at a time, and returns every group proposed plus the
// batches that failed. A failed batch does not stop the others: merges are
// independent, and the ones found are still good.
func proposeMergesInBatches(ctx context.Context, digest string, cards []*cardState, ask askAboutMerges) ([]mergeGroup, []mergeBatchFailure) {
	var batches [][]*cardState
	for start := 0; start < len(cards); start += mergeCardsPerCall {
		batches = append(batches, cards[start:min(start+mergeCardsPerCall, len(cards))])
	}
	groupsByBatch := make([][]mergeGroup, len(batches))
	failuresByBatch := make([]error, len(batches))
	slots := make(chan struct{}, mergeCallsAtOnce)
	var wait sync.WaitGroup
	for index, batch := range batches {
		wait.Add(1)
		go func() {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			report, err := ask(ctx, mergeBatchPrompt(digest, batch))
			if err != nil {
				failuresByBatch[index] = err
				return
			}
			groupsByBatch[index] = report.Merges
		}()
	}
	wait.Wait()
	var groups []mergeGroup
	var failures []mergeBatchFailure
	for index := range batches {
		if failuresByBatch[index] != nil {
			failures = append(failures, mergeBatchFailure{batchNumber: index + 1, err: failuresByBatch[index]})
			continue
		}
		groups = append(groups, groupsByBatch[index]...)
	}
	return groups, failures
}

// combineMergeGroups joins groups that share a card, since two calls can
// each see one side of the same bucket. A joined group keeps the one existing
// survivor its parts name; parts naming different existing survivors
// disagree about which card the bucket is, and the group is refused and
// returned in refused rather than guessed. With no existing survivor, the
// title, category, action and urgency most parts give win, ties going to the
// first alphabetically.
func combineMergeGroups(groups []mergeGroup) (combined []mergeGroup, refused []string) {
	parent := map[string]string{}
	var find func(string) string
	find = func(id string) string {
		if parent[id] == id {
			return id
		}
		parent[id] = find(parent[id])
		return parent[id]
	}
	for _, g := range groups {
		for _, id := range g.CardIDs {
			if _, known := parent[id]; !known {
				parent[id] = id
			}
		}
		for _, id := range g.CardIDs[1:] {
			if a, b := find(g.CardIDs[0]), find(id); a != b {
				parent[b] = a
			}
		}
	}
	partsByRoot := map[string][]mergeGroup{}
	var roots []string
	for _, g := range groups {
		if len(g.CardIDs) == 0 {
			continue
		}
		root := find(g.CardIDs[0])
		if _, seen := partsByRoot[root]; !seen {
			roots = append(roots, root)
		}
		partsByRoot[root] = append(partsByRoot[root], g)
	}
	for _, root := range roots {
		parts := partsByRoot[root]
		cardIDs := map[string]bool{}
		survivors := map[string]bool{}
		titles, categories, actions, urgencies := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
		var reasons []string
		for _, part := range parts {
			for _, id := range part.CardIDs {
				cardIDs[id] = true
			}
			if part.SurvivorCardID != "" {
				survivors[part.SurvivorCardID] = true
			}
			count(titles, strings.TrimSpace(part.SurvivorTitle))
			count(categories, part.Category)
			count(actions, part.Action)
			count(urgencies, part.Urgency)
			if part.Reason != "" && !containsString(reasons, part.Reason) {
				reasons = append(reasons, part.Reason)
			}
		}
		joined := mergeGroup{
			CardIDs: sortedKeys(cardIDs), SurvivorTitle: mostCommon(titles), Category: mostCommon(categories),
			Action: mostCommon(actions), Urgency: mostCommon(urgencies), Reason: strings.Join(reasons, " / "),
		}
		if len(survivors) > 1 {
			refused = append(refused, fmt.Sprintf("cards %s: the calls named different survivors %s", strings.Join(joined.CardIDs, ","), strings.Join(sortedKeys(survivors), ", ")))
			continue
		}
		for survivor := range survivors {
			joined.SurvivorCardID = survivor
		}
		combined = append(combined, joined)
	}
	return combined, refused
}

func count(counts map[string]int, value string) {
	if value != "" {
		counts[value]++
	}
}

func mostCommon(counts map[string]int) string {
	best, bestCount := "", 0
	for _, value := range sortedKeys(counts) {
		if counts[value] > bestCount {
			best, bestCount = value, counts[value]
		}
	}
	return best
}

func sortedKeys[V any](set map[string]V) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
