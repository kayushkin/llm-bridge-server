#!/usr/bin/env python3
"""Score usage-store's tests for the spend-keys mechanism by breaking it.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-spend-keys.py [--diffs] [--crosstable]

⚠️ **This is the engine's SEVENTH copy, and it is the same blob as the other
six.** md5 `9a81a32e5827b59c1a3093bf88187b17`, taken from git blob
`664f35f475edb9b7d018a28136211bf58a0ff53e` — what
`scheduler/fix/the-scorer-counts-occurrences-not-files`,
`bundle-store/docs/one-sabotage-engine-again`,
`agent-store/test/the-tracked-file-switch-is-unreached`,
`skill-store/test/the-install-path-is-unreached`,
`tool-store/test/the-repo-map-and-recent-files-tools-are-unreached` and
`mailstack/test/the-write-ops-are-unreached` all carry. Diff before editing; an
eighth blob is a fork. Take the blob off the BRANCH, not out of a working tree —
those checkouts sit on whatever branch their last pass left them on, and
md5summing them answers about the wrong commit (221st).

Why this seam is worth a scorer: measured on `main`, a `panic()` on the first
line of any of these twenty-three left `go test ./...` green. Only
`migrateSpend` reddened, and only because `Open` runs it.

    spend.go                  SaveKeyMeta        ┐
    spend.go                  SaveDailySpend     │
    spend.go                  ListKeyMeta        │ the store: what a key cost,
    spend.go                  LatestKeyRaw       ├ what credit was bought, and
    spend.go                  SumSpendSince      │ therefore what is left
    spend.go                  PerKeyTotals       │
    spend.go                  AddTopup           │
    spend.go                  ListTopups         │
    spend.go                  DeleteTopup        │
    spend.go                  containsAny        ┘
    internal/server/server.go assembleProvider   <- the census row
    internal/server/server.go handleSpendKeys    ┐
    internal/server/server.go handleSpendKeyRaw  │
    internal/server/server.go handleSpendRefresh │
    internal/server/server.go persistAnthropic   ├ the routes above it
    internal/server/server.go handleListTopups   │
    internal/server/server.go handleAddTopup     │
    internal/server/server.go handleDeleteTopup  │
    internal/server/server.go handleHealth       ┘
    internal/server/server.go SaveAdminHint      ┐
    internal/server/server.go New                ├ the surface they sit behind
    internal/server/server.go routes             │
    internal/server/server.go ServeHTTP          ┘

The census named ONE of the twenty-three. That is the sixth consecutive row on
`e9b0b89c` where the function name under-described the mechanism it belongs to,
and the 222nd's rule — read the row's neighbors before scoping the work to its
title — has now not once failed to pay.

⚠️ Read that measurement on `main`. usage-store's checkout was standing on
`test/auth-store-clients-speak-the-real-protocol`, which is exactly the base the
223rd's rule is about: a reach guard run on whatever branch a checkout was left
on answers about the wrong tree, and it fails in the direction that closes a row
without testing it. This branch is cut from `main`.

Result, 2026-08-15: **70/74 real mechanisms caught, both controls behaved.** The
four uncaught rows are all declared below with their reasons — three are guards
dominated by their own callee or by a conversion that no input can separate from
its absence, and one is a copy that exists to show what the assertions do NOT
rest on. Round one scored lower with three `compile error` rows and one abort:
every compile error was a case that DELETED a value another line still used, and
the abort was `w.WriteHeader(http.StatusNoContent)`, which appears in the delete
handler and in the preflight branch both.

What makes this mechanism worth more than a coverage row: it is the only code in
the fleet whose output is money the user is deciding by. Two figures on one
response are computed by different routes through the store — the window totals
are accumulated key by key while walking `ListKeyMeta`, and the balance is a
single provider-wide sum — so they can disagree without either being obviously
wrong. Sixteen of the cases below are about which day, which key and which
deposit each of them counts.
"""

import sys

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))

from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "spend.go", REPO / "internal" / "server" / "server.go"]
PACKAGES = [".", "./internal/server"]

CASES = [
    # ---- the window: which days each figure counts ----
    Case(
        "the provider-wide sum drops the day its window opens on",
        [("""SELECT COALESCE(SUM(total_usd), 0) FROM spend_daily
			WHERE provider = ? AND date >= ?""",
          """SELECT COALESCE(SUM(total_usd), 0) FROM spend_daily
			WHERE provider = ? AND date > ?""")],
    ),
    Case(
        "the per-key totals drop the day their window opens on",
        [("""SELECT api_key_id, SUM(total_usd) FROM spend_daily
		WHERE provider = ? AND date >= ?""",
          """SELECT api_key_id, SUM(total_usd) FROM spend_daily
		WHERE provider = ? AND date > ?""")],
    ),
    Case(
        "a sum asked about one key answers for the whole provider",
        [("WHERE provider = ? AND api_key_id = ? AND date >= ?",
          "WHERE provider = ? AND (? IS NOT NULL) AND date >= ?")],
    ),
    Case(
        "the per-key totals are grouped by provider, so one key's figure stands for all of them",
        [("GROUP BY api_key_id", "GROUP BY provider")],
    ),
    Case(
        "the 24h cutoff is today's midnight, so yesterday falls out of the window",
        [("day1 := now.AddDate(0, 0, -1).Truncate(24 * time.Hour).Unix()",
          "day1 := now.AddDate(0, 0, 0).Truncate(24 * time.Hour).Unix()")],
    ),
    Case(
        "the 24h cutoff reaches four days back",
        [("day1 := now.AddDate(0, 0, -1).Truncate(24 * time.Hour).Unix()",
          "day1 := now.AddDate(0, 0, -4).Truncate(24 * time.Hour).Unix()")],
    ),
    Case(
        "the 7d cutoff reaches only two days back",
        [("day7 := now.AddDate(0, 0, -7).Truncate(24 * time.Hour).Unix()",
          "day7 := now.AddDate(0, 0, -2).Truncate(24 * time.Hour).Unix()")],
    ),
    Case(
        "the 7d cutoff reaches a month back",
        [("day7 := now.AddDate(0, 0, -7).Truncate(24 * time.Hour).Unix()",
          "day7 := now.AddDate(0, 0, -30).Truncate(24 * time.Hour).Unix()")],
    ),
    Case(
        "the 30d cutoff reaches only a week back",
        [("day30 := now.AddDate(0, 0, -30).Truncate(24 * time.Hour).Unix()",
          "day30 := now.AddDate(0, 0, -7).Truncate(24 * time.Hour).Unix()")],
    ),
    Case(
        "the 30d cutoff reaches two months back",
        [("day30 := now.AddDate(0, 0, -30).Truncate(24 * time.Hour).Unix()",
          "day30 := now.AddDate(0, 0, -60).Truncate(24 * time.Hour).Unix()")],
    ),
    Case(
        "the 24h cutoff keeps the time of day instead of falling to midnight",
        [("day1 := now.AddDate(0, 0, -1).Truncate(24 * time.Hour).Unix()",
          "day1 := now.AddDate(0, 0, -1).Unix()")],
        expected_unnoticed="the store converts the cutoff to a UTC date before comparing it "
                           "with the YYYY-MM-DD keys in spend_daily, so the time of day cannot "
                           "survive as far as the query — the truncation is a statement of "
                           "intent that no input can make observable",
    ),

    # ---- the upserts: a refresh replaces, it does not accumulate ----
    Case(
        "a day's cost is added to what was already recorded for that day",
        [("""			total_usd  = excluded.total_usd,""",
          """			total_usd  = spend_daily.total_usd + excluded.total_usd,""")],
    ),
    Case(
        "a refreshed key keeps the status it was retired under",
        [("			api_key_status = excluded.api_key_status,",
          "			api_key_status = spend_keys.api_key_status,")],
    ),
    Case(
        "a refreshed key keeps the name it was renamed away from",
        [("			api_key_name   = excluded.api_key_name,",
          "			api_key_name   = spend_keys.api_key_name,")],
    ),
    Case(
        "the raw snapshot is never refreshed after the first fetch",
        [("			raw_json       = excluded.raw_json,",
          "			raw_json       = spend_keys.raw_json,")],
    ),
    Case(
        "a key meta row with no provider is accepted",
        [('	if m.Provider == "" || m.APIKeyID == "" {',
          '	if m.APIKeyID == "" {')],
    ),
    Case(
        "a key meta row arrives with no fetch time and keeps it",
        [("""	if m.FetchedAt == 0 {
		m.FetchedAt = time.Now().Unix()
	}""", "")],
    ),
    Case(
        "a daily row arrives with no fetch time and keeps it",
        [("""	if d.FetchedAt == 0 {
		d.FetchedAt = time.Now().Unix()
	}""", "")],
    ),

    # ---- the key list ----
    Case(
        "archived keys are listed above active ones",
        [("ORDER BY (api_key_status = 'active') DESC, api_key_name ASC",
          "ORDER BY (api_key_status = 'active') ASC, api_key_name ASC")],
    ),
    Case(
        "keys of equal standing are listed by name backwards",
        [("ORDER BY (api_key_status = 'active') DESC, api_key_name ASC",
          "ORDER BY (api_key_status = 'active') DESC, api_key_name DESC")],
    ),
    Case(
        "every key's raw API snapshot rides along in the list",
        [("SELECT provider, api_key_id, api_key_name, api_key_hint, api_key_status, fetched_at",
          "SELECT provider, api_key_id, api_key_name, api_key_hint, api_key_status, fetched_at, COALESCE(raw_json, '')"),
         ("if err := rows.Scan(&k.Provider, &k.APIKeyID, &k.APIKeyName, &k.APIKeyHint, &k.APIKeyStatus, &k.FetchedAt); err != nil {",
          "if err := rows.Scan(&k.Provider, &k.APIKeyID, &k.APIKeyName, &k.APIKeyHint, &k.APIKeyStatus, &k.FetchedAt, &k.RawJSON); err != nil {")],
    ),
    Case(
        "a provider with no keys yet answers null instead of an empty list",
        [("""	out := []KeyMeta{}
	for rows.Next() {
		var k KeyMeta""",
          """	var out []KeyMeta
	for rows.Next() {
		var k KeyMeta""")],
    ),

    # ---- the raw snapshot ----
    Case(
        "a key that was never seen is an error rather than an empty snapshot",
        [("""	if err == sql.ErrNoRows {
		return "", nil
	}""",
          """	if err == sql.ErrNoRows {
		return "", err
	}""")],
    ),
    Case(
        "a snapshot stored as NULL fails the read",
        [("	var raw sql.NullString", "	var raw string"),
         ("	return raw.String, nil", "	return raw, nil")],
    ),

    # ---- deposits ----
    Case(
        "a deposit of nothing is recorded",
        [("	if t.AmountUSD <= 0 {", "	if t.AmountUSD < 0 {")],
    ),
    Case(
        "a deposit with no date is dated now",
        [('		return 0, fmt.Errorf("occurred_at required")',
          "		t.OccurredAt = time.Now().Unix()")],
    ),
    Case(
        "the date a deposit was typed in overwrites the one supplied",
        [("	if t.CreatedAt == 0 {", "	if t.CreatedAt >= 0 {")],
    ),
    Case(
        "a deposit's note is dropped on the way in",
        [("		t.Provider, t.AmountUSD, t.OccurredAt, t.Note, t.CreatedAt,",
          '		t.Provider, t.AmountUSD, t.OccurredAt, "", t.CreatedAt,')],
    ),
    Case(
        "deposits are listed newest first, which moves the balance baseline",
        [("ORDER BY occurred_at ASC, id ASC", "ORDER BY occurred_at DESC, id ASC")],
    ),
    Case(
        "two deposits on the same second come back in the other order",
        [("ORDER BY occurred_at ASC, id ASC", "ORDER BY occurred_at ASC, id DESC")],
    ),
    Case(
        "deleting one deposit deletes every other one",
        [("DELETE FROM spend_topups WHERE id = ?", "DELETE FROM spend_topups WHERE id != ?")],
    ),

    # ---- migrateSpend ----
    Case(
        "the migration drops the current layout and keeps the superseded one",
        [('	if oldSchema != "" && containsAny(oldSchema, "total_usd_24h", "total_usd_30d") {',
          '	if oldSchema != "" && !containsAny(oldSchema, "total_usd_24h", "total_usd_30d") {')],
    ),
    Case(
        "containsAny answers about the substrings that are absent",
        [("		if strings.Contains(s, sub) {", "		if !strings.Contains(s, sub) {")],
    ),

    # ---- which account is configured ----
    Case(
        "anthropic reports itself configured with no collector behind it",
        [('		Anthropic: s.assembleProvider("anthropic", s.spendAnthropic != nil),',
          '		Anthropic: s.assembleProvider("anthropic", true),')],
    ),
    Case(
        "openai reports configured off the back of anthropic's collector",
        [('		OpenAI:    s.assembleProvider("openai", false),',
          '		OpenAI:    s.assembleProvider("openai", s.spendAnthropic != nil),')],
    ),
    Case(
        "the anthropic account is assembled from openai's rows",
        [('		Anthropic: s.assembleProvider("anthropic", s.spendAnthropic != nil),',
          '		Anthropic: s.assembleProvider("openai", s.spendAnthropic != nil),')],
    ),

    # ---- the admin-key hint ----
    Case(
        "every provider shows the anthropic admin hint",
        [("		AdminKeyHint: s.adminHints[provider],",
          '		AdminKeyHint: s.adminHints["anthropic"],')],
    ),
    Case(
        "a rotated admin key leaves the retired hint on screen",
        [("	s.adminHints[provider] = hint",
          """	if _, seen := s.adminHints[provider]; !seen {
		s.adminHints[provider] = hint
	}""")],
    ),

    # ---- assembling the account ----
    Case(
        "the account's 24h header accumulates the 7d figure",
        [("		out.Total24h += row.Total24h", "		out.Total24h += row.Total7d")],
    ),
    # The two window columns are SWAPPED rather than one overwritten. Assigning
    # per7 twice would leave per24 declared and unused, and Go makes that a
    # compile error — a case that never runs, which the score line reports as a
    # coverage hole that is not there (223rd, 225th).
    Case(
        "a key row's 24h and 7d columns are swapped",
        [("			Total24h:     per24[m.APIKeyID],\n			Total7d:      per7[m.APIKeyID],",
          "			Total24h:     per7[m.APIKeyID],\n			Total7d:      per24[m.APIKeyID],")],
    ),
    Case(
        "a key row's 7d and 30d columns are swapped",
        [("			Total7d:      per7[m.APIKeyID],\n			Total30d:     per30[m.APIKeyID],",
          "			Total7d:      per30[m.APIKeyID],\n			Total30d:     per7[m.APIKeyID],")],
    ),
    Case(
        "the fetch time is dropped from every key row",
        [("			FetchedAt:    m.FetchedAt,", "			FetchedAt:    0,")],
    ),
    Case(
        "the balance is anchored to the most recent deposit, forgiving everything spent before it",
        [("		baseline := out.Topups[0].OccurredAt",
          "		baseline := out.Topups[len(out.Topups)-1].OccurredAt")],
    ),
    Case(
        "the balance ignores what has been spent against it",
        [("		remaining := topupsTotal - spend", "		remaining := topupsTotal")],
    ),
    Case(
        "an overspent balance is clamped at zero",
        [("		remaining := topupsTotal - spend",
          """		remaining := topupsTotal - spend
		if remaining < 0 {
			remaining = 0
		}""")],
    ),
    Case(
        "only the last deposit counts towards the credit bought",
        [("			topupsTotal += t.AmountUSD", "			topupsTotal = t.AmountUSD")],
    ),
    Case(
        "an account with one deposit is reported as having no credit at all",
        [("	if len(out.Topups) > 0 {", "	if len(out.Topups) > 1 {")],
    ),
    Case(
        "the deposits are computed from and then not listed",
        [("	if topups != nil {\n		out.Topups = topups\n	}",
          "	if topups != nil {\n		out.Topups = append([]usagestore.Topup{}, topups...)\n	}")],
        expected_unnoticed="the replacement is the same list by another route — it is here to "
                           "show the copy is not what any assertion rests on; the case that "
                           "matters is the one below, which stops the deposits reaching the "
                           "account at all",
    ),
    Case(
        "the deposits never reach the account, so no balance is ever computed",
        [("	if topups != nil {\n		out.Topups = topups\n	}", "	_ = topups")],
    ),

    # ---- the top-up routes ----
    Case(
        "a top-up list with no provider answers for the empty provider",
        [('	if provider == "" {\n		http.Error(w, `{"error":"provider query param required"}`, http.StatusBadRequest)\n		return\n	}',
          "")],
    ),
    Case(
        "an empty top-up list is rendered as null",
        [("	if topups == nil {\n		topups = []usagestore.Topup{}\n	}", "")],
        expected_unnoticed="ListTopups already returns an empty slice rather than nil, so this "
                           "guard is dominated by its callee and no input reaches it — pinned "
                           "one layer down instead, by the store test that fixes that contract",
    ),
    Case(
        "a date typed into the form is ignored in favor of now",
        [('	if occurredAt == 0 && req.OccurredAtStr != "" {',
          '	if occurredAt != 0 && req.OccurredAtStr != "" {')],
    ),
    Case(
        "an explicit timestamp is overwritten by the date string beside it",
        [('	if occurredAt == 0 && req.OccurredAtStr != "" {',
          '	if req.OccurredAtStr != "" {')],
    ),
    Case(
        "a date that cannot be parsed is silently accepted",
        [('			writeErr(w, http.StatusBadRequest, fmt.Errorf("occurred_at_str: %w", err))\n			return',
          "			err = nil")],
    ),
    Case(
        "the parsed date is read in the machine's own timezone",
        [("		occurredAt = t.UTC().Unix()", "		occurredAt = t.Unix()")],
        expected_unnoticed="time.Parse with no zone in the layout already returns a UTC time, so "
                           "the conversion is a no-op that no input can separate from its absence",
    ),
    Case(
        "a deposit the store refuses is reported as a server fault",
        [("	if err != nil {\n		writeErr(w, http.StatusBadRequest, err)\n		return\n	}\n	writeJSON(w, map[string]int64{\"id\": id})",
          "	if err != nil {\n		writeErr(w, http.StatusInternalServerError, err)\n		return\n	}\n	writeJSON(w, map[string]int64{\"id\": id})")],
    ),
    # `"id": 0` would orphan the id the store handed back; multiplying keeps the
    # variable live and returns the same useless zero.
    Case(
        "the id of the stored deposit is not returned",
        [('	writeJSON(w, map[string]int64{"id": id})', '	writeJSON(w, map[string]int64{"id": id * 0})')],
    ),
    Case(
        "a top-up id that is not a number deletes row zero and reports success",
        [("	id, err := strconv.ParseInt(r.PathValue(\"id\"), 10, 64)\n	if err != nil {",
          "	id, err := strconv.ParseInt(r.PathValue(\"id\"), 10, 64)\n	if err != nil && false {")],
    ),
    # The needle carries the delete above it: `w.WriteHeader(http.StatusNoContent)`
    # alone appears here AND in the preflight branch of ServeHTTP, and the engine
    # would have sabotaged whichever came first.
    Case(
        "a delete answers 200 with an empty body instead of 204",
        [("""	if err := s.store.DeleteTopup(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)""",
          """	if err := s.store.DeleteTopup(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)""")],
    ),

    # ---- the raw route ----
    Case(
        "a key with no snapshot answers 200 and nothing",
        [('	if raw == "" {\n		http.Error(w, `{"error":"no snapshot for that key"}`, http.StatusNotFound)\n		return\n	}',
          "")],
    ),
    Case(
        "the stored snapshot is re-encoded on the way out",
        [('	w.Header().Set("Content-Type", "application/json")\n	w.Write([]byte(raw))',
          "	writeJSON(w, raw)")],
    ),
    Case(
        "the provider and the key id are read from the path the wrong way round",
        [("	raw, err := s.store.LatestKeyRaw(provider, apiKeyID)",
          "	raw, err := s.store.LatestKeyRaw(apiKeyID, provider)")],
    ),

    # ---- refresh ----
    Case(
        "a box with no admin credential is reported as a server fault",
        [('			http.Error(w, `{"error":"anthropic admin credential not configured"}`, http.StatusNotFound)',
          '			http.Error(w, `{"error":"anthropic admin credential not configured"}`, http.StatusInternalServerError)')],
    ),
    Case(
        "a provider nothing can collect is reported as missing rather than refused",
        [('		writeJSON(w, map[string]interface{}{"keys": len(res.Keys), "daily": len(res.Daily)})\n	default:\n		http.Error(w, `{"error":"unknown provider"}`, http.StatusBadRequest)',
          '		writeJSON(w, map[string]interface{}{"keys": len(res.Keys), "daily": len(res.Daily)})\n	default:\n		http.Error(w, `{"error":"unknown provider"}`, http.StatusNotFound)')],
    ),
    Case(
        "a key that cannot be stored is passed over in silence",
        [("		if err := s.store.SaveKeyMeta(m); err != nil {",
          "		if err := s.store.SaveKeyMeta(m); err != nil && false {")],
    ),
    Case(
        "the failure does not name the key it choked on",
        [('			return fmt.Errorf("save key meta %s: %w", m.APIKeyID, err)',
          '			return fmt.Errorf("save key meta: %w", err)')],
    ),
    Case(
        "a fetch's daily rows are dropped on the floor",
        [("	for _, d := range res.Daily {", "	for _, d := range res.Daily[:0] {")],
    ),

    # ---- the surface every route sits behind ----
    Case(
        "a preflight is answered with 200 and a body",
        [('	if r.Method == "OPTIONS" {\n		w.WriteHeader(http.StatusNoContent)',
          '	if r.Method == "OPTIONS" {\n		w.WriteHeader(http.StatusOK)')],
    ),
    Case(
        "the browser is not told it may DELETE",
        [('	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")',
          '	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")')],
    ),
    Case(
        "the dashboard's origin is not allowed",
        [('	w.Header().Set("Access-Control-Allow-Origin", "*")',
          '	w.Header().Set("Access-Control-Allow-Origin", "https://example.invalid")')],
    ),
    Case(
        "the JSON content type is not allowed on a request, so the top-up POST never arrives",
        [('	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")',
          '	w.Header().Set("Access-Control-Allow-Headers", "Authorization")')],
    ),
    Case(
        "the top-up list is registered at a path nothing asks for",
        [('	s.mux.HandleFunc("GET /api/usage/spend/topups", s.handleListTopups)',
          '	s.mux.HandleFunc("GET /api/usage/spend/topup-list", s.handleListTopups)')],
    ),
    Case(
        "the raw-snapshot route is registered without its key segment",
        [('	s.mux.HandleFunc("GET /api/usage/spend/keys/{provider}/{api_key_id}/raw", s.handleSpendKeyRaw)',
          '	s.mux.HandleFunc("GET /api/usage/spend/keys/{provider}/raw", s.handleSpendKeyRaw)')],
    ),
    Case(
        "health answers with a different word",
        [('	writeJSON(w, map[string]string{"status": "ok"})',
          '	writeJSON(w, map[string]string{"status": "alive"})')],
    ),

    # ---- controls ----
    # Known-positive: the table every figure in this mechanism is read out of.
    # Renaming it at creation leaves every query pointing at nothing, so both
    # packages must go red — a run where this scores UNNOTICED is a run whose
    # other rows mean nothing.
    Case(
        "CONTROL known-positive: the daily-spend table is created under another name",
        [("		CREATE TABLE IF NOT EXISTS spend_daily (",
          "		CREATE TABLE IF NOT EXISTS spend_daily_renamed (")],
    ),
    # Known-negative: the wording of a log line nothing reads. The suite asserts
    # what the response carries, never what the process printed.
    Case(
        "CONTROL known-negative: the list-meta log line is rewritten",
        [('		log.Printf("[spend] list meta %s: %v", provider, err)',
          '		log.Printf("[spend] could not list key metadata for %s: %v", provider, err)')],
        expected_unnoticed="the suite asserts the response body, and no test reads the process log",
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
