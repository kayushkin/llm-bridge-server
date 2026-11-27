package models

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"

	_ "modernc.org/sqlite"
)

// standaloneSchema is the schema a long-lived standalone store's file ends at:
// the tables the old credential code made are still there.
const standaloneSchema = `
CREATE TABLE providers (id TEXT PRIMARY KEY, name TEXT NOT NULL, base_url TEXT, auth_type TEXT NOT NULL DEFAULT 'api_key');
CREATE TABLE models (id TEXT PRIMARY KEY, provider TEXT NOT NULL REFERENCES providers(id), name TEXT NOT NULL,
	max_tokens INTEGER DEFAULT 200000, input_cost REAL DEFAULT 0, output_cost REAL DEFAULT 0,
	enabled INTEGER DEFAULT 1, priority INTEGER DEFAULT 100, short_name TEXT DEFAULT '');
CREATE TABLE model_aliases (alias TEXT PRIMARY KEY, model_id TEXT NOT NULL REFERENCES models(id));
CREATE TABLE model_health (model TEXT PRIMARY KEY, last_success_at INTEGER DEFAULT 0, last_success_ms INTEGER DEFAULT 0,
	avg_response_ms INTEGER DEFAULT 0, last_error_at INTEGER DEFAULT 0, last_error TEXT DEFAULT '', consecutive_errors INTEGER DEFAULT 0);
CREATE TABLE credentials (id TEXT PRIMARY KEY, provider TEXT NOT NULL, label TEXT DEFAULT '', auth_type TEXT NOT NULL, api_key TEXT);
CREATE TABLE model_credentials (model_id TEXT NOT NULL, credential_id TEXT NOT NULL, priority INTEGER DEFAULT 100, PRIMARY KEY (model_id, credential_id));
CREATE TABLE "usage" (date TEXT NOT NULL, agent TEXT NOT NULL, orchestrator TEXT NOT NULL DEFAULT 'inber', model TEXT NOT NULL,
	provider TEXT NOT NULL, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, requests INTEGER DEFAULT 0,
	cost_usd REAL DEFAULT 0, PRIMARY KEY (date, agent, orchestrator, model));
CREATE TABLE model_roles (role TEXT PRIMARY KEY, model_id TEXT NOT NULL REFERENCES models(id));
CREATE TABLE model_role_fallbacks (role TEXT NOT NULL REFERENCES model_roles(role), position INTEGER NOT NULL,
	model_id TEXT NOT NULL REFERENCES models(id), PRIMARY KEY (role, position), UNIQUE (role, model_id));
`

// The rows are written out of alphabetical order, so the test shows the
// import keeps the order the file listed providers and aliases in.
const standaloneRows = `
INSERT INTO providers VALUES ('zeta', 'Zeta', 'https://zeta.example/v4', 'api_key');
INSERT INTO providers VALUES ('anthropic', 'Anthropic', NULL, 'api_key');
INSERT INTO providers VALUES ('openai', 'OpenAI', NULL, 'api_key');
INSERT INTO models VALUES ('model-b', 'anthropic', 'Model B', 200000, 3.0, 15.0, 1, 10, 'b');
INSERT INTO models VALUES ('model-a', 'openai', 'Model A', 400000, 1.25, 10.0, 0, 30, 'a');
INSERT INTO models VALUES ('model-c', 'zeta', 'Model C', 128000, 0.5, 2.0, 1, 10, '');
INSERT INTO model_aliases VALUES ('second', 'model-b');
INSERT INTO model_aliases VALUES ('first', 'model-b');
INSERT INTO model_aliases VALUES ('other', 'model-a');
INSERT INTO model_health VALUES ('model-b', 1700000000, 900, 1000, 0, '', 0);
INSERT INTO model_health VALUES ('a-name-no-model-has', 0, 0, 0, 1700000100, 'boom', 3);
INSERT INTO credentials VALUES ('credential-1', 'anthropic', 'made up', 'api_key', 'not-a-real-key');
INSERT INTO usage VALUES ('2026-03-02', 'agent', 'inber', 'model-b', 'anthropic', 10, 20, 1, 0.25);
INSERT INTO model_roles VALUES ('best', 'model-b');
INSERT INTO model_roles VALUES ('efficient', 'model-a');
INSERT INTO model_role_fallbacks VALUES ('best', 1, 'model-c');
INSERT INTO model_role_fallbacks VALUES ('best', 2, 'model-a');
`

func writeStandaloneFile(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	file, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, statement := range statements {
		if _, err := file.Exec(statement); err != nil {
			t.Fatalf("writing the standalone file: %v", err)
		}
	}
	return path
}

func emptyModule(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	return db, New(db)
}

func TestImportCopiesTheRegistryAsTheFileHoldsIt(t *testing.T) {
	path := writeStandaloneFile(t, standaloneSchema, standaloneRows)
	db, store := emptyModule(t)

	counts, err := ImportStandaloneDatabase(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	wantCopied := map[string]int64{"providers": 3, "models": 3, "model_aliases": 3, "model_roles": 2,
		"model_role_fallbacks": 2, "model_health": 2, "usage": 1}
	if !reflect.DeepEqual(counts.Copied, wantCopied) {
		t.Errorf("copied %v, want %v", counts.Copied, wantCopied)
	}
	if want := map[string]int64{"credentials": 1, "model_credentials": 0}; !reflect.DeepEqual(counts.LeftInFile, want) {
		t.Errorf("left in the file %v, want %v", counts.LeftInFile, want)
	}

	providers, err := store.Providers()
	if err != nil {
		t.Fatal(err)
	}
	var providerIDs []string
	for _, provider := range providers {
		providerIDs = append(providerIDs, provider.ID)
	}
	if want := []string{"zeta", "anthropic", "openai"}; !reflect.DeepEqual(providerIDs, want) {
		t.Errorf("providers in order %v, want the file's order %v", providerIDs, want)
	}

	statuses, err := store.AllModelsWithStatus()
	if err != nil {
		t.Fatal(err)
	}
	var modelIDs []string
	for _, status := range statuses {
		modelIDs = append(modelIDs, status.ID)
	}
	if want := []string{"model-b", "model-c", "model-a"}; !reflect.DeepEqual(modelIDs, want) {
		t.Errorf("models %v, want %v (priority, then id)", modelIDs, want)
	}
	b := statuses[0]
	if !reflect.DeepEqual(b.Aliases, []string{"second", "first"}) {
		t.Errorf("aliases of model-b %v, want the file's order [second first]", b.Aliases)
	}
	if b.Name != "Model B" || b.ShortName != "b" || b.MaxTokens != 200000 || b.InputCost != 3.0 || b.OutputCost != 15.0 || !b.Enabled || b.Priority != 10 {
		t.Errorf("model-b came through as %+v", b.Model)
	}
	if b.Health.LastSuccessAt.Unix() != 1700000000 || b.Health.AvgResponseMs != 1000 {
		t.Errorf("model-b health %+v", b.Health)
	}
	if statuses[2].Enabled {
		t.Error("model-a was disabled in the file and came through enabled")
	}
	if health := store.GetHealth("a-name-no-model-has"); health.ConsecutiveErr != 3 || health.LastError != "boom" {
		t.Errorf("health kept under a name with no model came through as %+v", health)
	}

	roles, err := store.RoleModelLists()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"best": {"model-b", "model-c", "model-a"}, "efficient": {"model-a"}}; !reflect.DeepEqual(roles, want) {
		t.Errorf("roles %v, want %v", roles, want)
	}

	var baseURL sql.NullString
	if err := db.QueryRow(`SELECT base_url FROM providers WHERE id = 'zeta'`).Scan(&baseURL); err != nil || baseURL.String != "https://zeta.example/v4" {
		t.Errorf("zeta's base_url came through as %v (%v)", baseURL, err)
	}

	// A provider or alias added after the import goes after the copied ones.
	if err := store.AddProvider(Provider{ID: "added", Name: "Added"}); err != nil {
		t.Fatal(err)
	}
	if providers, _ = store.Providers(); providers[len(providers)-1].ID != "added" {
		t.Errorf("a provider added after the import is not listed last: %v", providers)
	}
	if err := store.AddAlias("model-b", "third"); err != nil {
		t.Fatal(err)
	}
	if model, _ := store.Models("anthropic"); !reflect.DeepEqual(model[0].Aliases, []string{"second", "first", "third"}) {
		t.Errorf("aliases after adding one: %v", model[0].Aliases)
	}
}

func TestImportTakesAFileWithoutTheOldTables(t *testing.T) {
	path := writeStandaloneFile(t, `
CREATE TABLE providers (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE models (id TEXT PRIMARY KEY, provider TEXT NOT NULL, name TEXT NOT NULL, max_tokens INTEGER DEFAULT 200000,
	input_cost REAL DEFAULT 0, output_cost REAL DEFAULT 0, enabled INTEGER DEFAULT 1, priority INTEGER DEFAULT 100, short_name TEXT DEFAULT '');
CREATE TABLE model_aliases (alias TEXT PRIMARY KEY, model_id TEXT NOT NULL);
CREATE TABLE model_roles (role TEXT PRIMARY KEY, model_id TEXT NOT NULL);
CREATE TABLE model_role_fallbacks (role TEXT NOT NULL, position INTEGER NOT NULL, model_id TEXT NOT NULL, PRIMARY KEY (role, position), UNIQUE (role, model_id));
CREATE TABLE model_health (model TEXT PRIMARY KEY, last_success_at INTEGER DEFAULT 0, last_success_ms INTEGER DEFAULT 0,
	avg_response_ms INTEGER DEFAULT 0, last_error_at INTEGER DEFAULT 0, last_error TEXT DEFAULT '', consecutive_errors INTEGER DEFAULT 0);
INSERT INTO providers VALUES ('anthropic', 'Anthropic');
INSERT INTO models VALUES ('model-b', 'anthropic', 'Model B', 200000, 3.0, 15.0, 1, 10, 'b');
`)
	db, store := emptyModule(t)
	counts, err := ImportStandaloneDatabase(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Copied["providers"] != 1 || counts.Copied["models"] != 1 || len(counts.LeftInFile) != 0 {
		t.Errorf("counts %+v", counts)
	}
	if _, present := counts.Copied["usage"]; present {
		t.Error("a file with no usage table reported copying one")
	}
	if model, err := store.ResolveModel("model-b"); err != nil || model.Provider != "anthropic" {
		t.Errorf("model-b came through as %+v (%v)", model, err)
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	cases := []struct {
		name      string
		file      []string
		wantError string
	}{
		{"a table it does not know", []string{standaloneSchema, standaloneRows, `CREATE TABLE notes (id TEXT); INSERT INTO notes VALUES ('x');`}, "does not know"},
		{"a column it does not know", []string{standaloneSchema, standaloneRows, `ALTER TABLE models ADD COLUMN color TEXT;`}, "models"},
		{"an enabled flag that is not 0 or 1", []string{standaloneSchema, standaloneRows, `UPDATE models SET enabled = 2 WHERE id = 'model-a';`}, "not 0 or 1"},
		{"an alias of a model the file does not hold", []string{standaloneSchema, standaloneRows, `INSERT INTO model_aliases VALUES ('dangling', 'gone');`}, "dangling"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeStandaloneFile(t, testCase.file...)
			db, _ := emptyModule(t)
			_, err := ImportStandaloneDatabase(context.Background(), db, path)
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("err = %v, want one containing %q", err, testCase.wantError)
			}
			var models int
			if err := db.QueryRow(`SELECT COUNT(*) FROM models`).Scan(&models); err != nil || models != 0 {
				t.Errorf("a refused import left %d models (%v)", models, err)
			}
		})
	}
}

func TestImportRefusesAModuleThatHoldsRows(t *testing.T) {
	path := writeStandaloneFile(t, standaloneSchema, standaloneRows)
	db, store := emptyModule(t)
	if err := store.AddProvider(Provider{ID: "already", Name: "Already"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("err = %v, want a refusal of a module that holds rows", err)
	}
}
