package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wamra/code-index/internal/config"
)

// cmdConfig prints the effective configuration, or with `init` writes a
// starter config file with the language servers found on this machine.
func cmdConfig(args []string, out, errw io.Writer) int {
	fs := newFlags("config", errw)
	force := fs.Bool("force", false, "init: overwrite an existing config file")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	path := configPath()
	switch {
	case len(pos) == 0:
		c, found, err := config.Load(path, os.Getenv)
		if err != nil {
			return fail(errw, "config: %v", err)
		}
		src := path + " (not found: defaults and environment only)"
		if found {
			src = path
		}
		if err := c.Resolved().Validate(); err != nil {
			return fail(errw, "config: %v", err)
		}
		out.Write(config.Effective(c, src, os.Getenv))
		return 0
	case len(pos) == 1 && pos[0] == "init":
		if _, err := os.Stat(path); err == nil && !*force {
			return fail(errw, "config init: %s exists (use --force to overwrite)", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fail(errw, "config init: %v", err)
		}
		langs := config.Discover(homeDir(), nil)
		if err := os.WriteFile(path, config.Init(langs), 0o644); err != nil {
			return fail(errw, "config init: %v", err)
		}
		fmt.Fprintf(out, "wrote %s\n", path)
		if len(langs) == 0 {
			fmt.Fprintln(out, "no known language server found on PATH; add entries under \"languages\" by hand")
		}
		for _, n := range sortedKeys(langs) {
			fmt.Fprintf(out, "  language %s: %s\n", n, langs[n].Command)
		}
		return 0
	}
	fmt.Fprintln(errw, "config: usage: config [init [--force]]")
	return 2
}
