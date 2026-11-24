package modelstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedProviderWithModels registers one provider plus the given models.
func seedProviderWithModels(t *testing.T, s *Store, provider string, models ...Model) {
	t.Helper()
	if err := s.AddProvider(Provider{ID: provider, Name: provider}); err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if err := s.AddModel(m); err != nil {
			t.Fatalf("AddModel(%s): %v", m.ID, err)
		}
	}
}

func TestProvidersAndModels(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-b", Provider: "anthropic", Name: "B", Enabled: true, Priority: 20},
		Model{ID: "claude-a", Provider: "anthropic", Name: "A", Enabled: true, Priority: 10, Aliases: []string{"sonnet"}},
	)

	providers, err := s.Providers()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].ID != "anthropic" {
		t.Fatalf("expected the one anthropic provider, got %+v", providers)
	}

	models, err := s.Models("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	// Models are ordered by priority, not insertion order.
	if models[0].ID != "claude-a" || models[1].ID != "claude-b" {
		t.Errorf("expected priority order [claude-a claude-b], got [%s %s]", models[0].ID, models[1].ID)
	}
	if len(models[0].Aliases) != 1 || models[0].Aliases[0] != "sonnet" {
		t.Errorf("expected alias sonnet to round-trip, got %v", models[0].Aliases)
	}

	// A provider with nothing registered has no models.
	other, err := s.Models("openai")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("expected no models for unregistered provider, got %d", len(other))
	}
}

func TestResolveModelByIDAndAlias(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-test", Provider: "anthropic", Name: "Test", Enabled: true, Priority: 10, Aliases: []string{"sonnet", "claude-sonnet"}},
	)

	byID, err := s.ResolveModel("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != "claude-test" || byID.Provider != "anthropic" {
		t.Errorf("resolve by id: got %+v", byID)
	}
	if !byID.Enabled {
		t.Error("resolve by id: expected Enabled to survive the round-trip")
	}

	for _, alias := range []string{"sonnet", "claude-sonnet"} {
		byAlias, err := s.ResolveModel(alias)
		if err != nil {
			t.Fatalf("resolve alias %q: %v", alias, err)
		}
		if byAlias.ID != "claude-test" {
			t.Errorf("resolve alias %q: expected claude-test, got %q", alias, byAlias.ID)
		}
	}

	if _, err := s.ResolveModel("no-such-model"); err == nil {
		t.Error("expected an error resolving an unknown model, got nil")
	}
}

func TestProviderFor(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "openai",
		Model{ID: "gpt-test", Provider: "openai", Name: "GPT", Enabled: true, Priority: 10, Aliases: []string{"gpt"}},
	)

	// ProviderFor resolves through aliases, since it delegates to ResolveModel.
	for _, ref := range []string{"gpt-test", "gpt"} {
		provider, err := s.ProviderFor(ref)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", ref, err)
		}
		if provider != "openai" {
			t.Errorf("ProviderFor(%q) = %q, want openai", ref, provider)
		}
	}

	if _, err := s.ProviderFor("no-such-model"); err == nil {
		t.Error("expected an error for an unknown model, got nil")
	}
}

func TestSetEnabled(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-test", Provider: "anthropic", Name: "Test", Enabled: true, Priority: 10},
	)

	if err := s.SetEnabled("claude-test", false); err != nil {
		t.Fatal(err)
	}
	m, err := s.ResolveModel("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if m.Enabled {
		t.Error("expected model to be disabled")
	}

	if err := s.SetEnabled("claude-test", true); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ResolveModel("claude-test")
	if !m.Enabled {
		t.Error("expected model to be re-enabled")
	}

	// Toggling a model that doesn't exist is an error, not a silent no-op.
	if err := s.SetEnabled("no-such-model", false); err == nil {
		t.Error("expected an error for an unknown model, got nil")
	}
}

func TestSetCost(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-test", Provider: "anthropic", Name: "Test", Enabled: true, Priority: 10, InputCost: 1, OutputCost: 2},
	)

	if err := s.SetCost("claude-test", 3.5, 14); err != nil {
		t.Fatal(err)
	}
	m, err := s.ResolveModel("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if m.InputCost != 3.5 || m.OutputCost != 14 {
		t.Errorf("expected cost 3.5/14, got %v/%v", m.InputCost, m.OutputCost)
	}

	// Setting the cost of a model that doesn't exist is an error, not a no-op.
	if err := s.SetCost("no-such-model", 1, 1); err == nil {
		t.Error("expected an error for an unknown model, got nil")
	}
}

func TestSetPriority(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-test", Provider: "anthropic", Name: "Test", Enabled: true, Priority: 100},
	)

	if err := s.SetPriority("claude-test", 5); err != nil {
		t.Fatal(err)
	}
	m, _ := s.ResolveModel("claude-test")
	if m.Priority != 5 {
		t.Errorf("expected priority 5, got %d", m.Priority)
	}

	if err := s.SetPriority("no-such-model", 1); err == nil {
		t.Error("expected an error for an unknown model, got nil")
	}
}

// TestSetShortNameSurvivesEveryReadPath guards the failure this column is most
// likely to hit: a read path whose SELECT list forgot the column returns the
// zero value with no error at all, so the nickname just quietly vanishes on
// that one path while every other path looks fine.
func TestSetShortNameSurvivesEveryReadPath(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-test", Provider: "anthropic", Name: "Test", Enabled: true, Priority: 100,
			Aliases: []string{"testy"}},
	)

	if err := s.SetShortName("claude-test", "opus-4.6"); err != nil {
		t.Fatal(err)
	}

	byID, _ := s.ResolveModel("claude-test")
	if byID.ShortName != "opus-4.6" {
		t.Errorf("ResolveModel by ID: expected short name opus-4.6, got %q", byID.ShortName)
	}
	byAlias, _ := s.ResolveModel("testy")
	if byAlias.ShortName != "opus-4.6" {
		t.Errorf("ResolveModel by alias: expected short name opus-4.6, got %q", byAlias.ShortName)
	}
	listed, _ := s.Models("anthropic")
	if len(listed) != 1 || listed[0].ShortName != "opus-4.6" {
		t.Errorf("Models: expected short name opus-4.6, got %+v", listed)
	}
	chain, _ := s.FailoverChain()
	if len(chain) != 1 || chain[0].ShortName != "opus-4.6" {
		t.Errorf("FailoverChain: expected short name opus-4.6, got %+v", chain)
	}
	statuses, _ := s.AllModelsWithStatus()
	if len(statuses) != 1 || statuses[0].ShortName != "opus-4.6" {
		t.Errorf("AllModelsWithStatus: expected short name opus-4.6, got %+v", statuses)
	}

	// AddModel replaces the whole row, so a re-add that carries the short name
	// must keep it — this is the path every sync run takes.
	if err := s.AddModel(Model{ID: "claude-test", Provider: "anthropic", Name: "Test",
		Enabled: true, Priority: 100, ShortName: "opus-4.6"}); err != nil {
		t.Fatal(err)
	}
	reAdded, _ := s.ResolveModel("claude-test")
	if reAdded.ShortName != "opus-4.6" {
		t.Errorf("after AddModel re-add: expected short name opus-4.6, got %q", reAdded.ShortName)
	}

	if err := s.SetShortName("no-such-model", "nope"); err == nil {
		t.Error("expected an error for an unknown model, got nil")
	}
}

func TestAddAndRemoveAlias(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-a", Provider: "anthropic", Name: "A", Enabled: true, Priority: 10},
		Model{ID: "claude-b", Provider: "anthropic", Name: "B", Enabled: true, Priority: 20},
	)

	if err := s.AddAlias("claude-a", "sonnet"); err != nil {
		t.Fatal(err)
	}
	m, err := s.ResolveModel("sonnet")
	if err != nil {
		t.Fatalf("resolve alias after AddAlias: %v", err)
	}
	if m.ID != "claude-a" {
		t.Errorf("expected alias to resolve to claude-a, got %s", m.ID)
	}

	// Re-adding the same alias->model pair is an idempotent success.
	if err := s.AddAlias("claude-a", "sonnet"); err != nil {
		t.Errorf("re-adding the same alias should be a no-op, got %v", err)
	}

	// Repointing an existing alias to a different model must fail loudly.
	if err := s.AddAlias("claude-b", "sonnet"); err == nil {
		t.Error("expected repointing an existing alias to error, got nil")
	}

	// An alias for a model that doesn't exist is rejected.
	if err := s.AddAlias("no-such-model", "ghost"); err == nil {
		t.Error("expected an error aliasing an unknown model, got nil")
	}

	// An alias that shadows an existing model ID would be unreachable; reject it.
	if err := s.AddAlias("claude-a", "claude-b"); err == nil {
		t.Error("expected an error for an alias colliding with a model ID, got nil")
	}

	// Remove it and confirm it stops resolving.
	if err := s.RemoveAlias("sonnet"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModel("sonnet"); err == nil {
		t.Error("expected the alias to stop resolving after removal")
	}

	// Removing a nonexistent alias is an error, not a no-op.
	if err := s.RemoveAlias("sonnet"); err == nil {
		t.Error("expected an error removing a nonexistent alias, got nil")
	}
}

func TestDeleteModel(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "claude-test", Provider: "anthropic", Name: "Test", Enabled: true, Priority: 10, Aliases: []string{"sonnet", "claude-sonnet"}},
	)

	if err := s.DeleteModel("claude-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModel("claude-test"); err == nil {
		t.Error("expected the model to be gone after DeleteModel")
	}
	// Its aliases must be gone too, not left dangling.
	for _, alias := range []string{"sonnet", "claude-sonnet"} {
		if _, err := s.ResolveModel(alias); err == nil {
			t.Errorf("expected alias %q to be removed with its model", alias)
		}
	}

	// Deleting a model that doesn't exist is an error, not a no-op.
	if err := s.DeleteModel("no-such-model"); err == nil {
		t.Error("expected an error deleting an unknown model, got nil")
	}
}

func TestFailoverChainOrdersByPriorityAndSkipsDisabled(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "third", Provider: "anthropic", Name: "Third", Enabled: true, Priority: 30},
		Model{ID: "first", Provider: "anthropic", Name: "First", Enabled: true, Priority: 10},
		Model{ID: "second", Provider: "anthropic", Name: "Second", Enabled: true, Priority: 20},
		Model{ID: "off", Provider: "anthropic", Name: "Disabled", Enabled: false, Priority: 1},
	)

	chain, err := s.FailoverChain()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "second", "third"}
	if len(chain) != len(want) {
		t.Fatalf("expected %d models in the chain, got %d", len(want), len(chain))
	}
	for i, id := range want {
		if chain[i].ID != id {
			t.Errorf("chain[%d] = %q, want %q", i, chain[i].ID, id)
		}
	}

	// The disabled model sorts first by priority, so its absence proves the
	// enabled filter runs rather than the ordering hiding it.
	for _, m := range chain {
		if m.ID == "off" {
			t.Error("disabled model must not appear in the failover chain")
		}
	}
}

func TestAllModelsWithStatusJoinsHealth(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "healthy", Provider: "anthropic", Name: "Healthy", Enabled: true, Priority: 10},
		Model{ID: "untried", Provider: "anthropic", Name: "Untried", Enabled: true, Priority: 20},
	)
	s.RecordSuccess("healthy", 250)

	statuses, err := s.AllModelsWithStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}

	byID := map[string]ModelStatus{}
	for _, st := range statuses {
		byID[st.ID] = st
	}

	healthy := byID["healthy"]
	if healthy.Health == nil {
		t.Fatal("expected a health record for the model that reported success")
	}
	if healthy.Health.LastSuccessMs != 250 {
		t.Errorf("expected last_success_ms 250, got %d", healthy.Health.LastSuccessMs)
	}
	if healthy.Health.IsUnknown() {
		t.Error("a model that reported success must not be Unknown")
	}

	// A model that has never been called still gets a (zero) health record,
	// so dashboard callers can rely on Health being non-nil.
	untried := byID["untried"]
	if untried.Health == nil {
		t.Fatal("expected a non-nil health record even for an untried model")
	}
	if !untried.Health.IsUnknown() {
		t.Error("a model that was never called should be Unknown")
	}
}

func TestRecordSuccessAndError(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "m", Provider: "anthropic", Name: "M", Enabled: true, Priority: 10},
	)

	// Unknown until it's been called.
	if !s.GetHealth("m").IsUnknown() {
		t.Error("expected a never-called model to be Unknown")
	}

	// First success seeds the rolling average with the raw duration.
	s.RecordSuccess("m", 100)
	h := s.GetHealth("m")
	if h.LastSuccessMs != 100 {
		t.Errorf("expected last_success_ms 100, got %d", h.LastSuccessMs)
	}
	if h.AvgResponseMs != 100 {
		t.Errorf("expected the first sample to seed avg=100, got %d", h.AvgResponseMs)
	}

	// Subsequent successes fold in via the 0.3/0.7 exponential moving average:
	// (3*200 + 7*100) / 10 = 130.
	s.RecordSuccess("m", 200)
	h = s.GetHealth("m")
	if h.AvgResponseMs != 130 {
		t.Errorf("expected EMA 130, got %d", h.AvgResponseMs)
	}

	// Errors accumulate.
	s.RecordError("m", "timeout")
	h = s.GetHealth("m")
	if h.ConsecutiveErr != 1 {
		t.Errorf("expected 1 consecutive error, got %d", h.ConsecutiveErr)
	}
	if h.LastError != "timeout" {
		t.Errorf("expected last_error 'timeout', got %q", h.LastError)
	}
	s.RecordError("m", "500")
	h = s.GetHealth("m")
	if h.ConsecutiveErr != 2 {
		t.Errorf("expected 2 consecutive errors, got %d", h.ConsecutiveErr)
	}

	// A success clears the error streak.
	s.RecordSuccess("m", 150)
	h = s.GetHealth("m")
	if h.ConsecutiveErr != 0 {
		t.Errorf("expected the error streak to reset on success, got %d", h.ConsecutiveErr)
	}
	if h.LastError != "" {
		t.Errorf("expected last_error to clear on success, got %q", h.LastError)
	}
}

func TestModelHealthIsHealthy(t *testing.T) {
	now := time.Now()
	window := time.Hour

	tests := []struct {
		name   string
		health *ModelHealth
		want   bool
	}{
		{"nil is never healthy", nil, false},
		{"never seen", &ModelHealth{Model: "m"}, false},
		{"recent success", &ModelHealth{Model: "m", LastSuccessAt: now.Add(-time.Minute)}, true},
		{"success older than the window is stale", &ModelHealth{Model: "m", LastSuccessAt: now.Add(-2 * time.Hour)}, false},
		// RecordError stamps LastErrorAt and increments ConsecutiveErr in one
		// statement, and RecordSuccess clears both together. So a fixture that
		// sets the stamp must set the counter too; leaving the counter at its
		// zero value builds a row no writer can produce.
		{"error after the last success", &ModelHealth{
			Model:          "m",
			LastSuccessAt:  now.Add(-10 * time.Minute),
			LastErrorAt:    now.Add(-time.Minute),
			ConsecutiveErr: 1,
		}, false},
		// The case the old timestamp comparison got wrong: stored at unix-second
		// resolution, a fast failure carries the same stamp as the success before it.
		{"error in the same second as the last success", &ModelHealth{
			Model:          "m",
			LastSuccessAt:  now.Add(-time.Minute),
			LastErrorAt:    now.Add(-time.Minute),
			ConsecutiveErr: 1,
		}, false},
		// A success clears the streak, so this is a legacy row: an error stamp
		// left behind by an older writer, already superseded by a success.
		{"error before the last success", &ModelHealth{
			Model:         "m",
			LastSuccessAt: now.Add(-time.Minute),
			LastErrorAt:   now.Add(-10 * time.Minute),
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.health.IsHealthy(window); got != tt.want {
				t.Errorf("IsHealthy(%v) = %v, want %v", window, got, tt.want)
			}
		})
	}
}

func TestModelHealthIsUnknown(t *testing.T) {
	var nilHealth *ModelHealth
	if !nilHealth.IsUnknown() {
		t.Error("nil health should be Unknown")
	}
	if !(&ModelHealth{Model: "m"}).IsUnknown() {
		t.Error("a health record with no success and no error should be Unknown")
	}
	if (&ModelHealth{Model: "m", LastErrorAt: time.Now()}).IsUnknown() {
		t.Error("a model that has errored has been tried, so it is not Unknown")
	}
	if (&ModelHealth{Model: "m", LastSuccessAt: time.Now()}).IsUnknown() {
		t.Error("a model that has succeeded has been tried, so it is not Unknown")
	}
}

func TestFreshDB(t *testing.T) {
	// Ensure Open works on a completely fresh path
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("DB file should exist")
	}

	// Should be able to seed and use
	if err := s.Seed(); err != nil {
		t.Fatal(err)
	}

	providers, _ := s.Providers()
	if len(providers) == 0 {
		t.Error("expected seeded providers")
	}
}

// TestAnErrorInTheSameSecondAsASuccessStillMarksTheModelUnhealthy pins the
// defect that made this file's older health tests assert on ConsecutiveErr and
// LastError instead of on IsHealthy.
//
// model_health stores unix seconds. When IsHealthy decided "the last attempt
// failed" by testing LastErrorAt.After(LastSuccessAt), a success and an error
// landing in the same second compared equal — not after — and a model that had
// just failed kept reporting healthy. Recording back-to-back, as here, is the
// ordinary case for an error that fails fast (400, 404, an instant refusal).
//
// The two calls straddle a second boundary about 1 run in 500 (measured
// 2026-08-31: 10 in 5000). A skip then would read as a pass everywhere, so each
// attempt uses a fresh store and the test retries until the pair shares a
// second. Every attempt missing the precondition is a failure, not a skip.
func TestAnErrorInTheSameSecondAsASuccessStillMarksTheModelUnhealthy(t *testing.T) {
	const attempts = 5
	var h *ModelHealth
	for attempt := 1; ; attempt++ {
		s := tempStore(t)
		seedProviderWithModels(t, s, "anthropic",
			Model{ID: "m", Provider: "anthropic", Name: "M", Enabled: true, Priority: 10},
		)

		s.RecordSuccess("m", 100)
		s.RecordError("m", "529 overloaded")

		h = s.GetHealth("m")

		// The precondition the bug needed.
		if h.LastErrorAt.Equal(h.LastSuccessAt) {
			break
		}
		if attempt == attempts {
			t.Fatalf("success and error did not land in the same second in %d attempts "+
				"(last: %s vs %s); the same-second case is not being exercised",
				attempts, h.LastSuccessAt, h.LastErrorAt)
		}
	}

	if h.IsHealthy(24 * time.Hour) {
		t.Errorf("a model whose last recorded outcome was an error reported healthy "+
			"(consecutive_errors=%d, last_error=%q)", h.ConsecutiveErr, h.LastError)
	}
}

// TestHealthTracksTheLastOutcomeNotTheTimestampOrder walks a success/error/success
// cycle and asserts IsHealthy follows the outcome each time. The middle step is
// the one the timestamp comparison could not see.
func TestHealthTracksTheLastOutcomeNotTheTimestampOrder(t *testing.T) {
	s := tempStore(t)
	seedProviderWithModels(t, s, "anthropic",
		Model{ID: "m", Provider: "anthropic", Name: "M", Enabled: true, Priority: 10},
	)
	const window = 24 * time.Hour

	s.RecordSuccess("m", 100)
	if h := s.GetHealth("m"); !h.IsHealthy(window) {
		t.Fatal("a model that just succeeded should be healthy")
	}

	s.RecordError("m", "boom")
	if h := s.GetHealth("m"); h.IsHealthy(window) {
		t.Error("a model that just failed should not be healthy")
	}

	// A success clears the streak, so health returns without waiting for a clock tick.
	s.RecordSuccess("m", 120)
	if h := s.GetHealth("m"); !h.IsHealthy(window) {
		t.Error("a success after an error should restore health")
	}
}

// TestNeverSeenAndStaleStillOutrankTheOutcomeBit guards the two checks that run
// before the outcome test, so a future change to the outcome bit cannot make a
// never-called or long-silent model look healthy.
func TestNeverSeenAndStaleStillOutrankTheOutcomeBit(t *testing.T) {
	never := &ModelHealth{Model: "m"}
	if never.IsHealthy(24 * time.Hour) {
		t.Error("a model that has never succeeded must not be healthy")
	}

	stale := &ModelHealth{Model: "m", LastSuccessAt: time.Now().Add(-48 * time.Hour)}
	if stale.IsHealthy(24 * time.Hour) {
		t.Error("a success older than the window must not count as healthy")
	}
	if stale.LastRecordedOutcomeWasAnError() {
		t.Error("a clean-but-stale model has no error streak")
	}
}
