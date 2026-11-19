package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wamra/code-index/skills"
)

// skillsDir is the Claude Code skills directory: --dir, else <home>/.claude/skills.
func skillsDir(flagDir string) string {
	if flagDir != "" {
		return flagDir
	}
	return filepath.Join(homeDir(), ".claude", "skills")
}

func cmdInstallSkill(args []string, out, errw io.Writer) int {
	fs := newFlags("install-skill", errw)
	dir := fs.String("dir", "", "skills directory (default ~/.claude/skills)")
	dry := fs.Bool("dry-run", false, "print the target path and write nothing")
	if pos, err := parseInterspersed(fs, args); err != nil || len(pos) != 0 {
		return 2
	}
	target := filepath.Join(skillsDir(*dir), "code-search", "SKILL.md")
	if *dry {
		fmt.Fprintf(out, "dry run: skill would be written to %s\n", target)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fail(errw, "install-skill: %v", err)
	}
	if err := os.WriteFile(target, skills.CodeSearch, 0o644); err != nil {
		return fail(errw, "install-skill: %v", err)
	}
	fmt.Fprintf(out, "skill installed: %s\n", target)
	return 0
}
