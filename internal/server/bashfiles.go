package server

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// extractBashFilePaths parses a Bash command and returns a deduplicated list
// of files that the command appears to mutate (create, modify, or delete).
// Paths are resolved against cwd if provided. Best-effort: words that contain
// shell expansion ($VAR, $(cmd), globs) are skipped because we can't resolve
// them without executing the shell.
//
// Recognized mutators:
//   - Output redirects: `>`, `>>`, `2>`, `&>`, `<>`, `>|`
//   - Heredoc-out: `<<` / `<<<` are read-only on the input side, ignored
//   - Commands: rm, unlink, touch, cp, mv, ln, sed -i, tee, dd of=...
//
// Read-only inspectors (cat, head, grep, ls, find, …) are deliberately
// excluded — capturing them would just clutter the diff view.
func extractBashFilePaths(command, cwd string) []string {
	if command == "" {
		return nil
	}
	r := strings.NewReader(command)
	file, err := syntax.NewParser().Parse(r, "")
	if err != nil {
		// A malformed shell command can still be one Claude tried to run; we
		// just can't extract from it. Returning nil is fine — the snapshot
		// is best-effort and the Bash output itself is still rendered.
		return nil
	}

	seen := make(map[string]struct{})
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Redirect:
			collectRedirectFile(n, add)
		case *syntax.CallExpr:
			collectCommandFiles(n, add)
		}
		return true
	})
	return out
}

// collectRedirectFile records the target file of a write-style redirect.
// Read-side redirects (`<`, `<<`, `<<<`) don't mutate state so we skip them.
func collectRedirectFile(r *syntax.Redirect, add func(string)) {
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll,
		syntax.ClbOut, syntax.RdrInOut:
		// > >> &> &>> >| <>
		if p, ok := simpleWord(r.Word); ok {
			add(p)
		}
	}
}

// collectCommandFiles inspects a single command invocation for known
// file-mutating utilities and records their target paths.
func collectCommandFiles(c *syntax.CallExpr, add func(string)) {
	if len(c.Args) == 0 {
		return
	}
	cmd, ok := simpleWord(c.Args[0])
	if !ok {
		return
	}
	// Strip a leading path so `/usr/bin/rm` matches `rm`.
	base := filepath.Base(cmd)

	args := make([]string, 0, len(c.Args)-1)
	for _, a := range c.Args[1:] {
		if p, ok := simpleWord(a); ok {
			args = append(args, p)
		} else {
			args = append(args, "") // placeholder so positional args stay aligned
		}
	}

	switch base {
	case "rm", "unlink", "shred":
		// All non-flag args are files (or dirs — Stat will skip dirs).
		for _, a := range positionals(args) {
			add(a)
		}
	case "touch":
		for _, a := range positionals(args) {
			add(a)
		}
	case "cp", "install":
		// Last positional is the destination; intermediates are sources we
		// don't need to snapshot (they're not being mutated). If only one
		// positional, that's also the destination.
		ps := positionals(args)
		if len(ps) > 0 {
			add(ps[len(ps)-1])
		}
	case "mv", "rename":
		ps := positionals(args)
		// Both source (deleted) and destination (created) change.
		for _, p := range ps {
			add(p)
		}
	case "ln":
		ps := positionals(args)
		if len(ps) > 0 {
			// Destination is the last positional; if missing, ln uses cwd
			// with the source's basename, which we can't resolve generically.
			add(ps[len(ps)-1])
		}
	case "sed":
		// Only `-i` mutates files in place. Detect it and treat trailing
		// non-flag args as targets (skipping the script arg right after -e).
		if !sliceContainsFlag(args, "-i") && !sliceHasPrefix(args, "-i") {
			return
		}
		for _, a := range sedTargets(args) {
			add(a)
		}
	case "tee":
		// All non-flag positional args are output files.
		for _, a := range positionals(args) {
			add(a)
		}
	case "dd":
		// dd uses key=value args; the output file is `of=...`.
		for _, a := range args {
			if rest, ok := strings.CutPrefix(a, "of="); ok {
				add(rest)
			}
		}
	}
}

// simpleWord returns the literal text of a word that has no shell expansion.
// Returns ok=false for words containing $VAR, $(cmd), `cmd`, globs, or
// arithmetic — anything we can't resolve without running the shell.
func simpleWord(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[") {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// positionals filters out flag-shaped args (anything starting with `-`).
// Empty strings (placeholders for words we couldn't resolve) are dropped.
func positionals(args []string) []string {
	out := make([]string, 0, len(args))
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--" {
			// Everything after `--` is positional regardless of leading dash.
			continue
		}
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func sliceContainsFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func sliceHasPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) && a != prefix {
			return true
		}
	}
	return false
}

// sedTargets pulls trailing file args off a `sed -i …` invocation. We assume
// the conventional ordering: flags, then a script, then files. The script can
// be supplied via `-e SCRIPT` or as the first non-flag positional; either way
// we drop the first positional and treat the rest as files. This matches how
// people actually use `sed -i`.
func sedTargets(args []string) []string {
	pos := positionals(args)
	if len(pos) <= 1 {
		// No file targets supplied (sed reading stdin, or only the script).
		return nil
	}
	return pos[1:]
}
