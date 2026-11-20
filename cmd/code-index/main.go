// Command code-index indexes the symbols of a source tree through language
// servers and searches them.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = ""

func cmdVersion(_ []string, out, _ io.Writer) int {
	v := version
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	fmt.Fprintln(out, "code-index", v)
	return 0
}

type command struct {
	run  func(args []string, out, errw io.Writer) int
	help string
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"index":         {cmdIndex, "index [PATH] [--embed] [--force] [--no-edges] [--quiet]"},
		"sweep":         {cmdSweep, "sweep [--embed] [--force] [--no-edges] [--quiet]"},
		"search":        {cmdSearch, "search QUERY [--mode auto|vector|fts|hybrid] [--kind KIND] [--lang LANG] [--path SUBSTR] [--k 10] [--json]"},
		"show":          {cmdShow, "show SYMBOL [--source] [--json]   (SYMBOL: qualified name, name or path:line)"},
		"callers":       {cmdCallers, "callers SYMBOL [--json]   (who calls SYMBOL)"},
		"callees":       {cmdCallees, "callees SYMBOL [--json]   (what SYMBOL calls)"},
		"impact":        {cmdImpact, "impact SYMBOL [--depth 3] [--json]   (transitive callers)"},
		"path":          {cmdPath, "path FROM TO [--json]   (shortest call path)"},
		"stats":         {cmdStats, "stats"},
		"doctor":        {cmdDoctor, "doctor"},
		"config":        {cmdConfig, "config [init [--force]]"},
		"install-skill": {cmdInstallSkill, "install-skill [--dir SKILLS_DIR] [--dry-run]   (writes the code-search skill, default ~/.claude/skills)"},
		"version":       {cmdVersion, "version"},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: code-index <command> [flags]")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(w, "  "+commands[n].help)
	}
}

func run(args []string, out, errw io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(errw)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	c, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(errw, "code-index: unknown command %q\n", args[0])
		usage(errw)
		return 2
	}
	return c.run(args[1:], out, errw)
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func newFlags(name string, errw io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errw)
	return fs
}

// parseInterspersed parses flags that may appear before or after positional
// arguments and returns the positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
