package evalstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// writeSyntheticBench writes a bench directory shaped like the Python one:
// two email cases and one demo case, two replayed models, a two-candidate
// judging (judge-) and a three-candidate one (judge3-), each by two judges.
func writeSyntheticBench(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, lines ...any) {
		var b strings.Builder
		for _, line := range lines {
			b.WriteString(mustJSON(line) + "\n")
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	model := map[string]any{"type": "model", "identity": map[string]string{"modelId": "model-original"}}
	filing := func(ref string) json.RawMessage {
		return json.RawMessage(`{"filings":[` + syntheticFiling(ref, syntheticCardA) + `]}`)
	}
	demo := json.RawMessage(`{"messages":[{"key":"msg-1","subject":"s","body":"Hi Grace,"},{"key":"msg-2","subject":"s","body":"Hi Ada,"}]}`)
	write("cases.json", []map[string]any{
		{"id": "transcript-1", "cmd": "email", "model": model, "system": "File mail.", "prompt": syntheticFilingPrompt("e1"), "answer": filing("e1"),
			"tin": 3, "tcr": 1000, "tcw": 200, "tout": 80, "ts0": "2026-01-01T10:00:00.000Z", "ts1": "2026-01-01T10:00:04.500Z"},
		{"id": "transcript-2", "cmd": "email", "model": model, "system": "File mail.", "prompt": syntheticFilingPrompt("e2"), "answer": filing("e2"),
			"tin": 3, "tcr": 1000, "tcw": 400, "tout": 120, "ts0": "2026-01-01T11:00:00.000Z", "ts1": "2026-01-01T11:00:02.000Z"},
		{"id": "transcript-3", "cmd": "demo", "model": model, "system": "Write mail.", "prompt": syntheticDemoPrompt, "answer": demo,
			"tin": 3, "tcr": 500, "tcw": 0, "tout": 900, "ts0": "2026-01-01T12:00:00.000Z", "ts1": "2026-01-01T12:00:10.000Z"},
	})
	write("email_schemas.json", map[string]any{"report": map[string]any{"type": "object"}, "max": 8192})
	write("demo_schema.json", map[string]any{"schema": map[string]any{"type": "object"}, "max": 8192})
	replay := func(id, cmd, model string, answer json.RawMessage) map[string]any {
		return map[string]any{"id": id, "cmd": cmd, "model_req": model, "model": model, "parsed": answer, "duration_ms": 3000,
			"usage": map[string]int{"input_tokens": 1500, "output_tokens": 40, "cache_read_tokens": 100}}
	}
	write("replay-codex.jsonl", replay("transcript-1", "email", "model-small", filing("e1")), replay("transcript-2", "email", "model-small", filing("e2")),
		replay("transcript-3", "demo", "model-small", demo))
	write("replay-claude.jsonl", replay("transcript-1", "email", "model-large", filing("e1")), replay("transcript-2", "email", "model-large", filing("e2")),
		replay("transcript-3", "demo", "model-large", demo))
	judgment := func(id, cmd string, mapping map[string]string, ranking []string, malformed bool) map[string]any {
		var verdicts []map[string]any
		for _, label := range ranking {
			verdicts = append(verdicts, map[string]any{"label": label, "acceptable": label != "B", "failure_kind": "none", "reason": "r"})
		}
		if malformed {
			ranking = ranking[:1]
		}
		return map[string]any{"id": id, "cmd": cmd, "mapping": mapping, "parsed": map[string]any{"ranking": ranking, "verdicts": verdicts},
			"usage": map[string]int{"input_tokens": 5000, "output_tokens": 300}, "duration_ms": 9000}
	}
	two := map[string]string{"A": "model-small", "B": "model-original"}
	three := map[string]string{"A": "model-large", "B": "model-small", "C": "model-original"}
	for _, judge := range []string{"judge-a", "judge-b"} {
		write("judge-"+judge+".jsonl", judgment("transcript-1", "email", two, []string{"A", "B"}, false),
			judgment("transcript-2", "email", two, []string{"B", "A"}, false), judgment("transcript-3", "demo", two, []string{"A", "B"}, false))
		write("judge3-"+judge+".jsonl", judgment("transcript-1", "email", three, []string{"A", "B", "C"}, false),
			judgment("transcript-2", "email", three, []string{"C", "B", "A"}, judge == "judge-b"),
			judgment("transcript-3", "demo", three, []string{"B", "A", "C"}, false))
	}
	return dir
}

func TestTheLegacyBenchImportsAsTwoSuitesAndOneRunPerJudgingAndSuite(t *testing.T) {
	store := openTestStore(t)
	dir := writeSyntheticBench(t)
	result, err := store.ImportLegacyBench(context.Background(), dir, syntheticCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Suites) != 2 || result.Cases != 3 || len(result.Runs) != 4 {
		t.Fatalf("result %+v", result)
	}
	byJudging := map[string]LegacyImportRun{}
	for _, run := range result.Runs {
		if run.SuiteID == result.Suites[0] {
			byJudging[run.Judging] = run
		}
	}
	if got := strings.Join(byJudging["judge"].Candidates, ","); got != "model-original,model-small" {
		t.Errorf("two-candidate order %s", got)
	}
	if got := strings.Join(byJudging["judge3"].Candidates, ","); got != "model-original,model-small,model-large" {
		t.Errorf("three-candidate order %s: the original first, then each model where it first appeared", got)
	}

	email, err := store.GetSuite(result.Suites[0])
	if err != nil {
		t.Fatal(err)
	}
	if email.TaskKind != "email-filing" || email.CheckerName != "email-filing" || email.BaselineModelID != "model-original" || email.MaxTokens != 8192 ||
		email.CostModel.Batching == nil || email.WeeklyCallsSource.CallsPerWeek != 190 {
		t.Errorf("email suite %+v", email)
	}
	cases, _ := store.ListCases(email.ID)
	if len(cases) != 2 || cases[0].SourceKind != SourceKindClaudeCodeTranscript || cases[0].SourceRef != "transcript-1" ||
		*cases[0].OriginalDurationMs != 4500 || cases[0].OriginalCacheWriteTokens != 200 {
		t.Errorf("email cases %+v", cases[0])
	}

	run3, err := store.GetRun(byJudging["judge3"].RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run3.Status != RunStatusCompleted || run3.Origin != RunOriginLegacyImport || !strings.Contains(run3.PricesNote, "model-store at import") {
		t.Errorf("run %+v", run3)
	}
	answers, _ := store.ListAnswers(run3.ID, AnswerFilter{})
	origins := map[string]int{}
	for _, a := range answers {
		origins[a.Origin]++
	}
	if origins[AnswerOriginCaseOriginal] != 2 || origins[AnswerOriginLegacyImport] != 4 {
		t.Errorf("answer origins %v", origins)
	}

	report, err := store.BuildReport(context.Background(), run3.ID, ReportOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Judges[1].Malformed != 1 || report.Judges[0].Malformed != 0 {
		t.Errorf("judges %+v", report.Judges)
	}
	// judge-a: case 1 ranks large, small, original; case 2 original, small, large.
	original := report.Models[0]
	if original.ModelID != "model-original" || *original.PerJudge[0].MeanRank != 2 || original.PerJudge[0].Judged != 2 || original.PerJudge[1].Judged != 1 {
		t.Errorf("original %+v", original.PerJudge)
	}
	// Label B was unacceptable in every verdict: model-small here.
	if small := report.Models[1]; small.PerJudge[0].Acceptable != 0 || small.AllJudgesJudged != 1 {
		t.Errorf("small %+v", small)
	}
	if len(report.Checks) == 0 || report.BaselineAgreement == nil {
		t.Errorf("checks %v agreement %v", report.Checks, report.BaselineAgreement)
	}

	if _, err := store.ImportLegacyBench(context.Background(), dir, syntheticCatalog()); !errors.Is(err, ErrConflict) {
		t.Errorf("a second import = %v, want conflict", err)
	}
}

func TestTheLegacyImportRefusesAModelModelStoreDoesNotKnow(t *testing.T) {
	store := openTestStore(t)
	catalog := syntheticCatalog()
	delete(catalog, "model-large")
	if _, err := store.ImportLegacyBench(context.Background(), writeSyntheticBench(t), catalog); err == nil || !strings.Contains(err.Error(), "model-large") {
		t.Errorf("import = %v", err)
	}
	if suites, _ := store.ListSuites(); len(suites) != 0 {
		t.Errorf("a refused import left %d suites", len(suites))
	}
}

// fakeOperations is llm-bridge-server's operation read side.
type fakeOperations struct {
	receipts []msg.OperationReceipt
	intents  map[string]msg.OperationIntent
}

func (f *fakeOperations) ListOperations(_ context.Context, filter OperationsFilter) ([]msg.OperationReceipt, error) {
	var out []msg.OperationReceipt
	for _, r := range f.receipts {
		if strings.HasPrefix(r.CorrelationID, filter.CorrelationIDPrefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeOperations) ReadIntent(_ context.Context, id string) (msg.OperationIntent, error) {
	intent, ok := f.intents[id]
	if !ok {
		return msg.OperationIntent{}, fmt.Errorf("no intent %s", id)
	}
	return intent, nil
}

func syntheticOperations(n int) *fakeOperations {
	f := &fakeOperations{intents: map[string]msg.OperationIntent{}}
	for i := range n {
		id := fmt.Sprintf("operation_%02d", i)
		correlation := "email-run-1"
		if i%3 == 2 {
			correlation = "other-job"
		}
		schema := `{"type":"object"}`
		if i == 4 {
			schema = `{"type":"object","title":"older"}`
		}
		input, _ := json.Marshal(msg.LLMCompletionInput{Prompt: syntheticFilingPrompt(fmt.Sprint("e", i)), SystemPrompt: "File mail.", Model: "model-original",
			Schema: json.RawMessage(schema)})
		f.intents[id] = msg.OperationIntent{Type: msg.OperationTypeLLMCompletion, Input: input}
		result, _ := json.Marshal(msg.LLMCompletionResult{Parsed: json.RawMessage(`{"filings":[]}`)})
		detail, _ := json.Marshal(map[string]any{"answered_by": "model-original", "duration_ms": 2500})
		f.receipts = append(f.receipts, msg.OperationReceipt{ID: id, CorrelationID: correlation, State: msg.OperationStateSucceeded, Result: result,
			Usage:     &msg.OperationUsage{Tokens: msg.TokenUsage{InputTokens: 900, OutputTokens: 30}},
			Evidence:  []msg.OperationEvidence{{Kind: "model_call", Detail: detail}},
			CreatedAt: time.Unix(1_700_000_000+int64(i), 0)})
	}
	return f
}

func TestImportingOperationsSamplesByCorrelationAndSkipsWhatIsAlreadyIn(t *testing.T) {
	store := openTestStore(t)
	suite := emailSuite(t, store)
	source := syntheticOperations(9) // 6 carry the email-run- correlation
	request := ImportOperationsRequest{OperationsFilter: OperationsFilter{CorrelationIDPrefix: "email-run-"}, SampleSize: 4, Seed: 3}
	first, err := store.ImportOperations(context.Background(), suite.ID, request, source)
	if err != nil {
		t.Fatal(err)
	}
	if first.Matched != 6 || len(first.Imported) != 4 || first.AlreadyImported != 0 {
		t.Fatalf("first import %+v", first)
	}
	second, err := store.ImportOperations(context.Background(), suite.ID, request, source)
	if err != nil {
		t.Fatal(err)
	}
	if second.AlreadyImported != 4 || len(second.Imported) != 2 {
		t.Errorf("second import %+v, want the 2 left", second)
	}
	if len(first.SchemaDiffers)+len(second.SchemaDiffers) != 1 {
		t.Errorf("schema differs: %v %v, want operation_04 alone", first.SchemaDiffers, second.SchemaDiffers)
	}
	c, err := store.GetCase(first.Imported[0])
	if err != nil {
		t.Fatal(err)
	}
	if c.SourceKind != SourceKindLLMBridgeOperation || !strings.HasPrefix(c.SourceRef, "operation_") || c.OriginalModelID != "model-original" ||
		c.OriginalInputTokens != 900 || *c.OriginalDurationMs != 2500 || c.Features["emails"] != 1 {
		t.Errorf("case %+v", c)
	}
}

func TestImportingOperationsWithoutABridgeIsRefused(t *testing.T) {
	store := openTestStore(t)
	suite := emailSuite(t, store)
	if _, err := store.ImportOperations(context.Background(), suite.ID, ImportOperationsRequest{}, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("import with no bridge = %v", err)
	}
}
