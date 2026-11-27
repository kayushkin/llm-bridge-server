// probe reads a Bash command from stdin (or argv) and prints the splitter's
// atom output as indented JSON. Useful for sanity-checking what the rule
// engine will see when crafting Bash atom regexes.
//
//	echo 'git status && rm -rf foo' | probe
//	probe 'git status && rm -rf foo'
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/permissions/splitter"
)

func main() {
	cmd := readCommand()
	atoms := splitter.Split(cmd)
	out, err := json.MarshalIndent(atoms, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

func readCommand() string {
	if len(os.Args) > 1 {
		return strings.Join(os.Args[1:], " ")
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: read stdin:", err)
		os.Exit(1)
	}
	return strings.TrimRight(string(b), "\n")
}
