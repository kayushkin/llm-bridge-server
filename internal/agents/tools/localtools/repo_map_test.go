package localtools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo_map is the tool an agent calls to decide what a codebase contains
// before it reads anything, so every line it emits is a claim the agent acts
// on without checking. Measured on `main`: a panic() on the first line of
// RepoMap, buildRepoMap, parseGoFileCompact, parseGoFileFull,
// compactFuncSignature, compactTypeDecl, compactType, isExported, isStdlib,
// isRelevantFile OR shouldIgnore left `go test ./...` green. Eleven functions,
// one mechanism, executed by nothing.
//
// These tests characterise what the mechanism does today. Where the behavior
// looks wrong the test says so in a comment and pins it anyway — a
// characterisation suite exists to make the next change visible, not to assert
// the current answer is the right one.

func runRepoMap(t *testing.T, root string, ignore []string, params map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RepoMap(root, ignore).Run(context.Background(), string(raw))
	if err != nil {
		t.Fatalf("repo_map failed: %v", err)
	}
	return out
}

// writeTree lays out files under a fresh temp dir. Keys are slash-separated
// relative paths; parent directories are created as needed.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// sectionFor returns the body the map printed under "## <rel>", and whether
// the map mentioned that path at all. Reading a named section is what
// separates "the file is described wrongly" from "the file is missing", which
// a substring search over the whole document cannot do.
func sectionFor(out, rel string) (string, bool) {
	header := "## " + rel + "\n"
	i := strings.Index(out, header)
	if i < 0 {
		return "", false
	}
	rest := out[i+len(header):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimRight(rest, "\n"), true
}

func mustSection(t *testing.T, out, rel string) string {
	t.Helper()
	body, ok := sectionFor(out, rel)
	if !ok {
		t.Fatalf("the map has no section for %q; it printed:\n%s", rel, out)
	}
	return body
}

func requireNoSection(t *testing.T, out, rel string) {
	t.Helper()
	if body, ok := sectionFor(out, rel); ok {
		t.Errorf("the map describes %q, which it was meant to leave out; it printed:\n%s", rel, body)
	}
}

// ---- the document ----

func TestTheMapIsHeadedAndOneSectionPerFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.go":      "package a\n\nfunc Alpha() {}\n",
		"README.md": "hello",
	})

	out := runRepoMap(t, root, nil, map[string]any{})

	if !strings.HasPrefix(out, "# Repository Structure\n\n") {
		t.Errorf("the map lost its header; it starts:\n%.60q", out)
	}
	if got := strings.Count(out, "## "); got != 2 {
		t.Errorf("want one section per described file (2), got %d:\n%s", got, out)
	}
}

// The agent reads this top to bottom, so the order is part of the answer. It
// is sorted by path, which is not the order the walk produces.
func TestSectionsAreSortedByPathRatherThanWalkOrder(t *testing.T) {
	root := writeTree(t, map[string]string{
		"zeta/z.go":  "package zeta\n\nfunc Z() {}\n",
		"alpha/a.go": "package alpha\n\nfunc A() {}\n",
		"m.go":       "package m\n\nfunc M() {}\n",
	})

	out := runRepoMap(t, root, nil, map[string]any{})

	want := []string{
		"## " + filepath.Join("alpha", "a.go"),
		"## m.go",
		"## " + filepath.Join("zeta", "z.go"),
	}
	at := -1
	for _, header := range want {
		i := strings.Index(out, header)
		if i < 0 {
			t.Fatalf("section %q missing from:\n%s", header, out)
		}
		if i < at {
			t.Errorf("sections are out of order at %q:\n%s", header, out)
		}
		at = i
	}
}

// ⚠️ The fixture above cannot actually see the sort, and that is worth keeping
// separate rather than folding in. filepath.Walk already yields lexical order
// within a directory, so for most trees walk order and sorted order are the
// same sequence and deleting the sort changes nothing observable.
//
// They diverge exactly when a file and a directory share a prefix: the walk
// descends "a/" before it reaches "a.go" because "a" < "a.go", while a string
// sort puts "a.go" first because '.' is below '/'. Measured, not reasoned —
// walk gives [a/b.go a.go], sort gives [a.go a/b.go].
func TestTheSortIsWhatOrdersAFileAgainstADirectorySharingItsPrefix(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.go":   "package a\n\nfunc A() {}\n",
		"a/b.go": "package b\n\nfunc B() {}\n",
	})

	out := runRepoMap(t, root, nil, map[string]any{})

	file := strings.Index(out, "## a.go")
	nested := strings.Index(out, "## "+filepath.Join("a", "b.go"))
	if file < 0 || nested < 0 {
		t.Fatalf("a section is missing:\n%s", out)
	}
	if file > nested {
		t.Errorf("the map is in walk order rather than sorted order:\n%s", out)
	}
}

func TestAPathScopesTheMapToOneSubdirectory(t *testing.T) {
	root := writeTree(t, map[string]string{
		"inside/in.go":  "package inside\n\nfunc In() {}\n",
		"outside/on.go": "package outside\n\nfunc On() {}\n",
	})

	out := runRepoMap(t, root, nil, map[string]any{"path": "inside"})

	// Paths are relative to the scoped directory, not to the repo root.
	mustSection(t, out, "in.go")
	requireNoSection(t, out, filepath.Join("outside", "on.go"))
}

// A path that does not exist is a caller mistake, and the walk's error is the
// only thing that reports it. Swallowing it would answer an empty map, which
// reads exactly like a directory that really is empty.
func TestAMissingPathIsAnErrorRatherThanAnEmptyMap(t *testing.T) {
	root := writeTree(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n"})

	_, err := RepoMap(root, nil).Run(context.Background(), `{"path":"no-such-dir"}`)
	if err == nil {
		t.Fatal("mapping a directory that does not exist reported success")
	}
	if !strings.Contains(err.Error(), "failed to build repo map") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

// Run has two failure producers — the request that will not parse and the map
// that will not build — and only the second says what it was doing. "it
// returned an error" cannot tell them apart, so this test names the producer
// twice over: the cause is a JSON syntax problem, which the build arm cannot
// raise, and the message is not the build's.
//
// Measured 2026-08-21 on this branch: rewriting the arm to
// fmt.Errorf("failed to build repo map: %w", err) sends a caller with a typo in
// their request off to look at the tree instead, and leaves errors.As
// satisfied — the syntax error survives the wrap. Only the second assertion
// reddens on it.
func TestUnparseableInputIsReportedRatherThanTreatedAsDefaults(t *testing.T) {
	root := writeTree(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n"})

	_, err := RepoMap(root, nil).Run(context.Background(), "{not json")
	if err == nil {
		t.Fatal("a malformed input was accepted as an empty request")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("a malformed request must be reported as one, and only the unmarshal arm raises a *json.SyntaxError; got %v", err)
	}
	if strings.Contains(err.Error(), "failed to build repo map") {
		t.Errorf("a request that will not parse was reported as the map build's failure: %v", err)
	}
}

// ---- what is left out ----

func TestTheHeavyDirectoriesAreSkippedWholesale(t *testing.T) {
	root := writeTree(t, map[string]string{
		"keep.go":                "package keep\n\nfunc Keep() {}\n",
		".git/hooks/x.go":        "package hooks\n\nfunc X() {}\n",
		"node_modules/p/y.go":    "package p\n\nfunc Y() {}\n",
		"vendor/v/z.go":          "package v\n\nfunc Z() {}\n",
		".openclaw/o.go":         "package o\n\nfunc O() {}\n",
		"logs/l.go":              "package l\n\nfunc L() {}\n",
		"nested/logs/deep/d.go":  "package d\n\nfunc D() {}\n",
		"nested/kept/shallow.go": "package kept\n\nfunc S() {}\n",
	})

	out := runRepoMap(t, root, nil, map[string]any{})

	mustSection(t, out, "keep.go")
	mustSection(t, out, filepath.Join("nested", "kept", "shallow.go"))
	for _, skipped := range []string{
		filepath.Join(".git", "hooks", "x.go"),
		filepath.Join("node_modules", "p", "y.go"),
		filepath.Join("vendor", "v", "z.go"),
		filepath.Join(".openclaw", "o.go"),
		filepath.Join("logs", "l.go"),
		filepath.Join("nested", "logs", "deep", "d.go"),
	} {
		requireNoSection(t, out, skipped)
	}
}

// Two different matches, and they are not equivalent: the glob is applied to
// the base name only, while the substring test sees the whole relative path.
// A caller passing a directory name gets the second one.
func TestAnIgnorePatternMatchesEitherTheBaseNameGlobOrTheWholePathAsASubstring(t *testing.T) {
	root := writeTree(t, map[string]string{
		"keep.go":            "package keep\n\nfunc Keep() {}\n",
		"gen_api.go":         "package gen\n\nfunc Gen() {}\n",
		"secret/hidden.go":   "package secret\n\nfunc Hidden() {}\n",
		"secretly/other.go":  "package secretly\n\nfunc Other() {}\n",
		"public/exposed.go":  "package public\n\nfunc Exposed() {}\n",
		"nested/gen_more.go": "package nested\n\nfunc More() {}\n",
	})

	out := runRepoMap(t, root, []string{"gen_*.go", "secret"}, map[string]any{})

	mustSection(t, out, "keep.go")
	mustSection(t, out, filepath.Join("public", "exposed.go"))
	// Glob against the base name, so it reaches a nested file the pattern
	// carries no directory for.
	requireNoSection(t, out, "gen_api.go")
	requireNoSection(t, out, filepath.Join("nested", "gen_more.go"))
	// Substring against the path, so "secret" takes "secretly" with it. That
	// is wider than a caller naming a directory is likely to expect, and it is
	// what the code does.
	requireNoSection(t, out, filepath.Join("secret", "hidden.go"))
	requireNoSection(t, out, filepath.Join("secretly", "other.go"))
}

// The ignore list is consulted for files only. A directory is walked into
// whatever it is called, and its contents drop out one by one via the
// substring test — so an ignored directory costs a full walk of it.
func TestAnIgnoredDirectoryIsStillWalked(t *testing.T) {
	root := writeTree(t, map[string]string{
		"build/out.go": "package build\n\nfunc Out() {}\n",
		"keep.go":      "package keep\n\nfunc Keep() {}\n",
	})

	// A glob that matches the directory's name and nothing inside it: if
	// directories were pruned, out.go would be gone. It is not.
	out := runRepoMap(t, root, []string{"build"}, map[string]any{})
	requireNoSection(t, out, filepath.Join("build", "out.go")) // via the substring branch

	out = runRepoMap(t, root, []string{"buil?"}, map[string]any{})
	mustSection(t, out, filepath.Join("build", "out.go")) // the glob never sees the directory
	mustSection(t, out, "keep.go")
}

func TestOnlyTheKnownTextExtensionsAreListedForNonGoFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"notes.md":    "hello",
		"conf.yaml":   "a: 1",
		"query.sql":   "SELECT 1",
		"script.sh":   "echo hi",
		"logo.png":    "\x89PNG",
		"archive.zip": "PK",
		"Makefile":    "all:",
	})

	out := runRepoMap(t, root, nil, map[string]any{})

	for _, listed := range []string{"notes.md", "conf.yaml", "query.sql", "script.sh"} {
		mustSection(t, out, listed)
	}
	for _, omitted := range []string{"logo.png", "archive.zip", "Makefile"} {
		requireNoSection(t, out, omitted)
	}
}

// The section header carries the path and the body carries the base name, so
// a nested file has to be described by both to tell the two apart.
func TestANonGoFileIsDescribedByItsBaseNameAndByteSize(t *testing.T) {
	root := writeTree(t, map[string]string{
		"notes.md":       "hello",
		"docs/deep/x.md": "hi",
	})
	out := runRepoMap(t, root, nil, map[string]any{})

	if body := mustSection(t, out, "notes.md"); body != "// notes.md (5 bytes)" {
		t.Errorf("want the base name and the byte count, got %q", body)
	}
	if body := mustSection(t, out, filepath.Join("docs", "deep", "x.md")); body != "// x.md (2 bytes)" {
		t.Errorf("want the base name only in the body, got %q", body)
	}
}

// A file holding nothing but its package clause tells the agent nothing, so it
// is dropped rather than printed as an empty section. The threshold is the
// number of parts, and the package line is always one of them.
func TestAFileWithNothingButAPackageClauseIsLeftOut(t *testing.T) {
	root := writeTree(t, map[string]string{
		"empty.go": "package empty\n",
		"full.go":  "package full\n\nfunc F() {}\n",
	})

	out := runRepoMap(t, root, nil, map[string]any{})

	requireNoSection(t, out, "empty.go")
	mustSection(t, out, "full.go")
}

// A file that does not parse is still named. Dropping it would let a syntax
// error hide a file from the agent entirely, which is the one case where the
// agent most needs to be told the file is there.
func TestAFileThatDoesNotParseIsNamedWithItsError(t *testing.T) {
	root := writeTree(t, map[string]string{"broken.go": "package broken\n\nfunc ( {\n"})

	body := mustSection(t, runRepoMap(t, root, nil, map[string]any{}), "broken.go")

	if !strings.HasPrefix(body, "// Parse error:") {
		t.Errorf("want the file named with its parse error, got %q", body)
	}
}

// ---- what a Go file is reduced to ----

const sampleGo = `package sample

import (
	"fmt"
	"encoding/json"
	"github.com/kayushkin/llm-bridge-server/internal/agents/tools/schema"
)

type Small struct {
	Alpha string
	Beta  int
	priv  bool
}

type Wide struct {
	A, B, C, D string
	hidden     int
}

type Empty struct{}

type Reader interface {
	Read() error
	Close() error
}

type Alias = string

func Plain(a string, b int) error { return nil }

func Grouped(a, b string) (int, error) { return 0, nil }

func (s *Small) Method(v json.RawMessage) *Wide { return nil }

func none() {}

var _ = fmt.Sprint
var _ = schema.Str
`

func sampleParts(t *testing.T) []string {
	t.Helper()
	root := writeTree(t, map[string]string{"sample.go": sampleGo})
	body := mustSection(t, runRepoMap(t, root, nil, map[string]any{}), "sample.go")
	return strings.Split(body, "\n")
}

func requirePart(t *testing.T, parts []string, want string) {
	t.Helper()
	for _, p := range parts {
		if p == want {
			return
		}
	}
	t.Errorf("want a line %q, got:\n%s", want, strings.Join(parts, "\n"))
}

func TestThePackageClauseLeadsTheSummary(t *testing.T) {
	parts := sampleParts(t)

	if parts[0] != "pkg sample" {
		t.Errorf("want the package first, got %q", parts[0])
	}
}

// Only imports whose path contains a dot are kept, which is a test for a
// hostname rather than for the standard library. It keeps "gopkg.in/yaml.v3"
// and drops "encoding/json" — and would drop a third-party package hosted
// under a dotless path.
func TestOnlyDottedImportPathsAreKept(t *testing.T) {
	parts := sampleParts(t)

	var imports string
	for _, p := range parts {
		if strings.HasPrefix(p, "imports: ") {
			imports = p
		}
	}
	if imports == "" {
		t.Fatalf("the summary lists no imports:\n%s", strings.Join(parts, "\n"))
	}
	if !strings.Contains(imports, "github.com/kayushkin/llm-bridge-server/internal/agents/tools/schema") {
		t.Errorf("the third-party import was dropped: %q", imports)
	}
	for _, dropped := range []string{"encoding/json", `"fmt"`} {
		if strings.Contains(imports, dropped) {
			t.Errorf("%q should not be listed as an import: %q", dropped, imports)
		}
	}
}

func TestAFunctionIsReducedToItsNameAndTypes(t *testing.T) {
	parts := sampleParts(t)

	requirePart(t, parts, "Plain(string, int) error")
}

// Names grouped onto one type still count once each, so the arity survives the
// reduction. Dropping the repeat would print Grouped(string) for a
// two-argument function.
func TestGroupedParametersAreExpandedToOnePerName(t *testing.T) {
	parts := sampleParts(t)

	requirePart(t, parts, "Grouped(string, string) (int, error)")
}

func TestAMethodIsPrefixedByItsReceiverType(t *testing.T) {
	parts := sampleParts(t)

	requirePart(t, parts, "*Small.Method(json.RawMessage) *Wide")
}

// Unexported functions are summarized too — the filter this mechanism applies
// is on struct FIELDS, not on declarations.
func TestUnexportedFunctionsAreSummarizedAsWell(t *testing.T) {
	parts := sampleParts(t)

	requirePart(t, parts, "none()")
}

// Three or fewer exported fields are spelled out; above that the summary
// collapses to a count. ⚠️ The count is of ALL fields, exported or not, so
// `Wide` reports 5 while naming 4 — the two numbers in this mechanism are
// taken from different sets.
func TestASmallStructIsSpelledOutAndAWideOneIsCounted(t *testing.T) {
	parts := sampleParts(t)

	requirePart(t, parts, "type Small struct{Alpha string; Beta int}")
	requirePart(t, parts, "type Wide struct{5 fields}")
	requirePart(t, parts, "type Empty struct{}")
}

func TestAnInterfaceIsSummarizedByItsMethodNames(t *testing.T) {
	parts := sampleParts(t)

	requirePart(t, parts, "type Reader interface{Read, Close}")
}

// Everything that is neither a struct nor an interface is printed with an "="
// whether or not it was declared as an alias, so `type Celsius float64` and
// `type Celsius = float64` are indistinguishable in the map.
func TestANamedTypeIsPrintedAsAnAliasWhetherOrNotItIsOne(t *testing.T) {
	root := writeTree(t, map[string]string{
		"kinds.go": "package kinds\n\ntype Alias = string\n\ntype Defined float64\n\nfunc F() {}\n",
	})
	body := mustSection(t, runRepoMap(t, root, nil, map[string]any{}), "kinds.go")
	parts := strings.Split(body, "\n")

	requirePart(t, parts, "type Alias = string")
	requirePart(t, parts, "type Defined = float64")
}

// Types are emitted after every function, not in source order, so the map's
// layout is fixed regardless of how the file was written.
func TestTypesFollowFunctionsWhateverTheSourceOrder(t *testing.T) {
	root := writeTree(t, map[string]string{
		"order.go": "package order\n\ntype First struct{}\n\nfunc Second() {}\n",
	})
	body := mustSection(t, runRepoMap(t, root, nil, map[string]any{}), "order.go")

	fn := strings.Index(body, "Second()")
	ty := strings.Index(body, "type First")
	if fn < 0 || ty < 0 {
		t.Fatalf("summary is missing a declaration:\n%s", body)
	}
	if fn > ty {
		t.Errorf("types were printed before functions:\n%s", body)
	}
}

// ---- how a type expression is spelled ----

func TestEveryTypeExpressionShapeHasItsOwnSpelling(t *testing.T) {
	root := writeTree(t, map[string]string{
		"shapes.go": `package shapes

import "time"

func Star(a *int) {}
func Slice(a []string) {}
func Map(a map[string]int) {}
func Variadic(a ...string) {}
func Chan(a chan int) {}
func Func(a func(int) error) {}
func Anon(a interface{}) {}
func Struct(a struct{ X int }) {}
func Stdlib(a time.Duration) {}
func Foreign(a schema.Thing) {}
func Nested(a map[string][]*time.Time) {}
`,
	})
	body := mustSection(t, runRepoMap(t, root, nil, map[string]any{}), "shapes.go")
	parts := strings.Split(body, "\n")

	requirePart(t, parts, "Star(*int)")
	requirePart(t, parts, "Slice([]string)")
	requirePart(t, parts, "Map(map[string]int)")
	requirePart(t, parts, "Variadic(...string)")
	// A channel loses its direction and its element type.
	requirePart(t, parts, "Chan(chan)")
	// A function parameter loses its whole signature.
	requirePart(t, parts, "Func(func)")
	requirePart(t, parts, "Anon(interface{})")
	// An inline struct loses its fields.
	requirePart(t, parts, "Struct(struct)")
	requirePart(t, parts, "Nested(map[string][]*Time)")
}

// The qualifier is dropped for a package on a hardcoded list and kept for
// everything else. ⚠️ The list holds identifiers, not import paths, and it is
// partial: "time" is on it, "filepath" and "sort" are not, so time.Duration
// prints bare while filepath.WalkFunc keeps its package. Two types with one
// name in two stdlib packages are then indistinguishable.
func TestAQualifierIsDroppedOnlyForThePackagesOnTheStdlibList(t *testing.T) {
	root := writeTree(t, map[string]string{
		"quals.go": `package quals

import (
	"encoding/json"
	"path/filepath"
	"time"
)

func Listed(a time.Duration) {}
func Unlisted(a filepath.WalkFunc) {}
func Encoded(a json.RawMessage) {}
func Foreign(a schema.InputSchema) {}
`,
	})
	body := mustSection(t, runRepoMap(t, root, nil, map[string]any{}), "quals.go")
	parts := strings.Split(body, "\n")

	requirePart(t, parts, "Listed(Duration)")
	requirePart(t, parts, "Unlisted(filepath.WalkFunc)")
	requirePart(t, parts, "Foreign(schema.InputSchema)")
	// ⚠️ The list carries "encoding", and no import ever produces "encoding"
	// as a qualifier — the identifier for encoding/json is "json". So that
	// entry can never match, and json.RawMessage is spelled like a
	// third-party type. Four of the twelve entries are unreachable this way.
	requirePart(t, parts, "Encoded(json.RawMessage)")
}

// A type expression the reducer has no case for prints as "?" rather than
// being dropped, so the parameter position survives even when its type does
// not. Generics land here: an instantiated type is an IndexExpr.
func TestATypeTheReducerCannotSpellPrintsAsAQuestionMark(t *testing.T) {
	root := writeTree(t, map[string]string{
		"generic.go": "package generic\n\ntype Box[T any] struct{ V T }\n\nfunc Take(b Box[int]) {}\n",
	})
	body := mustSection(t, runRepoMap(t, root, nil, map[string]any{}), "generic.go")
	parts := strings.Split(body, "\n")

	requirePart(t, parts, "Take(?)")
}

// ---- the format parameter ----

// "full" is documented as complete signatures and is wired to the same parser
// as "compact"; parseGoFileFull is a one-line forward. A caller asking for
// more detail gets the compact form and nothing says so. Pinned so that
// implementing "full" has to come past this test rather than silently
// changing what an existing caller receives.
func TestTheFullFormatIsCurrentlyIdenticalToCompact(t *testing.T) {
	root := writeTree(t, map[string]string{"sample.go": sampleGo})

	compact := runRepoMap(t, root, nil, map[string]any{"format": "compact"})
	full := runRepoMap(t, root, nil, map[string]any{"format": "full"})
	defaulted := runRepoMap(t, root, nil, map[string]any{})

	if compact != defaulted {
		t.Errorf("the default format is not compact:\n--- default ---\n%s\n--- compact ---\n%s", defaulted, compact)
	}
	if full != compact {
		t.Errorf("full and compact have diverged; this test is the place to say so:\n--- full ---\n%s\n--- compact ---\n%s", full, compact)
	}
}

// Anything that is not the literal "compact" takes the full branch, including
// a misspelling. Since the two branches agree today the caller cannot tell,
// which is exactly what makes it worth pinning before they diverge.
func TestAnUnknownFormatTakesTheFullBranchRatherThanFailing(t *testing.T) {
	root := writeTree(t, map[string]string{"sample.go": sampleGo})

	out, err := RepoMap(root, nil).Run(context.Background(), `{"format":"copmact"}`)
	if err != nil {
		t.Fatalf("an unknown format was rejected: %v", err)
	}
	if out != runRepoMap(t, root, nil, map[string]any{"format": "full"}) {
		t.Error("an unknown format took neither branch")
	}
}

// ---- the tool's own declaration ----

func TestTheToolDeclaresBothItsOptionalParameters(t *testing.T) {
	impl := RepoMap(t.TempDir(), nil)

	if impl.Name != "repo_map" {
		t.Errorf("tool name is %q", impl.Name)
	}
	raw, err := json.Marshal(impl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"path", "format"} {
		if !strings.Contains(string(raw), `"`+param+`"`) {
			t.Errorf("the schema does not declare %q: %s", param, raw)
		}
	}
	// Neither is required — an empty request maps the whole repo.
	if strings.Contains(string(raw), `"required":["`) {
		t.Errorf("repo_map declares a required parameter: %s", raw)
	}
}
