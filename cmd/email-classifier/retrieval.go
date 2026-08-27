package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ============================ similar filed mail ============================
//
// Worked examples for the model: for each email being classified, the stored
// messages that read most like it, together with the card each one actually
// landed on. The 2024-2026 retrieval literature validates exactly this shape —
// BM25 candidates, similarity-selected demonstrations for a classification
// LLM, deterministic recency decay — and it is the reason mailstack now
// records message bodies. (Full sources: noteboard note
// 72503642-f787-4f97-ab4b-576037346bb2.)
//
// ⚠️ THIS NEVER ROUTES. A retrieved example reaches the model as prompt text
// and nothing else; no code path files an email because of what it resembles.
// A sender-affinity rule that skipped the model was tried, filed a Wells Fargo
// security alert onto a marketing card unread, and was deleted — see
// routeByMessageIdentity. The guards below exist so its failure mode cannot
// grow back through this feature:
//
//   - Only messages whose RFC 5322 Message-ID is linked on a card count as
//     examples. That link is written exactly when a filed message was actually
//     fetched and read (refile.go uses the same fact as its discriminator), so
//     mail the retired sender rule filed unread — 298 of the board's 846 email
//     links when this was built — can never be cited as precedent. A wrong
//     label on the nearest neighbour is the highest-leverage error in this
//     design; those are the likeliest wrong labels, and they are excluded by
//     construction.
//   - At most two examples per card, so five examples cannot be five copies of
//     one bucket: near-miss mail with a different correct destination is the
//     documented distractor failure mode, and coverage of the plausible
//     destinations is what the examples are for.

// filedExample is one worked example: a stored message and where it went.
type filedExample struct {
	Subject   string
	FromEmail string
	Date      time.Time
	CardID    string
	CardTitle string
}

// similarExamplesPerEmail caps how many examples one email carries into the
// digest. The literature's answer for a label space this size is 10-20 per
// decision at most, with returns diminishing fast; five keeps a ten-email
// batch's example block smaller than the emails themselves.
const similarExamplesPerEmail = 5

// maxExamplesPerCard bounds how many of those may share one card.
const maxExamplesPerCard = 2

// recencyHalfLifeDays is the half-life of an example's weight. Ninety days:
// old precedent still counts — buckets are durable — but between two equally
// alike messages the one filed this month outweighs the one filed in spring.
const recencyHalfLifeDays = 90.0

// similarFiledExamples returns the worked examples for one email, best first.
func similarFiledExamples(mc *mailClient, state *boardState, em pendingEmail) ([]filedExample, error) {
	query := em.Meta.Subject + "\n" + em.Meta.From.Email + "\n" + bestBodyText(em)
	// Over-fetch: hits without a readable card link are dropped below, and the
	// per-card cap drops more.
	hits, err := mc.similarMessages(query, em.Meta.AccountID, em.Meta.ID, similarExamplesPerEmail*4)
	if err != nil {
		return nil, err
	}
	rankSimilarHitsWithRecencyDecay(hits, time.Now())

	var out []filedExample
	perCard := map[string]int{}
	for _, h := range hits {
		if h.MessageID == "" {
			continue
		}
		for _, cardID := range state.CardsByMessageID[h.MessageID] {
			card, ok := state.Cards[cardID]
			if !ok {
				continue
			}
			if perCard[cardID] >= maxExamplesPerCard {
				continue
			}
			perCard[cardID]++
			out = append(out, filedExample{
				Subject:   h.Subject,
				FromEmail: h.FromEmail,
				Date:      time.UnixMilli(h.DateMs),
				CardID:    cardID,
				CardTitle: card.Title,
			})
			if len(out) >= similarExamplesPerEmail {
				return out, nil
			}
		}
	}
	return out, nil
}

// rankSimilarHitsWithRecencyDecay reorders hits by bm25 goodness times an
// exponential age decay. Deterministic on purpose: the temporal-RAG results
// say to fuse recency into the score here, not to ask the model to weigh
// dates. bm25 scores are negative with lower better, so goodness is the
// negation; a hit with no date keeps its raw goodness rather than being
// invented an age.
func rankSimilarHitsWithRecencyDecay(hits []similarHit, now time.Time) {
	decayed := func(h similarHit) float64 {
		goodness := -h.Score
		if h.DateMs <= 0 {
			return goodness
		}
		ageDays := now.Sub(time.UnixMilli(h.DateMs)).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		return goodness * math.Exp2(-ageDays/recencyHalfLifeDays)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return decayed(hits[i]) > decayed(hits[j])
	})
}

// renderSimilarFiledForDigest writes one email's example block for the digest.
// Empty when there are no examples: an absent section reads as "no precedent",
// which is the truth, while an empty header would invite the model to wonder
// what should have been there.
func renderSimilarFiledForDigest(examples []filedExample) string {
	if len(examples) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("similar_previously_filed_emails:\n")
	for _, ex := range examples {
		subject := ex.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		fmt.Fprintf(&b, "  - %q from %s (%s) → filed on card_id=%s %q\n",
			truncate(subject, 70), ex.FromEmail, ex.Date.Format("Jan 2"), ex.CardID, truncate(ex.CardTitle, 70))
	}
	return b.String()
}
