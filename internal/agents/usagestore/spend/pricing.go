package spend

import "strings"

// Pricing holds per-million-token rates for one model. Cache rates derive from
// input via well-known multipliers (1.25x for 5m TTL writes, 2x for 1h TTL
// writes, 0.1x for cache reads), so we only store the canonical input/output
// pair and compute the rest at apply-time.
type Pricing struct {
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

// CacheWrite5mPerMTok is the 5-minute-TTL prompt-cache write rate.
func (p Pricing) CacheWrite5mPerMTok() float64 { return p.InputUSDPerMTok * 1.25 }

// CacheWrite1hPerMTok is the 1-hour-TTL prompt-cache write rate.
func (p Pricing) CacheWrite1hPerMTok() float64 { return p.InputUSDPerMTok * 2.0 }

// CacheReadPerMTok is the prompt-cache read rate (90% discount on input).
func (p Pricing) CacheReadPerMTok() float64 { return p.InputUSDPerMTok * 0.10 }

// anthropicPricing maps a canonical model family to its rates. Match logic
// strips the date suffix (e.g. "-20250929") and tries the most specific
// substring first. Numbers from anthropic.com/pricing as of late 2026; bump
// when prices change.
var anthropicPricing = []struct {
	match   string
	pricing Pricing
}{
	// Opus tier — flat $15/$75 across versions.
	{"claude-opus-4-7", Pricing{15, 75}},
	{"claude-opus-4-6", Pricing{15, 75}},
	{"claude-opus-4-5", Pricing{15, 75}},
	{"claude-opus-4-1", Pricing{15, 75}},
	{"claude-opus-4", Pricing{15, 75}},
	{"claude-3-opus", Pricing{15, 75}},
	{"opus", Pricing{15, 75}}, // catch-all last

	// Sonnet tier — $3/$15 across versions.
	{"claude-sonnet-4-7", Pricing{3, 15}},
	{"claude-sonnet-4-6", Pricing{3, 15}},
	{"claude-sonnet-4-5", Pricing{3, 15}},
	{"claude-sonnet-4", Pricing{3, 15}},
	{"claude-3-7-sonnet", Pricing{3, 15}},
	{"claude-3-5-sonnet", Pricing{3, 15}},
	{"claude-3-sonnet", Pricing{3, 15}},
	{"sonnet", Pricing{3, 15}},

	// Haiku tier.
	{"claude-haiku-4-5", Pricing{1, 5}},
	{"claude-3-5-haiku", Pricing{0.80, 4}},
	{"claude-haiku-3-5", Pricing{0.80, 4}},
	{"claude-3-haiku", Pricing{0.25, 1.25}},
	{"haiku", Pricing{0.25, 1.25}},
}

// LookupAnthropic returns pricing for a model ID and ok=false if unknown.
// Caller should treat unknown models as a cost of zero AND log loudly so the
// user sees the gap rather than silent under-reporting.
func LookupAnthropic(model string) (Pricing, bool) {
	if model == "" {
		return Pricing{}, false
	}
	m := strings.ToLower(model)
	for _, e := range anthropicPricing {
		if strings.Contains(m, e.match) {
			return e.pricing, true
		}
	}
	return Pricing{}, false
}
