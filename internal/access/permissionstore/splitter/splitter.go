// Package splitter parses a Bash command line into a flat list of atoms — one
// per discrete command invocation or write redirection — so a permission rule
// engine can evaluate each piece independently.
//
// The splitter is a security boundary. Anything it fails to extract becomes
// invisible to the rule engine, so the design errs toward over-reporting:
//
//   - Every CallExpr in the AST emits one Call atom (including those nested
//     inside command substitutions, process substitutions, control flow,
//     subshells, and function bodies).
//   - Every write-side redirection emits one Redirect atom with the target
//     path (best-effort flattened).
//   - bash/sh/zsh/ksh/dash/ash with -c "<string>" recursively splits the
//     string. eval <args> joins and recursively splits.
//   - Parse failures collapse to a single Unparseable atom, which the rule
//     engine treats as ask (never silently allow).
//
// Word flattening replaces non-literal expansions (variables, command
// substitutions, arithmetic, process substitutions) with placeholders so a
// rule like ^rm\b can't be evaded by quoting the literal command — but the
// real rm inside a $() will still appear as its own Call atom regardless.
package splitter

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// AtomKind identifies the shape of an atom emitted by Split.
type AtomKind string

const (
	// AtomCall is a single command invocation: argv[0] is the command,
	// argv[1:] are arguments. Embedded expansions are flattened to
	// placeholder text ("$VAR", "$(...)", etc.).
	AtomCall AtomKind = "call"

	// AtomRedirect is a write-side redirection target: > >> >| &> &>>.
	// Read redirections (<, <<, <<<) are not emitted — they don't mutate.
	AtomRedirect AtomKind = "redirect"

	// AtomUnparseable is the catch-all for parser failures. Rule engines
	// should treat this as "ask" — never silently allow.
	AtomUnparseable AtomKind = "unparseable"
)

// Atom is one discrete piece of a Bash command line. Multiple atoms can
// originate from a single Split call when the input chains, pipes, redirects,
// or substitutes commands.
type Atom struct {
	Kind AtomKind `json:"kind"`

	// Argv and Line populate AtomCall.
	Argv []string `json:"argv,omitempty"`
	Line string   `json:"line,omitempty"`

	// Op and Target populate AtomRedirect.
	Op     string `json:"op,omitempty"`
	Target string `json:"target,omitempty"`

	// Original and Reason populate AtomUnparseable.
	Original string `json:"original,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Split parses a Bash command and returns its atoms. Order in the returned
// slice mirrors AST traversal order (roughly source order, with nested
// commands appearing after their containing call). Callers must allow only if
// every atom is allowed under the rule set.
func Split(command string) []Atom {
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return []Atom{{
			Kind:     AtomUnparseable,
			Original: command,
			Reason:   err.Error(),
		}}
	}
	var atoms []Atom
	syntax.Walk(file, func(n syntax.Node) bool {
		stmt, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		if call, ok := stmt.Cmd.(*syntax.CallExpr); ok {
			extractCall(call, &atoms)
		}
		for _, r := range stmt.Redirs {
			extractRedirect(r, &atoms)
		}
		// Returning true lets Walk descend into the statement's children
		// — that's how nested commands inside CmdSubst, ProcSubst,
		// BinaryCmd, control flow, subshells, etc. surface as their own
		// Stmt visits and emit their own atoms.
		return true
	})
	return atoms
}

func extractCall(c *syntax.CallExpr, atoms *[]Atom) {
	if len(c.Args) == 0 {
		// Bare assignments without a command (FOO=bar on a line by itself)
		// don't run anything — skip.
		return
	}
	argv := make([]string, 0, len(c.Args))
	for _, w := range c.Args {
		argv = append(argv, flattenWord(w))
	}
	*atoms = append(*atoms, Atom{
		Kind: AtomCall,
		Argv: argv,
		Line: strings.Join(argv, " "),
	})

	// Nested invocations: bash -c, sh -c, etc. carry a string argument that
	// is itself a script. Re-parse it so anything inside is a first-class
	// atom too.
	if isShell(argv[0]) {
		if inner := dashCArg(argv); inner != "" {
			*atoms = append(*atoms, Split(inner)...)
		}
	}
	// eval joins its args and runs them; same treatment.
	if argv[0] == "eval" && len(argv) > 1 {
		*atoms = append(*atoms, Split(strings.Join(argv[1:], " "))...)
	}
}

func extractRedirect(r *syntax.Redirect, atoms *[]Atom) {
	op := r.Op.String()
	if !isWriteOp(r.Op) {
		return
	}
	target := flattenWord(r.Word)
	if target == "" {
		return
	}
	*atoms = append(*atoms, Atom{
		Kind:   AtomRedirect,
		Op:     op,
		Target: target,
	})
}

// flattenWord renders a Word as a single string with literal text preserved
// and expansions replaced by placeholders. This is intentionally lossy: the
// real expanded value is not knowable at parse time, but the placeholder
// preserves enough shape for regex matching.
func flattenWord(w *syntax.Word) string {
	if w == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range w.Parts {
		b.WriteString(flattenPart(p))
	}
	return b.String()
}

func flattenPart(p syntax.WordPart) string {
	switch x := p.(type) {
	case *syntax.Lit:
		return x.Value
	case *syntax.SglQuoted:
		return x.Value
	case *syntax.DblQuoted:
		var b strings.Builder
		for _, inner := range x.Parts {
			b.WriteString(flattenPart(inner))
		}
		return b.String()
	case *syntax.ParamExp:
		return "$" + paramName(x)
	case *syntax.CmdSubst:
		return "$(...)"
	case *syntax.ArithmExp:
		return "$((...))"
	case *syntax.ProcSubst:
		if x.Op == syntax.CmdIn {
			return "<(...)"
		}
		return ">(...)"
	case *syntax.ExtGlob:
		return ""
	}
	return ""
}

func paramName(p *syntax.ParamExp) string {
	if p == nil || p.Param == nil {
		return "?"
	}
	return p.Param.Value
}

func isWriteOp(op syntax.RedirOperator) bool {
	switch op {
	case syntax.RdrOut, syntax.AppOut, syntax.ClbOut, syntax.RdrAll, syntax.AppAll:
		return true
	}
	return false
}

var shellNames = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "ksh": true, "dash": true, "ash": true,
}

func isShell(name string) bool {
	return shellNames[path.Base(name)]
}

// dashCArg returns the argument that follows -c in argv, if any. CC's tool
// inputs may use either "-c" + next-arg or "-c<concatenated>"; we only
// canonicalize the first form because the parser's argv split already
// separates -c from the next arg.
func dashCArg(argv []string) string {
	for i, a := range argv {
		if a == "-c" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
