#!/usr/bin/env python3
"""Score tool-store's tests for the two codebase-survey tools by breaking them.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-repo-map.py [--diffs] [--crosstable]

⚠️ **This branch's engine is md5 `addb06c7867cc34eb6cb79c504350a55` (946 lines),
and no other repo on this box carries it yet.** Take the blob off the BRANCH,
not out of a working tree — those checkouts sit on whatever branch their last
pass left them on, and md5summing them answers about the wrong commit (221st).

⛔ **The two md5s this header used to name are both gone, and reading it as
written would send you to the wrong engine.** It said this file rode the
canonical `9a81a32e` (534 lines) shared with five other repos, and that
tool-store's own `2932bd4d` (406 lines) sat on two unmerged branches and "on
neither `main` nor any other branch". By the time this card was worked,
`2932bd4d` had reached `main` — `merge/the-walk-cap-test-meets-the-walk-cap-fix`
merged `test/truncation-budgets-are-pinned-to-their-values` into the trunk
lineage and carried the older engine in with it. **A sentence saying where a
blob is NOT is a cached query, and it goes stale in the direction that reads as
settled** (99th, 165th).

**Neither side was simply stale, which is why this is a union and not a
choice.** `2932bd4d` alone carried the 42nd pass's guard classification —
`classify_caught`, `counts_as_coverage`, `failure_messages` and
`Case.expected_guard_caught`, the machinery that separates an assertion firing
from a reach guard falling over — and lacked the 218th's needle-once rule.
`9a81a32e` carried the needle-once rule, the multi-target shape, the cross-table
and the stale-declaration check, and had never heard of a reach guard. The
engine on this branch is both, landed by the 240th pass. The other five repos
still carry `9a81a32e`, scheduler carries `9d5ad6a0` and bundle-store
`3a7741d9`; propagating the union to them is a decision and is NOT done here.

⚠️ **Scores taken on this repo under `2932bd4d` still want re-taking.** It
predates the needle-occurrence guard, so every case scored by
`sabotage-set-enabled.py` and by the truncation plans was scored by an engine
that silently sabotages the FIRST of two matching occurrences. Filed, not done
here.

Why this seam is worth a scorer: measured on `main` — not on the branch this
file sits on, per the 223rd — a `panic()` on the first line of any of these left
`go test ./...` green:

    repo_map.go      RepoMap  buildRepoMap  parseGoFileCompact  parseGoFileFull
                     compactFuncSignature  compactTypeDecl  compactType
                     isExported  isStdlib  isRelevantFile  shouldIgnore
    recent_files.go  RecentFiles  formatRecentFiles  parseDuration

Fourteen functions across two files, executed by nothing. The census row named
two of them.

⚠️ **The three finders are the instructive exception and they are NOT in that
list.** `findRecentlyModified`, `findRecentlyModifiedGit` and
`findRecentlyModifiedMtime` all redden the suite under a `panic()`, because
`cancellation_test.go` calls the first one — and that test asserts a canceled
call stops, never which files an uncanceled one returns. The reach guard reads
them as covered while nothing checks what they answer. Cases below sabotage all
three, and before this branch every one of them scored UNNOTICED.

📄 What these two tools are for is why the gaps matter past a coverage row.
Both are survey tools: an agent calls them to decide what a codebase contains
and what has been worked on, then acts on the answer without reading the files
to check it. A wrong line here is not a wrong render — it is a premise.

Case-writing rules inherited from the scheduler, bundle-store, agent-store,
seed-profile and install plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - A control is a case and obeys the case rules (220th).
  - Read every applied diff against its label (`--diffs`). A row prints the
    name you gave it, not the edit you made.
  - An UNNOTICED row is a claim about which LINE the engine moved before it is
    a claim about the tests (218th), and the case NAME is a claim the run can
    contradict too (221st).
  - Before reading UNNOTICED as a coverage hole, check whether the mutation is
    observable at all (221st).
  - Ask what would have to differ for a green row to flip (223rd).

⚠️ Needles here are anchored to their enclosing function wherever the fragment
repeats. `ModTime:      info.ModTime(),` appears in BOTH finders in
recent_files.go, and `if info.IsDir() {` plus the `baseName == ".git"` skip
appear in both files — the engine aborts on an ambiguous needle rather than
picking one, which is the 218th's fix doing its job, and the anchoring below is
what the abort asks for.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered. The other tools in the package (shell, fs, grep, browser,
web_fetch, web_search, scheduler, scratchpad, task_plan, end_turn) are not
sabotaged here and have their own suites or none. `childprocess` is used as a
fixture by the git finder and is not scored.

⚠️ `--crosstable` has NOT been taken for this repo and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The 222nd and 223rd passes left that
open and this pass does not close it either.

Score when filed by the 224th pass: 66/67 real mechanisms caught, both controls
behaved, exit 0. The 67th is the declared empty-name guard below, which no test
can separate because the mutation is unobservable.

Two rounds were needed and neither round was a verdict on the suite. The first
scored 60/66 with FOUR compile errors — cases that never ran, not weaker
UNNOTICEDs (223rd) — each one a deletion that orphaned a variable or an import;
they are rewritten above as drifted values or paired edits. Of the two real
UNNOTICEDs that round, one was a mutation nothing could observe (declared) and
one was a genuine hole in the suite (closed). The second round left one
UNNOTICED whose cause was the FIXTURE, not the tests: filepath.Walk already
yields lexical order, so deleting the sort changes nothing unless a file and a
directory share a prefix — "a.go" against "a/b.go", where the walk gives
[a/b.go a.go] and the sort gives [a.go a/b.go]. Measured with a throwaway
program rather than argued. That case now has a fixture that can see it.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "tools" / "repo_map.go", REPO / "tools" / "recent_files.go"]
PACKAGES = ["./tools"]

CASES = [
    # ================= repo_map: the document =================
    Case(
        "the map loses its heading, so the answer no longer says what it is",
        [('builder.WriteString("# Repository Structure\\n\\n")',
          'builder.WriteString("# Files\\n\\n")')],
    ),
    Case(
        # Two edits, because deleting the only sort.Slice call orphans the
        # "sort" import and a compile error is the case not having run (223rd).
        "sections stop being sorted, so the map comes back in walk order",
        [("\tsort.Slice(entries, func(i, j int) bool {\n\t\treturn entries[i].FilePath < entries[j].FilePath\n\t})\n",
          ""),
         ('\t"sort"\n', "")],
    ),
    Case(
        "the sort is reversed, so the map reads from the end of the tree",
        [("\t\treturn entries[i].FilePath < entries[j].FilePath",
          "\t\treturn entries[i].FilePath > entries[j].FilePath")],
    ),
    Case(
        "a section is headed by its base name, so two files of one name collide in the map",
        [('builder.WriteString(fmt.Sprintf("## %s\\n", e.FilePath))',
          'builder.WriteString(fmt.Sprintf("## %s\\n", filepath.Base(e.FilePath)))')],
    ),

    # ================= repo_map: what is left out =================
    Case(
        "the heavy directories are descended into rather than skipped",
        [("\t\t\t\treturn filepath.SkipDir\n\t\t\t}\n\t\t\treturn nil\n\t\t}\n\n\t\t// Get relative path",
          "\t\t\t\treturn nil\n\t\t\t}\n\t\t\treturn nil\n\t\t}\n\n\t\t// Get relative path")],
    ),
    Case(
        "logs/ stops being skipped, so a build's output is mapped as source",
        [('\t\t\t\tbaseName == "vendor" || baseName == ".openclaw" ||\n\t\t\t\tbaseName == "logs" {',
          '\t\t\t\tbaseName == "vendor" || baseName == ".openclaw" ||\n\t\t\t\tbaseName == "loggs" {')],
    ),
    Case(
        "the ignore list is never consulted, so every excluded file is mapped anyway",
        [("\t\tif shouldIgnore(relPath, ignorePatterns) {\n\t\t\treturn nil\n\t\t}\n",
          "")],
    ),
    Case(
        "the ignore glob is matched against the whole path, so it stops reaching nested files",
        [("\t\tmatched, _ := filepath.Match(pattern, filepath.Base(path))",
          "\t\tmatched, _ := filepath.Match(pattern, path)")],
    ),
    Case(
        # Rewritten as one edit over both lines: dropping the `if` alone
        # orphans `matched`, and a compile error is not a weaker UNNOTICED.
        "the ignore list drops its glob branch, so a pattern with a wildcard matches nothing",
        [("\t\tmatched, _ := filepath.Match(pattern, filepath.Base(path))\n\t\tif matched {\n\t\t\treturn true\n\t\t}\n",
          "\t\t_, _ = filepath.Match(pattern, filepath.Base(path))\n")],
    ),
    Case(
        "the ignore list drops its substring branch, so naming a directory stops excluding it",
        [("\t\tif strings.Contains(path, pattern) {\n\t\t\treturn true\n\t\t}\n",
          "")],
    ),
    Case(
        "every non-Go file is judged relevant, so binaries are listed as project files",
        [("func isRelevantFile(path string) bool {\n\text := strings.ToLower(filepath.Ext(path))",
          "func isRelevantFile(path string) bool {\n\treturn true\n\text := strings.ToLower(filepath.Ext(path))")],
    ),
    Case(
        "the relevant-extension list loses .sql, so schema files vanish from the map",
        [('\t\t".sh", ".py", ".js", ".ts", ".sql", ".proto",',
          '\t\t".sh", ".py", ".js", ".ts", ".proto",')],
    ),
    Case(
        "a non-Go file loses its byte count, so size stops being part of the answer",
        [('\t\t\t\tContent:  fmt.Sprintf("// %s (%d bytes)", filepath.Base(path), info.Size()),',
          '\t\t\t\tContent:  fmt.Sprintf("// %s", filepath.Base(path)),')],
    ),
    Case(
        "a non-Go file is described by its whole path rather than its base name",
        [('fmt.Sprintf("// %s (%d bytes)", filepath.Base(path), info.Size())',
          'fmt.Sprintf("// %s (%d bytes)", relPath, info.Size())')],
    ),
    Case(
        "a file that does not parse is dropped instead of named, so a syntax error hides it entirely",
        [("\t\t\t\tentries = append(entries, entry{\n\t\t\t\t\tFilePath: relPath,\n\t\t\t\t\tContent:  fmt.Sprintf(\"// Parse error: %v\", err),\n\t\t\t\t\tIsGoFile: true,\n\t\t\t\t})\n\t\t\t\treturn nil",
          "\t\t\t\treturn nil")],
    ),
    Case(
        "an empty summary is emitted anyway, so a package-only file becomes a blank section",
        [("\t\t\tif summary != \"\" {", "\t\t\tif true {")],
    ),
    Case(
        "the package-only threshold moves, so a file with nothing but its package clause is mapped",
        [("\tif len(parts) <= 1 {\n\t\treturn \"\", nil // Empty or package-only file\n\t}",
          "\tif len(parts) < 1 {\n\t\treturn \"\", nil // Empty or package-only file\n\t}")],
    ),

    # ================= repo_map: the Go summary =================
    Case(
        "the package clause is left out, so nothing in the map says what package a file is in",
        [('\t\tparts = append(parts, fmt.Sprintf("pkg %s", node.Name.Name))',
          '\t\tparts = append(parts, "")')],
    ),
    Case(
        "the import filter is inverted, so the standard library is listed and the real dependencies are not",
        [('\t\tif strings.Contains(path, ".") {', '\t\tif !strings.Contains(path, ".") {')],
    ),
    Case(
        "a method loses its receiver, so it reads as a package-level function",
        [("\tif decl.Recv != nil && len(decl.Recv.List) > 0 {", "\tif false {")],
    ),
    Case(
        "grouped parameters collapse to one, so a function's arity is understated",
        [("\t\t\tcount := len(param.Names)", "\t\t\tcount := 1")],
    ),
    Case(
        "parameter types are dropped, so every signature reads as taking nothing",
        [("\tif decl.Type.Params != nil {", "\tif false {")],
    ),
    Case(
        "a single return value is parenthesised like a tuple",
        [("\t\tif len(results) == 1 {", "\t\tif len(results) == 0 {")],
    ),
    Case(
        "return types are dropped, so a function that errors is indistinguishable from one that cannot",
        [("\tif decl.Type.Results != nil && len(decl.Type.Results.List) > 0 {", "\tif false {")],
    ),
    Case(
        "the struct-detail threshold drops, so a small struct is summarized as a count",
        [("\t\tif len(exported) > 0 && len(exported) <= 3 {",
          "\t\tif len(exported) > 0 && len(exported) <= 1 {")],
    ),
    Case(
        # The two numbers come from different sets: the names are the exported
        # ones, the count is every field. A test that reads only the names
        # cannot see this.
        "the collapsed struct count reports only its exported fields",
        [('\t\treturn fmt.Sprintf("type %s struct{%d fields}", typeName, totalFields)',
          '\t\treturn fmt.Sprintf("type %s struct{%d fields}", typeName, len(exported))')],
    ),
    Case(
        "the exported test is inverted, so a struct is summarized by its private fields",
        [("\t\t\t\t\tif isExported(name.Name) {", "\t\t\t\t\tif !isExported(name.Name) {")],
    ),
    Case(
        "the exported test widens to the whole alphabet, so unexported fields are advertised as API",
        [("\treturn name[0] >= 'A' && name[0] <= 'Z'",
          "\treturn name[0] >= 'A' && name[0] <= 'z'")],
    ),
    # Dominated by construction, so declared rather than scored (222nd). The
    # names reaching isExported come from parsed *ast.Ident nodes, and a parsed
    # identifier is never the empty string — so the guard's branch cannot be
    # taken and no test can separate its two answers. This is the FIRST version
    # of the case above; it scored UNNOTICED and the reason was the mutation,
    # not the suite (221st).
    Case(
        "the empty-name guard answers the opposite way",
        [("func isExported(name string) bool {\n\tif name == \"\" {\n\t\treturn false\n\t}",
          "func isExported(name string) bool {\n\tif name == \"\" {\n\t\treturn true\n\t}")],
        expected_unnoticed="isExported is only ever called with a parsed identifier, which is never empty, so the guard is unreachable and its two answers are indistinguishable",
    ),
    Case(
        "the interface-detail threshold drops, so a two-method interface is summarized as a count",
        [("\t\tif len(methods) <= 5 {", "\t\tif len(methods) <= 1 {")],
    ),
    Case(
        "an interface's methods are spelled by type rather than by name",
        [("\t\t\tif len(method.Names) > 0 {\n\t\t\t\tmethods = append(methods, method.Names[0].Name)",
          "\t\t\tif false {\n\t\t\t\tmethods = append(methods, method.Names[0].Name)")],
    ),
    Case(
        "a named type stops being printed with an equals sign, so its spelling changes under callers",
        [('\t\treturn fmt.Sprintf("type %s = %s", typeName, typeStr)',
          '\t\treturn fmt.Sprintf("type %s %s", typeName, typeStr)')],
    ),

    # ================= repo_map: type spelling =================
    Case(
        "a pointer loses its star, so a pointer and a value read alike",
        [('\t\treturn "*" + compactType(t.X)', "\t\treturn compactType(t.X)")],
    ),
    Case(
        "a slice loses its brackets, so a slice and its element read alike",
        [('\t\treturn "[]" + compactType(t.Elt)', "\t\treturn compactType(t.Elt)")],
    ),
    Case(
        "a map is spelled with its key and value the wrong way round",
        [('\t\treturn fmt.Sprintf("map[%s]%s", compactType(t.Key), compactType(t.Value))',
          '\t\treturn fmt.Sprintf("map[%s]%s", compactType(t.Value), compactType(t.Key))')],
    ),
    Case(
        "a variadic parameter loses its ellipsis, so it reads as a plain slice",
        [('\t\treturn "..." + compactType(t.Elt)', "\t\treturn compactType(t.Elt)")],
    ),
    Case(
        "a channel is spelled as an unknown type",
        [('\tcase *ast.ChanType:\n\t\treturn "chan"', '\tcase *ast.ChanType:\n\t\treturn "?"')],
    ),
    Case(
        "a function parameter is spelled as an unknown type",
        [('\tcase *ast.FuncType:\n\t\treturn "func"', '\tcase *ast.FuncType:\n\t\treturn "?"')],
    ),
    Case(
        "an inline struct is spelled as an unknown type",
        [('\tcase *ast.StructType:\n\t\treturn "struct"', '\tcase *ast.StructType:\n\t\treturn "?"')],
    ),
    Case(
        "an unrecognized type is spelled as nothing, so the parameter disappears rather than being marked unknown",
        [('\tdefault:\n\t\treturn "?"\n\t}\n}\n\nfunc isExported', '\tdefault:\n\t\treturn ""\n\t}\n}\n\nfunc isExported')],
    ),
    Case(
        "every package qualifier is treated as standard library, so third-party types lose their package",
        [("func isStdlib(pkg string) bool {\n\tstdlibs := []string{",
          "func isStdlib(pkg string) bool {\n\treturn true\n\tstdlibs := []string{")],
    ),
    Case(
        "no package qualifier is treated as standard library, so stdlib types keep theirs",
        [("func isStdlib(pkg string) bool {\n\tstdlibs := []string{",
          "func isStdlib(pkg string) bool {\n\treturn false\n\tstdlibs := []string{")],
    ),
    Case(
        "the stdlib list loses time, so a duration is spelled like a third-party type",
        [('\t\t"context", "fmt", "os", "io", "http", "time", "sync",',
          '\t\t"context", "fmt", "os", "io", "http", "sync",')],
    ),

    # ================= recent_files: the window =================
    Case(
        "the default window shrinks, so a day's work is reported as an hour's",
        [('\t\t\t\tparams.Since = "24h"', '\t\t\t\tparams.Since = "1h"')],
    ),
    Case(
        "the empty-result sentence stops naming the window it searched",
        [('\t\t\t\treturn fmt.Sprintf("No files modified in the last %s.", params.Since), nil',
          '\t\t\t\treturn "No files modified recently.", nil')],
    ),
    Case(
        "the days multiplier drops, so a window given in days is read as hours",
        [("\t\treturn time.Duration(n) * 24 * time.Hour, nil",
          "\t\treturn time.Duration(n) * time.Hour, nil")],
    ),
    Case(
        "the days suffix stops being handled, so a window given in days is rejected",
        [('\tif strings.HasSuffix(s, "d") {', "\tif false {")],
    ),
    Case(
        "an unparseable window stops being quoted back, so the caller is not told what was rejected",
        [('\t\t\t\treturn "", fmt.Errorf("invalid duration \'%s\': %w", params.Since, err)',
          '\t\t\t\treturn "", fmt.Errorf("invalid duration: %w", err)')],
    ),

    # ================= recent_files: which finder answers =================
    Case(
        "git is never preferred, so a repo's listing is taken from mtimes and uncommitted noise",
        [("\tif err == nil && len(gitFiles) > 0 {\n\t\treturn gitFiles, nil\n\t}",
          "\tif err == nil && false {\n\t\treturn gitFiles, nil\n\t}")],
    ),
    Case(
        "an empty git answer is returned as the result, so a repo with no recent commits reports nothing",
        [("\tif err == nil && len(gitFiles) > 0 {", "\tif err == nil && len(gitFiles) >= 0 {")],
    ),
    Case(
        # A drifted value rather than a deletion: replacing sinceArg at the call
        # site orphans it and both lines that build it.
        "the git window is widened to ten years, so every file ever committed is reported as recent",
        [("\tsinceTime := time.Now().Add(-since)",
          "\tsinceTime := time.Now().Add(-87600 * time.Hour)")],
    ),
    Case(
        "the mtime cutoff is inverted, so the tool reports everything it has NOT touched",
        [("\t\tif info.ModTime().After(cutoff) {", "\t\tif info.ModTime().Before(cutoff) {")],
    ),
    Case(
        "the mtime walk stops skipping .git, so a repo's object store is reported as recent work",
        [('\t\t\tif baseName == ".git" || baseName == "node_modules" || baseName == "vendor" {',
          '\t\t\tif baseName == "node_modules" || baseName == "vendor" {')],
    ),
    Case(
        "the mtime walk stamps rows with the time of the scan rather than the file",
        [("\t\t\tresults = append(results, recentFile{\n\t\t\t\tPath:         path,\n\t\t\t\tRelativePath: relPath,\n\t\t\t\tModTime:      info.ModTime(),",
          "\t\t\tresults = append(results, recentFile{\n\t\t\t\tPath:         path,\n\t\t\t\tRelativePath: relPath,\n\t\t\t\tModTime:      time.Now(),")],
    ),
    Case(
        "a git-sourced row is stamped with the time of the scan rather than the file",
        [("\t\tresults = append(results, recentFile{\n\t\t\tPath:         fullPath,\n\t\t\tRelativePath: line,\n\t\t\tModTime:      info.ModTime(),",
          "\t\tresults = append(results, recentFile{\n\t\t\tPath:         fullPath,\n\t\t\tRelativePath: line,\n\t\t\tModTime:      time.Now(),")],
    ),

    # ================= recent_files: the rendering =================
    Case(
        "the listing's total counts something other than the rows under it",
        [('\tbuilder.WriteString(fmt.Sprintf("# Recently Modified Files (%d total)\\n\\n", len(files)))',
          '\tbuilder.WriteString(fmt.Sprintf("# Recently Modified Files (%d total)\\n\\n", len(files)+1))')],
    ),
    Case(
        "the listing is numbered from zero",
        [('\t\tbuilder.WriteString(fmt.Sprintf("%d. **%s** (%d lines, %s)\\n", i+1, file.RelativePath, lineCount, timeStr))',
          '\t\tbuilder.WriteString(fmt.Sprintf("%d. **%s** (%d lines, %s)\\n", i, file.RelativePath, lineCount, timeStr))')],
    ),
    Case(
        "the line count loses its final line",
        [('\t\t\tlineCount = strings.Count(string(content), "\\n") + 1',
          '\t\t\tlineCount = strings.Count(string(content), "\\n")')],
    ),
    Case(
        "a file that cannot be read is reported with a line count rather than zero",
        [("\t\tlineCount := 0\n\t\tif err == nil {", "\t\tlineCount := 1\n\t\tif err == nil {")],
    ),
    Case(
        "the just-now bucket narrows, so a file touched seconds ago is reported in minutes",
        [("\t\tif timeSince < time.Minute {", "\t\tif timeSince < time.Second {")],
    ),
    Case(
        "the minutes bucket narrows, so most of an hour is reported in hours",
        [("\t\t} else if timeSince < time.Hour {", "\t\t} else if timeSince < 30*time.Minute {")],
    ),
    Case(
        "the hours bucket narrows, so most of a day is reported in days",
        [("\t\t} else if timeSince < 24*time.Hour {", "\t\t} else if timeSince < 12*time.Hour {")],
    ),
    Case(
        "the day count is computed against the wrong number of hours",
        [('\t\t\ttimeStr = fmt.Sprintf("%dd ago", int(timeSince.Hours()/24))',
          '\t\t\ttimeStr = fmt.Sprintf("%dd ago", int(timeSince.Hours()/12))')],
    ),
    Case(
        "content is never included, so asking for it changes nothing",
        [("\t\tif includeContent && err == nil {", "\t\tif false && err == nil {")],
    ),
    Case(
        "content is always included, so a metadata request returns every file's body",
        [("\t\tif includeContent && err == nil {", "\t\tif err == nil {")],
    ),
    Case(
        "the request's content switch never reaches the formatter",
        [("\t\t\treturn formatRecentFiles(files, params.IncludeContent)",
          "\t\t\treturn formatRecentFiles(files, false)")],
    ),
    Case(
        "the content fence is dropped, so a file body runs into the listing around it",
        [('\t\t\tbuilder.WriteString("```\\n")\n\t\t\tbuilder.WriteString(string(content))',
          "\t\t\tbuilder.WriteString(string(content))")],
    ),

    # ================= controls =================
    Case(
        "CONTROL known-positive: every mapped path is reported one directory deeper than it is",
        [("\t\trelPath, _ := filepath.Rel(rootDir, path)\n\t\tif shouldIgnore(relPath, ignorePatterns) {",
          "\t\trelPath, _ := filepath.Rel(rootDir, path)\n\t\trelPath = filepath.Join(\"nested\", relPath)\n\t\tif shouldIgnore(relPath, ignorePatterns) {")],
    ),
    # Known-negative: the Source field records which finder produced a row and
    # is never rendered. Nothing downstream reads it, so nothing can see it
    # change — the field is a comment with a type.
    Case(
        "CONTROL known-negative: a git-sourced row is labeled as having come from the mtime walk",
        [('\t\t\tSource:       "git",', '\t\t\tSource:       "mtime",')],
        expected_unnoticed="Source is set by both finders and read by nothing — formatRecentFiles never renders it",
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
