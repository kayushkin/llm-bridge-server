package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kayushkin/tool-store/internal/childprocess"
	"github.com/kayushkin/tool-store/schema"
)

// Grep returns a tool for searching file contents with grep/ripgrep.
// Models tend to batch search queries into a single call when a dedicated tool exists.
func Grep() Impl {
	// Recursive is a *bool, not a bool, because the schema advertises a default
	// of true. A plain bool cannot tell "the caller omitted the key" from "the
	// caller sent false" — both arrive as the zero value — so reading one would
	// have turned every call that omits the key into a non-recursive search.
	// The pointer is what lets absent mean the advertised default.
	type input struct {
		Pattern   string   `json:"pattern"`
		Paths     []string `json:"paths"`
		Recursive *bool    `json:"recursive"`
		Flags     string   `json:"flags"`
	}
	return Impl{
		Name: "ripgrep",
		Description: `Search file contents for a pattern using ripgrep (rg). Returns matching lines with file:line prefix.
- pattern: regex pattern to search for
- paths: files or directories to search (default: current directory)
- recursive: search directories recursively (default: true)
- flags: extra flags like "-i" (case insensitive), "-l" (files only), "-w" (word match), "--type go"
Prefer this over shell grep — it respects .gitignore and is faster.`,
		InputSchema: schema.Props([]string{"pattern"}, map[string]any{
			"pattern":   schema.Str("Regex pattern to search for"),
			"paths":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Files or directories to search (default: .)"},
			"recursive": schema.Bool("Search recursively (default: true)"),
			"flags":     schema.Str("Extra rg flags like -i, -l, -w, --type go"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			_, rgErr := exec.LookPath("rg")
			bin, args, nothingToSearch := searchCommand(in.Pattern, in.Paths, in.Flags, in.Recursive, rgErr == nil)
			if nothingToSearch {
				return "no matches found", nil
			}

			cmd := childprocess.NewCommand(ctx, bin, args...)
			cmd.Env = ensureDevToolsOnPath()
			out, err := cmd.CombinedOutput()
			result := strings.TrimSpace(string(out))

			if result == "" {
				return "no matches found", nil
			}

			// Truncate if too many results
			lines := strings.Split(result, "\n")
			if len(lines) > 200 {
				result = strings.Join(lines[:200], "\n") + fmt.Sprintf("\n... (%d more matches)", len(lines)-200)
			}

			return result, nil
		},
	}
}

// searchCommand decides which binary to run and with what arguments. It is a
// separate function, and takes rgAvailable rather than looking it up, because
// the grep branch is unreachable on any host that has ripgrep installed — so
// on the machines that develop this tool, the fallback can only be tested by
// asserting on the command it would build. Returning the argv rather than
// running it is what makes that possible.
//
// nothingToSearch reports that there are no operands left to hand the command.
// It is not the same as "no matches": grep invoked with no path operand reads
// stdin, and a tool call that blocks on stdin never returns.
func searchCommand(pattern string, paths []string, flags string, recursiveOpt *bool, rgAvailable bool) (bin string, args []string, nothingToSearch bool) {
	// The schema's stated default. Absent means recursive.
	recursive := recursiveOpt == nil || *recursiveOpt

	searchPaths := paths
	if len(searchPaths) == 0 {
		searchPaths = []string{"."}
	}

	if rgAvailable {
		bin = "rg"
		// -H because the description promises "matching lines with file:line
		// prefix", and both rg and grep drop the filename when exactly one
		// file is searched. It goes before the caller's flags so a caller
		// asking for bare lines (-I in rg, -h in grep) still wins.
		args = []string{"-n", "-H", "--no-heading"}
		if !recursive {
			// rg descends into directory operands by default; depth 1 keeps it
			// to the files directly inside one. Operands that name a file are
			// still searched.
			args = append(args, "--max-depth", "1")
		}
	} else {
		bin = "grep"
		if recursive {
			args = []string{"-rn", "-H"}
		} else {
			args = []string{"-n", "-H"}
			// grep has no max-depth flag and reports a directory operand as an
			// error instead of searching what is in it, so a non-recursive
			// search has to name the files itself.
			searchPaths = filesDirectlyInside(searchPaths)
			if len(searchPaths) == 0 {
				return bin, nil, true
			}
		}
	}

	if flags != "" {
		args = append(args, strings.Fields(flags)...)
	}
	args = append(args, pattern)
	args = append(args, searchPaths...)
	return bin, args, false
}

// filesDirectlyInside replaces each directory operand with the regular files
// immediately inside it and passes everything else through unchanged. It is
// how the grep fallback expresses a non-recursive search: grep has no
// max-depth flag, so the caller has to do the descending it is refusing to do.
// A path that cannot be read is dropped rather than reported — grep would have
// said the same thing about it, and the search over the readable operands is
// still worth running.
func filesDirectlyInside(paths []string) []string {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			out = append(out, filepath.Join(p, e.Name()))
		}
	}
	return out
}
