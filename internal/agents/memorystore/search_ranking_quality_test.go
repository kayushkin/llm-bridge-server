package memorystore

import (
	"math"
	"sort"
	"testing"
	"time"
)

// This file is the measurement behind the switch from a hashed bag of words to
// BM25 (noteboard todo 0c31b497-0418-4577-b156-6699918ef5a8). It keeps BOTH
// scorers runnable against one labeled corpus, so the claim "this is better"
// is a number this suite re-derives rather than a sentence in a commit message.
//
// The old scorer is not a reconstruction from memory: Embedder and
// CosineSimilarity are still in the package, and rankByHashedBagOfWords below
// applies them in exactly the blend search.go used to — similarity × importance
// × 0.99^daysSinceAccess, sorted descending, ties broken on id.

// rankedCase is one query and the memories a reader asking it wants back.
//
// `relevant` is judged from the content alone: a memory is relevant when it is
// about the thing asked for. No judgment here refers to either scorer.
type rankedCase struct {
	query    string
	relevant []string
	why      string
}

// rankingCorpus is written in this box's own vocabulary because that is the
// text these stores actually hold — service names, ports, Go identifiers and
// ordinary English around them.
func rankingCorpus() []Memory {
	docs := []struct{ id, content string }{
		{"sched-restart", "the scheduler kills a job's whole process group when it exceeds timeout_seconds"},
		{"sched-cron", "scheduling a job on :8092 uses a five field cron expression parsed by robfig"},
		{"sched-patch", "PATCH on a scheduler job accepts every field and reloads the schedule on the next tick"},
		{"note-fts", "noteboard stores notes todos and ranked lists in sqlite with an fts5 index"},
		{"note-hold", "a held noteboard item is withheld from the default listing until the hold is cleared"},
		{"note-revisions", "every noteboard update writes a snapshot of eight columns to item_revisions"},
		{"auth-oauth", "auth-store refreshes stored oauth tokens server side and keeps an audit log"},
		{"auth-resolve", "dash resolves its google calendar credential from auth-store before proxying"},
		{"plex-proxy", "videostack proxies the plex library api and caches thumbnails on disk"},
		{"plex-refresh", "refreshing a plex library section re-reads every item and rebuilds the cache"},
		{"bus-nats", "the bus persists published messages through nats jetstream and replays them on subscribe"},
		{"bus-ws", "a websocket subscriber receives bus messages in real time without polling"},
		{"mail-imap", "mailstack reads privateemail over imap and writes outbound mail over smtp"},
		{"mail-graph", "outlook accounts go through the microsoft graph api rather than imap"},
		{"forge-slot", "forge manages isolated git worktree slots tied to an agent session"},
		{"forge-preview", "a preview deployment is built from a forge slot and torn down with it"},
		{"harness-spawn", "llm-bridge-server spawns a harness subprocess and streams its events over sse"},
		{"harness-resume", "claude code sessions are resumed by replaying the rollout file with --resume"},
		{"log-jsonl", "logstack writes one jsonl file per date and source in a unified json format"},
		{"log-forward", "log-store materializes message history and forwards events to logstack"},
		{"health-systemd", "healthcheck watches systemd units and reports version drift between binaries"},
		{"health-http", "an http check posts to the endpoint and records the status code it got back"},
		{"skill-ingest", "skill-store ingests a skill by walking a directory for SKILL.md files"},
		{"tool-provision", "tool-store provision returns mcp config with env values resolved from auth-store"},
		{"repo-detect", "repodetect reports the languages frameworks build tools and test tools a repo uses"},
		{"bundle-resolve", "bundle-store composes a session bundle from skill ids and tool ids by repo tags"},
		{"kanban-place", "kanban-store records where a card sits and treats noteboard as the card's content"},
		{"event-radar", "event-store holds sources to watch and the candidate events they yield"},
		{"quote-attrib", "a quote points at its author and its work by id so a rename moves every quote"},
		{"job-listings", "job-store keeps sources listings documents and the applications made from them"},
		{"marg-graph", "marginalia extracts people places and artifacts from a chapter into a local graph"},
		{"book-import", "bookstack imports from an inbox and converts formats with calibre"},
		{"down-queue", "downloadstack searches several media sources and falls back between them on retry"},
		{"si-route", "si routes messages between matterbridge discord telegram and the inber io feeds"},
		{"multi-matrix", "multichat bridges telegram whatsapp and signal into synapse with mautrix"},
		{"android-ptt", "the watch sends push to talk audio to the phone over the message client"},
		{"argraph-tree", "argraphments turns a transcript into an argument tree with whisper and claude"},
		{"midi-tab", "miditab converts a midi file into ascii tablature for guitar bass and ukulele"},
		{"dmv-cli", "renodmv lists reno dmv appointment types and deep links to the booking page"},
		{"north-search", "northwind-api serves a product catalog search with cursor paging"},
	}

	stamped := time.Now()
	memories := make([]Memory, 0, len(docs))
	for _, d := range docs {
		memories = append(memories, Memory{
			ID:           d.id,
			Content:      d.content,
			Importance:   0.5,
			Source:       "test",
			CreatedAt:    stamped,
			LastAccessed: stamped,
		})
	}
	return memories
}

func rankingCases() []rankedCase {
	return []rankedCase{
		{
			query:    "scheduler job timeout",
			relevant: []string{"sched-restart", "sched-cron", "sched-patch"},
			why:      "everything about the scheduler's jobs",
		},
		{
			query:    "plex library caching",
			relevant: []string{"plex-proxy", "plex-refresh"},
			why:      "both plex memories",
		},
		{
			query:    "oauth token refresh",
			relevant: []string{"auth-oauth"},
			why:      "only auth-oauth mentions refreshing tokens",
		},
		{
			query:    "scheduling a recurring cron",
			relevant: []string{"sched-cron"},
			why:      "stemming: 'scheduling' has to reach 'scheduling'/'scheduler'",
		},
		{
			query:    "imap smtp mail",
			relevant: []string{"mail-imap", "mail-graph"},
			why:      "the two mailstack memories",
		},
		{
			query:    "git worktree slot",
			relevant: []string{"forge-slot", "forge-preview"},
			why:      "forge owns worktree slots",
		},
		{
			query:    "sqlite full text index",
			relevant: []string{"note-fts"},
			why:      "only noteboard's memory names an fts index",
		},
		{
			query:    "websocket subscriber",
			relevant: []string{"bus-ws"},
			why:      "one memory is about websocket subscription",
		},
		{
			query:    "resolve credentials from auth-store",
			relevant: []string{"auth-resolve", "auth-oauth", "tool-provision"},
			why:      "the three memories that name auth-store as the credential source",
		},
		{
			query:    "matterbridge discord telegram",
			relevant: []string{"si-route", "multi-matrix"},
			why:      "the two chat-bridging memories",
		},
	}
}

// rankByHashedBagOfWords is the scorer search.go used before this change, kept
// runnable so the comparison below is against the real thing.
func rankByHashedBagOfWords(memories []Memory, query string, limit int) []string {
	embedder := NewEmbedder()
	queryEmbedding := embedder.Embed(query)
	now := time.Now()

	type scored struct {
		id    string
		score float64
	}
	ranked := make([]scored, 0, len(memories))
	for _, m := range memories {
		similarity := CosineSimilarity(queryEmbedding, embedder.Embed(m.Content))
		daysSinceAccess := now.Sub(m.LastAccessed).Hours() / 24
		recencyBoost := math.Pow(recencyDecayPerDay, daysSinceAccess)
		ranked = append(ranked, scored{id: m.ID, score: similarity * m.Importance * recencyBoost})
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].id < ranked[j].id
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.id
	}
	return out
}

// precisionOverReturned is the share of the results actually handed back that
// are relevant.
//
// It is the metric that separates these two retrievers, and P@k is not, for a
// reason worth stating: the old scorer scores every memory in the store, so it
// ALWAYS returns k results and pads the tail with whatever scored least badly.
// BM25 returns only documents that matched, so it often returns fewer than k.
// P@k divides by k either way, which charges BM25 for declining to pad — it
// scores 0.33 for returning one perfect result when k is 3. Recall@k below is
// what stops abstention being free.
func precisionOverReturned(got []string, relevant []string) float64 {
	if len(got) == 0 {
		return 0
	}
	wanted := make(map[string]bool, len(relevant))
	for _, id := range relevant {
		wanted[id] = true
	}
	hits := 0
	for _, id := range got {
		if wanted[id] {
			hits++
		}
	}
	return float64(hits) / float64(len(got))
}

// recallAtK is the share of the relevant memories that made the first k.
func recallAtK(got []string, relevant []string, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	if len(got) > k {
		got = got[:k]
	}
	found := make(map[string]bool, len(got))
	for _, id := range got {
		found[id] = true
	}
	hits := 0
	for _, id := range relevant {
		if found[id] {
			hits++
		}
	}
	return float64(hits) / float64(len(relevant))
}

// reciprocalRank is 1/position of the first relevant result, and 0 if none of
// them made the list.
func reciprocalRank(got []string, relevant []string) float64 {
	wanted := make(map[string]bool, len(relevant))
	for _, id := range relevant {
		wanted[id] = true
	}
	for i, id := range got {
		if wanted[id] {
			return 1 / float64(i+1)
		}
	}
	return 0
}

func storeWithRankingCorpus(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })
	for _, m := range rankingCorpus() {
		if err := s.Save(m); err != nil {
			t.Fatalf("save %s: %v", m.ID, err)
		}
	}
	return s
}

// TestBM25OutranksTheHashedBagOfWords is the measurement the switch was made
// on. It scores both retrievers over the same labeled corpus and fails if the
// new one is not ahead.
//
// ⚠️ MRR is measured and reported and deliberately NOT asserted on, and this is
// the most useful thing the first run found: both retrievers score MRR 1.000.
// On all ten queries the hashed bag of words also puts a relevant memory FIRST.
// So the placeholder was never bad at finding the single best match — it is bad
// at everything after it, padding the rest of the budget with memories that
// merely collided in a hash bucket. A metric saturated at 1.000 for both arms
// cannot fail, and asserting on one would be a green light wired to nothing.
//
// The assertions are on the two metrics that do separate them, and both have to
// hold: precision alone would reward a retriever that answers almost nothing,
// and recall alone would reward one that returns the whole store.
//
// Deliberately inequalities and not pinned numbers: the corpus will grow, and a
// test pinning 0.833 fails on a corpus edit that changed neither retriever.
func TestBM25OutranksTheHashedBagOfWords(t *testing.T) {
	s := storeWithRankingCorpus(t)
	corpus := rankingCorpus()
	cases := rankingCases()

	const k = 3
	var bm25Precision, bm25Recall, bm25MRR float64
	var hashedPrecision, hashedRecall, hashedMRR float64

	for _, c := range cases {
		results, err := s.Search(c.query, k)
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		bm25IDs := idsOf(results)
		hashedIDs := rankByHashedBagOfWords(corpus, c.query, k)

		bm25P := precisionOverReturned(bm25IDs, c.relevant)
		hashedP := precisionOverReturned(hashedIDs, c.relevant)
		bm25R := recallAtK(bm25IDs, c.relevant, k)
		hashedR := recallAtK(hashedIDs, c.relevant, k)

		bm25Precision += bm25P
		hashedPrecision += hashedP
		bm25Recall += bm25R
		hashedRecall += hashedR
		bm25MRR += reciprocalRank(bm25IDs, c.relevant)
		hashedMRR += reciprocalRank(hashedIDs, c.relevant)

		t.Logf("%-38q precision bm25=%.2f hashed=%.2f  recall@%d bm25=%.2f hashed=%.2f  (%s)\n    bm25:   %v\n    hashed: %v",
			c.query, bm25P, hashedP, k, bm25R, hashedR, c.why, bm25IDs, hashedIDs)
	}

	n := float64(len(cases))
	bm25Precision /= n
	hashedPrecision /= n
	bm25Recall /= n
	hashedRecall /= n
	bm25MRR /= n
	hashedMRR /= n

	t.Logf("MEAN over %d queries: precision bm25=%.3f hashed=%.3f | recall@%d bm25=%.3f hashed=%.3f | MRR (reported, not asserted) bm25=%.3f hashed=%.3f",
		len(cases), bm25Precision, hashedPrecision, k, bm25Recall, hashedRecall, bm25MRR, hashedMRR)

	if bm25Precision <= hashedPrecision {
		t.Errorf("BM25 precision %.3f does not beat the hashed bag of words %.3f — the change bought nothing",
			bm25Precision, hashedPrecision)
	}
	if bm25Recall < hashedRecall {
		t.Errorf("BM25 recall@%d %.3f is WORSE than the hashed bag of words %.3f — precision was bought by answering less",
			k, bm25Recall, hashedRecall)
	}
}

// TestRankingMetricsCanReportAFailure is the control for the metrics above.
//
// A comparison that always says "better" is not a measurement. Each metric is
// driven with rankings whose right answers are known by construction, in both
// directions, so a metric returning a constant — or one with its arguments the
// wrong way round — cannot pass this.
func TestRankingMetricsCanReportAFailure(t *testing.T) {
	relevant := []string{"a", "b"}

	perfect := []string{"a", "b"}
	half := []string{"a", "x"}
	useless := []string{"x", "y", "z"}

	if got := precisionOverReturned(perfect, relevant); got != 1 {
		t.Errorf("precisionOverReturned on an all-relevant ranking = %v, want 1", got)
	}
	if got := precisionOverReturned(half, relevant); got != 0.5 {
		t.Errorf("precisionOverReturned on a half-relevant ranking = %v, want 0.5", got)
	}
	if got := precisionOverReturned(useless, relevant); got != 0 {
		t.Errorf("precisionOverReturned on an irrelevant ranking = %v, want 0", got)
	}
	if got := precisionOverReturned(nil, relevant); got != 0 {
		t.Errorf("precisionOverReturned on an empty ranking = %v, want 0", got)
	}

	if got := recallAtK(perfect, relevant, 3); got != 1 {
		t.Errorf("recallAtK with both relevant present = %v, want 1", got)
	}
	if got := recallAtK(half, relevant, 3); got != 0.5 {
		t.Errorf("recallAtK with one relevant present = %v, want 0.5", got)
	}
	if got := recallAtK(useless, relevant, 3); got != 0 {
		t.Errorf("recallAtK with neither present = %v, want 0", got)
	}
	// The cut at k has to be applied, or recall reads a result the caller was
	// never handed as though it had been.
	if got := recallAtK([]string{"x", "y", "a", "b"}, relevant, 2); got != 0 {
		t.Errorf("recallAtK counted relevant results below the cut = %v, want 0", got)
	}

	if got := reciprocalRank(perfect, relevant); got != 1 {
		t.Errorf("reciprocalRank with a relevant result first = %v, want 1", got)
	}
	if got := reciprocalRank([]string{"x", "a"}, relevant); got != 0.5 {
		t.Errorf("reciprocalRank with a relevant result second = %v, want 0.5", got)
	}
	if got := reciprocalRank(useless, relevant); got != 0 {
		t.Errorf("reciprocalRank with nothing relevant = %v, want 0", got)
	}

	// And the comparison itself has to be able to come out the other way: a
	// fixed irrelevant ranking must lose to the hashed bag of words on both
	// metrics, on every query in the case list.
	corpus := rankingCorpus()
	junk := []string{"midi-tab", "dmv-cli", "north-search"}
	for _, c := range rankingCases() {
		hashed := rankByHashedBagOfWords(corpus, c.query, 3)
		if precisionOverReturned(junk, c.relevant) > precisionOverReturned(hashed, c.relevant) {
			t.Errorf("%q: a fixed irrelevant ranking beat the hashed bag of words on precision — the metric is not measuring relevance", c.query)
		}
		if recallAtK(junk, c.relevant, 3) > recallAtK(hashed, c.relevant, 3) {
			t.Errorf("%q: a fixed irrelevant ranking beat the hashed bag of words on recall", c.query)
		}
	}
}
