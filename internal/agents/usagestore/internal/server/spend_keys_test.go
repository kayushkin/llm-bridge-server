package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	usagestore "github.com/kayushkin/usage-store"
	"github.com/kayushkin/usage-store/internal/config"
	"github.com/kayushkin/usage-store/spend"
)

// The /api/usage/spend/keys mechanism is what the Usage page reads to answer
// "what has this cost and what credit is left". Measured on `main` with a
// panic() at function entry, every function exercised here left `go test ./...`
// green — the census named assembleProvider and nothing executed any of them.

func newSpendServer(t *testing.T) (*Server, *usagestore.Store) {
	t.Helper()
	store, err := usagestore.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store, filepath.Join(t.TempDir(), "tokens.db"), nil, nil, nil, time.Hour, SpendCollectors{}, emptySettingsRegistry(t)), store
}

// newConfiguredSpendServer carries a non-nil Anthropic collector, which is the
// only thing that makes the anthropic account report itself as configured.
func newConfiguredSpendServer(t *testing.T) (*Server, *usagestore.Store) {
	t.Helper()
	store, err := usagestore.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	collectors := SpendCollectors{Anthropic: &spend.AnthropicKeyCollector{}}
	return New(store, filepath.Join(t.TempDir(), "tokens.db"), nil, nil, nil, time.Hour, collectors, emptySettingsRegistry(t)), store
}

// emptySettingsRegistry is the registry New requires, built from an empty
// environment: these tests exercise spend, not configuration.
func emptySettingsRegistry(t *testing.T) *servicesettings.Registry {
	t.Helper()
	registry, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatalf("settings registry: %v", err)
	}
	return registry
}

func daysAgoDate(n int) string {
	return time.Now().UTC().AddDate(0, 0, -n).Format("2006-01-02")
}

func saveSpend(t *testing.T, store *usagestore.Store, provider, keyID, date string, usd float64) {
	t.Helper()
	err := store.SaveDailySpend(usagestore.DailySpend{
		Provider: provider, APIKeyID: keyID, Date: date, TotalUSD: usd, FetchedAt: 1,
	})
	if err != nil {
		t.Fatalf("SaveDailySpend: %v", err)
	}
}

func saveMeta(t *testing.T, store *usagestore.Store, provider, keyID, name string) {
	t.Helper()
	err := store.SaveKeyMeta(usagestore.KeyMeta{
		Provider: provider, APIKeyID: keyID, APIKeyName: name,
		APIKeyHint: "sk-..." + keyID, APIKeyStatus: "active", FetchedAt: 1,
	})
	if err != nil {
		t.Fatalf("SaveKeyMeta: %v", err)
	}
}

func do(t *testing.T, srv *Server, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func getSpendKeys(t *testing.T, srv *Server) SpendKeysResponse {
	t.Helper()
	rec := do(t, srv, "GET", "/api/usage/spend/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET spend/keys: %d %s", rec.Code, rec.Body.String())
	}
	var resp SpendKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	return resp
}

// ---- which account is configured ----

func TestSpendKeysReportsAnthropicConfiguredOnlyWhenACollectorExists(t *testing.T) {
	// "Configured" is what the UI uses to decide between showing spend and
	// showing the connect-an-admin-key empty state, so a hardcoded true would
	// leave a box with no credential reporting $0 as if that were measured.
	unconfigured, _ := newSpendServer(t)
	if got := getSpendKeys(t, unconfigured).Anthropic.Configured; got {
		t.Fatal("no collector was supplied and anthropic still reports configured")
	}

	configured, _ := newConfiguredSpendServer(t)
	if got := getSpendKeys(t, configured).Anthropic.Configured; !got {
		t.Fatal("a collector was supplied and anthropic reports unconfigured")
	}
}

func TestSpendKeysReportsOpenAIUnconfiguredEvenWithAnAnthropicCollector(t *testing.T) {
	// There is no OpenAI collector at all yet. The flag must not track its
	// neighbor, or the page offers a refresh nothing implements.
	srv, _ := newConfiguredSpendServer(t)
	if getSpendKeys(t, srv).OpenAI.Configured {
		t.Fatal("openai reports configured off the back of anthropic's collector")
	}
}

func TestSpendKeysAnswersBothProvidersUnderTheirOwnNames(t *testing.T) {
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "anthropic key")
	saveMeta(t, store, "openai", "key_o", "openai key")
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(0), 3)
	saveSpend(t, store, "openai", "key_o", daysAgoDate(0), 7)

	resp := getSpendKeys(t, srv)
	if len(resp.Anthropic.Keys) != 1 || resp.Anthropic.Keys[0].APIKeyID != "key_a" {
		t.Fatalf("anthropic carries the wrong keys: %+v", resp.Anthropic.Keys)
	}
	if len(resp.OpenAI.Keys) != 1 || resp.OpenAI.Keys[0].APIKeyID != "key_o" {
		t.Fatalf("openai carries the wrong keys: %+v", resp.OpenAI.Keys)
	}
	if resp.Anthropic.Total30d != 3 || resp.OpenAI.Total30d != 7 {
		t.Fatalf("the two providers' totals are crossed: anthropic %v openai %v",
			resp.Anthropic.Total30d, resp.OpenAI.Total30d)
	}
}

// ---- the admin-key hint ----

func TestSpendKeysCarriesTheAdminHintSavedByTheLastFetch(t *testing.T) {
	srv, _ := newSpendServer(t)
	if hint := getSpendKeys(t, srv).Anthropic.AdminKeyHint; hint != "" {
		t.Fatalf("a hint appeared before any fetch: %q", hint)
	}

	srv.SaveAdminHint("anthropic", "sk-ant-admin...xyz")

	if hint := getSpendKeys(t, srv).Anthropic.AdminKeyHint; hint != "sk-ant-admin...xyz" {
		t.Fatalf("want the saved hint, got %q", hint)
	}
}

func TestSaveAdminHintKeepsProvidersApart(t *testing.T) {
	srv, _ := newSpendServer(t)
	srv.SaveAdminHint("anthropic", "anthropic-hint")

	resp := getSpendKeys(t, srv)
	if resp.OpenAI.AdminKeyHint != "" {
		t.Fatalf("anthropic's hint leaked onto openai: %q", resp.OpenAI.AdminKeyHint)
	}
}

func TestSaveAdminHintReplacesTheHintFromTheFetchBefore(t *testing.T) {
	// A rotated admin key must not leave the retired one on screen.
	srv, _ := newSpendServer(t)
	srv.SaveAdminHint("anthropic", "old")
	srv.SaveAdminHint("anthropic", "new")

	if hint := getSpendKeys(t, srv).Anthropic.AdminKeyHint; hint != "new" {
		t.Fatalf("want the latest hint, got %q", hint)
	}
}

// ---- the three windows ----

func TestSpendKeysSortsOneKeysDaysIntoTheThreeWindows(t *testing.T) {
	// 24h is UTC-day aligned to match how spend_daily is keyed, so it spans
	// today and yesterday; 7d and 30d are the same shape one cutoff further
	// back. A day landing in the wrong bucket makes the page's headline number
	// wrong in a way no error surfaces.
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "the key")
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(0), 1)
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(3), 10)
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(20), 100)
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(40), 1000)

	account := getSpendKeys(t, srv).Anthropic
	if account.Total24h != 1 {
		t.Fatalf("24h window: got %v, want only today's 1", account.Total24h)
	}
	if account.Total7d != 11 {
		t.Fatalf("7d window: got %v, want 1+10", account.Total7d)
	}
	if account.Total30d != 111 {
		t.Fatalf("30d window: got %v, want 1+10+100", account.Total30d)
	}
}

func TestSpendKeysCountsYesterdayInsideTheTwentyFourHourWindow(t *testing.T) {
	// The cutoff is yesterday's UTC midnight, not now minus 24 hours. Pinned
	// because it looks like an off-by-one and is the documented intent.
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "the key")
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(1), 5)

	if got := getSpendKeys(t, srv).Anthropic.Total24h; got != 5 {
		t.Fatalf("yesterday fell outside the 24h window: got %v, want 5", got)
	}
}

func TestSpendKeysGivesEachKeyItsOwnWindowTotals(t *testing.T) {
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "alpha")
	saveMeta(t, store, "anthropic", "key_b", "bravo")
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(0), 2)
	saveSpend(t, store, "anthropic", "key_b", daysAgoDate(3), 40)

	rows := map[string]KeyRow{}
	for _, row := range getSpendKeys(t, srv).Anthropic.Keys {
		rows[row.APIKeyID] = row
	}
	if rows["key_a"].Total24h != 2 || rows["key_a"].Total7d != 2 || rows["key_a"].Total30d != 2 {
		t.Fatalf("key_a's windows are wrong: %+v", rows["key_a"])
	}
	if rows["key_b"].Total24h != 0 {
		t.Fatalf("key_b spent nothing in 24h and reports %v", rows["key_b"].Total24h)
	}
	if rows["key_b"].Total7d != 40 || rows["key_b"].Total30d != 40 {
		t.Fatalf("key_b's windows are wrong: %+v", rows["key_b"])
	}
}

func TestSpendKeysAccountTotalIsTheSumOfItsKeyRows(t *testing.T) {
	// The header and the rows beneath it are two computations of one figure. If
	// they disagree the page shows its own arithmetic failing.
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "alpha")
	saveMeta(t, store, "anthropic", "key_b", "bravo")
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(0), 2)
	saveSpend(t, store, "anthropic", "key_b", daysAgoDate(0), 3)

	account := getSpendKeys(t, srv).Anthropic
	var sum24, sum7, sum30 float64
	for _, row := range account.Keys {
		sum24 += row.Total24h
		sum7 += row.Total7d
		sum30 += row.Total30d
	}
	if account.Total24h != sum24 || account.Total7d != sum7 || account.Total30d != sum30 {
		t.Fatalf("header (%v/%v/%v) does not match the rows (%v/%v/%v)",
			account.Total24h, account.Total7d, account.Total30d, sum24, sum7, sum30)
	}
	if account.Total24h != 5 {
		t.Fatalf("want 2+3=5, got %v", account.Total24h)
	}
}

func TestSpendKeysCarriesEveryColumnOfAKeysMetadata(t *testing.T) {
	srv, store := newSpendServer(t)
	err := store.SaveKeyMeta(usagestore.KeyMeta{
		Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "nightly worker",
		APIKeyHint: "sk-ant...9f2", APIKeyStatus: "archived", FetchedAt: 1699999999,
	})
	if err != nil {
		t.Fatalf("SaveKeyMeta: %v", err)
	}

	keys := getSpendKeys(t, srv).Anthropic.Keys
	if len(keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(keys))
	}
	got := keys[0]
	want := KeyRow{
		APIKeyID: "key_a", APIKeyName: "nightly worker", APIKeyHint: "sk-ant...9f2",
		APIKeyStatus: "archived", FetchedAt: 1699999999,
	}
	if got != want {
		t.Fatalf("key row is %+v, want %+v", got, want)
	}
}

func TestSpendKeysListsAKeyThatHasSpentNothing(t *testing.T) {
	// A key with metadata and no daily rows must still appear, at zero. Drop it
	// and a newly issued key is invisible until its first dollar.
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_idle", "idle")

	keys := getSpendKeys(t, srv).Anthropic.Keys
	if len(keys) != 1 || keys[0].APIKeyID != "key_idle" {
		t.Fatalf("the idle key is missing: %+v", keys)
	}
	if keys[0].Total30d != 0 {
		t.Fatalf("the idle key reports spend: %v", keys[0].Total30d)
	}
}

func TestSpendKeysAccountTotalsIgnoreSpendWhoseKeyHasNoMetadata(t *testing.T) {
	// Characterisation, not an endorsement. The window totals are accumulated
	// while walking ListKeyMeta, so a spend_daily row whose key was never saved
	// is money spent that the account header cannot see. The baseline figure
	// below is computed provider-wide and DOES see it, so the same response can
	// report $0 spent and a reduced balance at once.
	srv, store := newSpendServer(t)
	saveSpend(t, store, "anthropic", "key_never_saved", daysAgoDate(0), 25)
	if _, err := store.AddTopup(usagestore.Topup{
		Provider: "anthropic", AmountUSD: 100, OccurredAt: time.Now().AddDate(0, 0, -2).Unix(),
	}); err != nil {
		t.Fatalf("AddTopup: %v", err)
	}

	account := getSpendKeys(t, srv).Anthropic
	if account.Total30d != 0 {
		t.Fatalf("the header now sees metadata-less spend: %v — good, but the "+
			"comment above and the assertion below need rewriting", account.Total30d)
	}
	if account.SpendSinceBaseline != 25 {
		t.Fatalf("the baseline figure should see all provider spend: got %v, want 25",
			account.SpendSinceBaseline)
	}
	if account.RemainingUSD == nil || *account.RemainingUSD != 75 {
		t.Fatalf("remaining should be 100-25: %v", account.RemainingUSD)
	}
}

// ---- the balance ----

func TestSpendKeysLeavesTheBalanceUnsetWhenNoCreditWasEverBought(t *testing.T) {
	// nil is what tells the UI to offer "Add credit" instead of claiming a
	// balance of $0, which reads as "you are out of money".
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "alpha")
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(0), 5)

	account := getSpendKeys(t, srv).Anthropic
	if account.RemainingUSD != nil {
		t.Fatalf("want no balance, got %v", *account.RemainingUSD)
	}
	if account.BalanceSince != nil {
		t.Fatalf("want no baseline, got %v", *account.BalanceSince)
	}
	if account.TopupsTotalUSD != 0 || account.SpendSinceBaseline != 0 {
		t.Fatalf("want both balance figures at zero, got %+v", account)
	}
}

func TestSpendKeysComputesRemainingAsCreditMinusSpendSinceTheFirstTopup(t *testing.T) {
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "alpha")
	baseline := time.Now().UTC().AddDate(0, 0, -10)
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 100, OccurredAt: baseline.Unix()})
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 50, OccurredAt: time.Now().Unix()})
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(5), 30)

	account := getSpendKeys(t, srv).Anthropic
	if account.TopupsTotalUSD != 150 {
		t.Fatalf("credit should be 100+50, got %v", account.TopupsTotalUSD)
	}
	if account.SpendSinceBaseline != 30 {
		t.Fatalf("spend since the baseline should be 30, got %v", account.SpendSinceBaseline)
	}
	if account.RemainingUSD == nil || *account.RemainingUSD != 120 {
		t.Fatalf("remaining should be 150-30=120, got %v", account.RemainingUSD)
	}
	if account.BalanceSince == nil || *account.BalanceSince != baseline.Unix() {
		t.Fatalf("the baseline should be the EARLIEST top-up %d, got %v", baseline.Unix(), account.BalanceSince)
	}
}

func TestSpendKeysAnchorsTheBalanceToTheEarliestTopupNotTheLatest(t *testing.T) {
	// Anchoring to the most recent deposit would silently forgive every dollar
	// spent before it and show a balance far higher than the real one.
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "alpha")
	earliest := time.Now().UTC().AddDate(0, 0, -20)
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 100, OccurredAt: time.Now().AddDate(0, 0, -2).Unix()})
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 100, OccurredAt: earliest.Unix()})
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(15), 60)

	account := getSpendKeys(t, srv).Anthropic
	if account.BalanceSince == nil || *account.BalanceSince != earliest.Unix() {
		t.Fatalf("baseline is %v, want the earliest top-up %d", account.BalanceSince, earliest.Unix())
	}
	if account.SpendSinceBaseline != 60 {
		t.Fatalf("spend before the LATEST top-up was forgiven: got %v, want 60", account.SpendSinceBaseline)
	}
}

func TestSpendKeysExcludesSpendFromBeforeTheFirstTopup(t *testing.T) {
	// The balance answers "what is left of what I bought", so cost incurred
	// before the credit existed is not deducted from it.
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "alpha")
	store.AddTopup(usagestore.Topup{
		Provider: "anthropic", AmountUSD: 100,
		OccurredAt: time.Now().UTC().AddDate(0, 0, -5).Unix(),
	})
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(9), 40)
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(2), 10)

	account := getSpendKeys(t, srv).Anthropic
	if account.SpendSinceBaseline != 10 {
		t.Fatalf("spend from before the top-up was deducted: got %v, want 10", account.SpendSinceBaseline)
	}
	if account.RemainingUSD == nil || *account.RemainingUSD != 90 {
		t.Fatalf("remaining should be 100-10=90, got %v", account.RemainingUSD)
	}
}

func TestSpendKeysLetsTheBalanceGoNegativeWhenTheCreditIsSpent(t *testing.T) {
	// Clamping at zero would hide an overspend, which is the one case the
	// figure exists to make visible.
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "key_a", "alpha")
	store.AddTopup(usagestore.Topup{
		Provider: "anthropic", AmountUSD: 10,
		OccurredAt: time.Now().UTC().AddDate(0, 0, -5).Unix(),
	})
	saveSpend(t, store, "anthropic", "key_a", daysAgoDate(1), 25)

	account := getSpendKeys(t, srv).Anthropic
	if account.RemainingUSD == nil || *account.RemainingUSD != -15 {
		t.Fatalf("remaining should be 10-25=-15, got %v", account.RemainingUSD)
	}
}

func TestSpendKeysKeepsOneProvidersCreditOffTheOther(t *testing.T) {
	srv, store := newSpendServer(t)
	store.AddTopup(usagestore.Topup{
		Provider: "anthropic", AmountUSD: 100,
		OccurredAt: time.Now().UTC().AddDate(0, 0, -5).Unix(),
	})

	resp := getSpendKeys(t, srv)
	if resp.OpenAI.RemainingUSD != nil {
		t.Fatalf("anthropic's credit turned up on openai: %v", *resp.OpenAI.RemainingUSD)
	}
	if len(resp.OpenAI.Topups) != 0 {
		t.Fatalf("openai lists another provider's top-ups: %+v", resp.OpenAI.Topups)
	}
}

func TestSpendKeysListsTheTopupsItComputedTheBalanceFrom(t *testing.T) {
	// The UI shows the deposits under the balance. Listing them is what lets a
	// reader check the arithmetic rather than trust it.
	srv, store := newSpendServer(t)
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 40, OccurredAt: 1000, Note: "first"})
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 60, OccurredAt: 2000, Note: "second"})

	topups := getSpendKeys(t, srv).Anthropic.Topups
	if len(topups) != 2 {
		t.Fatalf("want 2 top-ups, got %+v", topups)
	}
	if topups[0].Note != "first" || topups[1].Note != "second" {
		t.Fatalf("top-ups are out of order: %+v", topups)
	}
}

// ---- the shape on the wire ----

func TestSpendKeysRendersEmptyCollectionsAsArrays(t *testing.T) {
	// `null` and `[]` are different values to the browser: the page maps over
	// both fields, so a null renders as a crash rather than an empty table.
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "GET", "/api/usage/spend/keys", "")
	body := rec.Body.String()

	if strings.Contains(body, `"keys":null`) {
		t.Fatalf("keys rendered as null: %s", body)
	}
	if strings.Contains(body, `"topups":null`) {
		t.Fatalf("topups rendered as null: %s", body)
	}
	if !strings.Contains(body, `"keys":[]`) || !strings.Contains(body, `"topups":[]`) {
		t.Fatalf("want empty arrays for both collections: %s", body)
	}
}

func TestSpendKeysRendersAnAbsentBalanceAsNull(t *testing.T) {
	// The counterpart to the arrays: here the empty value must NOT become a
	// number, or "no credit recorded" and "credit exhausted" render alike.
	srv, _ := newSpendServer(t)
	body := do(t, srv, "GET", "/api/usage/spend/keys", "").Body.String()

	if !strings.Contains(body, `"remaining_usd":null`) {
		t.Fatalf("want a null balance: %s", body)
	}
	if !strings.Contains(body, `"balance_since":null`) {
		t.Fatalf("want a null baseline: %s", body)
	}
}

func TestSpendKeysAnswersJSON(t *testing.T) {
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "GET", "/api/usage/spend/keys", "")
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type is %q", got)
	}
}

// ---- the raw snapshot route ----

func TestSpendKeyRawServesTheStoredSnapshotByteForByte(t *testing.T) {
	// The stored value is already JSON; re-encoding it would double-escape the
	// body the provider sent.
	srv, store := newSpendServer(t)
	raw := `{"id":"key_a","nested":{"kept":true}}`
	err := store.SaveKeyMeta(usagestore.KeyMeta{
		Provider: "anthropic", APIKeyID: "key_a", RawJSON: raw, FetchedAt: 1,
	})
	if err != nil {
		t.Fatalf("SaveKeyMeta: %v", err)
	}

	rec := do(t, srv, "GET", "/api/usage/spend/keys/anthropic/key_a/raw", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != raw {
		t.Fatalf("body is %q, want the stored snapshot %q", rec.Body.String(), raw)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type is %q", got)
	}
}

func TestSpendKeyRawIsNotFoundWhenNoSnapshotWasKept(t *testing.T) {
	srv, store := newSpendServer(t)
	// A key that exists with no raw_json is the same answer as no key at all:
	// there is nothing to show.
	saveMeta(t, store, "anthropic", "key_a", "alpha")

	rec := do(t, srv, "GET", "/api/usage/spend/keys/anthropic/key_a/raw", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for a key with no snapshot, got %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, srv, "GET", "/api/usage/spend/keys/anthropic/key_missing/raw", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown key, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSpendKeyRawReadsTheProviderAndKeyFromThePath(t *testing.T) {
	// Both path segments must reach the query. Ignoring either serves one
	// provider's raw credential snapshot under the other's URL.
	srv, store := newSpendServer(t)
	store.SaveKeyMeta(usagestore.KeyMeta{Provider: "anthropic", APIKeyID: "shared", RawJSON: `{"who":"anthropic"}`, FetchedAt: 1})
	store.SaveKeyMeta(usagestore.KeyMeta{Provider: "openai", APIKeyID: "shared", RawJSON: `{"who":"openai"}`, FetchedAt: 1})
	store.SaveKeyMeta(usagestore.KeyMeta{Provider: "anthropic", APIKeyID: "other", RawJSON: `{"who":"other"}`, FetchedAt: 1})

	rec := do(t, srv, "GET", "/api/usage/spend/keys/openai/shared/raw", "")
	if rec.Body.String() != `{"who":"openai"}` {
		t.Fatalf("provider segment ignored: got %s", rec.Body.String())
	}
	rec = do(t, srv, "GET", "/api/usage/spend/keys/anthropic/other/raw", "")
	if rec.Body.String() != `{"who":"other"}` {
		t.Fatalf("key segment ignored: got %s", rec.Body.String())
	}
}

// ---- top-up routes ----

func TestListTopupsRefusesToGuessAProvider(t *testing.T) {
	// Without the guard an empty provider queries for rows stored under "" and
	// answers 200 with an empty list, which reads as "no credit recorded" for
	// an account that has plenty.
	srv, store := newSpendServer(t)
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000})

	rec := do(t, srv, "GET", "/api/usage/spend/topups", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 with no provider, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "provider") {
		t.Fatalf("the refusal does not say what is missing: %s", rec.Body.String())
	}
}

func TestListTopupsReturnsTheProvidersDepositsEarliestFirst(t *testing.T) {
	srv, store := newSpendServer(t)
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 2000, Note: "second"})
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 20, OccurredAt: 1000, Note: "first"})
	store.AddTopup(usagestore.Topup{Provider: "openai", AmountUSD: 30, OccurredAt: 1500, Note: "other provider"})

	rec := do(t, srv, "GET", "/api/usage/spend/topups?provider=anthropic", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var topups []usagestore.Topup
	if err := json.Unmarshal(rec.Body.Bytes(), &topups); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(topups) != 2 {
		t.Fatalf("want anthropic's 2 top-ups, got %+v", topups)
	}
	if topups[0].Note != "first" || topups[1].Note != "second" {
		t.Fatalf("out of order: %+v", topups)
	}
}

func TestListTopupsRendersAnEmptyListAsAnArray(t *testing.T) {
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "GET", "/api/usage/spend/topups?provider=anthropic", "")
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Fatalf("want [], got %q", body)
	}
}

func TestAddTopupStoresTheDepositAndReturnsItsId(t *testing.T) {
	srv, store := newSpendServer(t)
	rec := do(t, srv, "POST", "/api/usage/spend/topups",
		`{"provider":"anthropic","amount_usd":25.5,"occurred_at":1000,"note":"invoice 42"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got map[string]int64
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["id"] == 0 {
		t.Fatalf("no id came back: %s", rec.Body.String())
	}

	topups, _ := store.ListTopups("anthropic")
	if len(topups) != 1 {
		t.Fatalf("want 1 stored top-up, got %+v", topups)
	}
	stored := topups[0]
	if stored.ID != got["id"] {
		t.Fatalf("the reply's id %d does not name the stored row %d", got["id"], stored.ID)
	}
	if stored.AmountUSD != 25.5 || stored.OccurredAt != 1000 || stored.Note != "invoice 42" {
		t.Fatalf("the deposit was altered on the way in: %+v", stored)
	}
}

func TestAddTopupAcceptsADateInsteadOfATimestamp(t *testing.T) {
	// The form sends YYYY-MM-DD. It must be read as UTC midnight, since the
	// baseline it becomes is compared against UTC-keyed spend days.
	srv, store := newSpendServer(t)
	rec := do(t, srv, "POST", "/api/usage/spend/topups",
		`{"provider":"anthropic","amount_usd":10,"occurred_at_str":"2026-03-04"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}

	topups, _ := store.ListTopups("anthropic")
	want := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC).Unix()
	if len(topups) != 1 || topups[0].OccurredAt != want {
		t.Fatalf("want occurred_at %d (UTC midnight), got %+v", want, topups)
	}
}

func TestAddTopupPrefersAnExplicitTimestampOverTheDateString(t *testing.T) {
	srv, store := newSpendServer(t)
	do(t, srv, "POST", "/api/usage/spend/topups",
		`{"provider":"anthropic","amount_usd":10,"occurred_at":777,"occurred_at_str":"2026-03-04"}`)

	topups, _ := store.ListTopups("anthropic")
	if len(topups) != 1 || topups[0].OccurredAt != 777 {
		t.Fatalf("want the explicit 777, got %+v", topups)
	}
}

func TestAddTopupDatesADepositWithNoDateToNow(t *testing.T) {
	srv, store := newSpendServer(t)
	before := time.Now().Unix()
	rec := do(t, srv, "POST", "/api/usage/spend/topups", `{"provider":"anthropic","amount_usd":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}

	topups, _ := store.ListTopups("anthropic")
	if len(topups) != 1 {
		t.Fatalf("want 1 top-up, got %+v", topups)
	}
	if topups[0].OccurredAt < before || topups[0].OccurredAt > time.Now().Unix()+1 {
		t.Fatalf("occurred_at %d is not now (>= %d)", topups[0].OccurredAt, before)
	}
}

func TestAddTopupRefusesAnUnparseableDate(t *testing.T) {
	// Falling back to "now" for a date the user typed wrong would move the
	// balance baseline to today and silently forgive all prior spend.
	srv, store := newSpendServer(t)
	rec := do(t, srv, "POST", "/api/usage/spend/topups",
		`{"provider":"anthropic","amount_usd":10,"occurred_at_str":"04/03/2026"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "occurred_at_str") {
		t.Fatalf("the refusal does not name the field: %s", rec.Body.String())
	}
	topups, _ := store.ListTopups("anthropic")
	if len(topups) != 0 {
		t.Fatalf("a rejected deposit was stored anyway: %+v", topups)
	}
}

func TestAddTopupRefusesMalformedJSON(t *testing.T) {
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "POST", "/api/usage/spend/topups", `{"provider":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAddTopupPassesTheStoresRefusalBackAsABadRequest(t *testing.T) {
	// A deposit of zero, a negative one, or one with no provider is the user's
	// mistake, not the server's. A 500 here would send them to the logs.
	srv, store := newSpendServer(t)
	bodies := []string{
		`{"provider":"","amount_usd":10,"occurred_at":1000}`,
		`{"provider":"anthropic","amount_usd":0,"occurred_at":1000}`,
		`{"provider":"anthropic","amount_usd":-5,"occurred_at":1000}`,
	}
	for _, body := range bodies {
		rec := do(t, srv, "POST", "/api/usage/spend/topups", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", body, rec.Code, rec.Body.String())
		}
	}
	topups, _ := store.ListTopups("anthropic")
	if len(topups) != 0 {
		t.Fatalf("a refused deposit reached the table: %+v", topups)
	}
}

func TestDeleteTopupRemovesTheRowAndAnswersNoContent(t *testing.T) {
	srv, store := newSpendServer(t)
	id, _ := store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000})
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 20, OccurredAt: 2000, Note: "keeper"})

	rec := do(t, srv, "DELETE", fmt.Sprintf("/api/usage/spend/topups/%d", id), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d %s", rec.Code, rec.Body.String())
	}

	topups, _ := store.ListTopups("anthropic")
	if len(topups) != 1 || topups[0].Note != "keeper" {
		t.Fatalf("want only the keeper left, got %+v", topups)
	}
}

func TestDeleteTopupRefusesAnIdThatIsNotANumber(t *testing.T) {
	// Without the guard ParseInt's zero reaches the store, deletes nothing and
	// answers 204 — a failed delete reported as a successful one.
	srv, store := newSpendServer(t)
	store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000})

	rec := do(t, srv, "DELETE", "/api/usage/spend/topups/not-a-number", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	topups, _ := store.ListTopups("anthropic")
	if len(topups) != 1 {
		t.Fatalf("the rejected delete still removed something: %+v", topups)
	}
}

// ---- refresh ----

func TestSpendRefreshIsNotFoundWhenNoAdminCredentialIsConfigured(t *testing.T) {
	// 404 rather than 500: the box has no admin key, which is a state, not a
	// fault. Reaching the collector here would dereference nil.
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "POST", "/api/usage/spend/refresh?provider=anthropic", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "admin credential") {
		t.Fatalf("the refusal does not say what is missing: %s", rec.Body.String())
	}
}

func TestSpendRefreshRefusesAProviderItCannotCollect(t *testing.T) {
	srv, _ := newConfiguredSpendServer(t)
	for _, provider := range []string{"", "openai", "nonsense"} {
		rec := do(t, srv, "POST", "/api/usage/spend/refresh?provider="+provider, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("provider=%q: want 400, got %d %s", provider, rec.Code, rec.Body.String())
		}
	}
}

func TestPersistAnthropicWritesEveryKeyAndDayOfAFetch(t *testing.T) {
	srv, store := newSpendServer(t)
	result := &spend.AnthropicResult{
		AdminKeyHint: "sk-ant-admin...zzz",
		Keys: []usagestore.KeyMeta{
			{Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "alpha", APIKeyStatus: "active", FetchedAt: 10},
			{Provider: "anthropic", APIKeyID: "key_b", APIKeyName: "bravo", APIKeyStatus: "active", FetchedAt: 10},
		},
		Daily: []usagestore.DailySpend{
			{Provider: "anthropic", APIKeyID: "key_a", Date: daysAgoDate(0), TotalUSD: 2, FetchedAt: 10},
			{Provider: "anthropic", APIKeyID: "key_a", Date: daysAgoDate(1), TotalUSD: 3, FetchedAt: 10},
			{Provider: "anthropic", APIKeyID: "key_b", Date: daysAgoDate(0), TotalUSD: 5, FetchedAt: 10},
		},
	}

	if err := srv.persistAnthropic(result); err != nil {
		t.Fatalf("persistAnthropic: %v", err)
	}

	metas, _ := store.ListKeyMeta("anthropic")
	if len(metas) != 2 {
		t.Fatalf("want both keys stored, got %+v", metas)
	}
	totals, _ := store.PerKeyTotals("anthropic", time.Now().AddDate(0, 0, -2).Unix())
	if totals["key_a"] != 5 {
		t.Fatalf("key_a should sum its two days to 5, got %v", totals["key_a"])
	}
	if totals["key_b"] != 5 {
		t.Fatalf("key_b should be 5, got %v", totals["key_b"])
	}
}

func TestPersistAnthropicFailsLoudlyAndNamesTheKeyItChokedOn(t *testing.T) {
	// A fetch that half-lands must say so. Swallowing the error would leave the
	// page reporting a successful refresh over stale numbers.
	srv, store := newSpendServer(t)
	result := &spend.AnthropicResult{
		Keys: []usagestore.KeyMeta{
			{Provider: "anthropic", APIKeyID: "key_a", APIKeyName: "alpha", FetchedAt: 10},
			{APIKeyID: "key_broken", APIKeyName: "no provider", FetchedAt: 10},
		},
	}

	err := srv.persistAnthropic(result)
	if err == nil {
		t.Fatal("an unstorable key was accepted silently")
	}
	if !strings.Contains(err.Error(), "key_broken") {
		t.Fatalf("the error does not name the key: %v", err)
	}

	metas, _ := store.ListKeyMeta("anthropic")
	if len(metas) != 1 {
		t.Fatalf("the keys before the failure should have landed: %+v", metas)
	}
}

// ---- the surface every route sits behind ----

func TestServeHTTPAnswersAPreflightWithoutReachingARoute(t *testing.T) {
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "OPTIONS", "/api/usage/spend/keys", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204 for a preflight, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a preflight answered with a body: %s", rec.Body.String())
	}
}

func TestServeHTTPAllowsTheBrowserToCallEveryMethodTheRoutesUse(t *testing.T) {
	// The dashboard is served from another origin, so the routes are only
	// reachable if these three headers are present — and DELETE is the one a
	// GET/POST-only list would quietly drop.
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "GET", "/api/usage/spend/keys", "")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin is %q", got)
	}
	allowed := rec.Header().Get("Access-Control-Allow-Methods")
	for _, method := range []string{"GET", "POST", "DELETE", "OPTIONS"} {
		if !strings.Contains(allowed, method) {
			t.Fatalf("%s is missing from Allow-Methods %q", method, allowed)
		}
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Content-Type") {
		t.Fatalf("Allow-Headers is %q, and the top-up POST sends JSON", got)
	}
}

func TestHealthAnswersOK(t *testing.T) {
	srv, _ := newSpendServer(t)
	rec := do(t, srv, "GET", "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got map[string]string
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "ok" {
		t.Fatalf("want status ok, got %s", rec.Body.String())
	}
}

func TestEverySpendRouteIsRegisteredUnderTheMethodItExpects(t *testing.T) {
	// routes() is the one place a path can go missing. A route registered under
	// the wrong method answers 405 to the UI and nothing else notices.
	srv, store := newSpendServer(t)
	id, _ := store.AddTopup(usagestore.Topup{Provider: "anthropic", AmountUSD: 10, OccurredAt: 1000})
	store.SaveKeyMeta(usagestore.KeyMeta{Provider: "anthropic", APIKeyID: "key_a", RawJSON: `{}`, FetchedAt: 1})

	cases := []struct {
		method, target string
	}{
		{"GET", "/api/usage/spend/keys"},
		{"GET", "/api/usage/spend/keys/anthropic/key_a/raw"},
		{"POST", "/api/usage/spend/refresh?provider=anthropic"},
		{"GET", "/api/usage/spend/topups?provider=anthropic"},
		{"POST", "/api/usage/spend/topups"},
		{"DELETE", fmt.Sprintf("/api/usage/spend/topups/%d", id)},
		{"GET", "/health"},
	}
	for _, c := range cases {
		body := ""
		if c.method == "POST" && strings.HasSuffix(c.target, "topups") {
			body = `{"provider":"anthropic","amount_usd":1,"occurred_at":1000}`
		}
		rec := do(t, srv, c.method, c.target, body)
		// An unrouted request is answered by the mux itself, so the discriminator
		// is the body, not the status: /spend/refresh legitimately answers 404
		// when no admin credential is configured, and that 404 comes from a
		// handler that ran.
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Fatalf("%s %s is not routed: %d %s", c.method, c.target, rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s %s is routed under another method: %d", c.method, c.target, rec.Code)
		}
	}
}
