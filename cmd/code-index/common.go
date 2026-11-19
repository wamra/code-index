package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/store"
	"github.com/wamra/code-index/internal/workspace"
)

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// configPath is the global config file.
func configPath() string { return config.Path(homeDir(), os.Getenv) }

// cwdRoot is the repo root of the working directory (for a non-git directory,
// the nearest ancestor that already has an index).
func cwdRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return workspace.FindIndexed(cwd)
}

// loadConfig returns the effective (resolved, validated) configuration for the
// repo at root: the global file with <root>/.Wammy/config.json merged over it.
func loadConfig(root string) (config.Config, config.Sources, error) {
	c, src, err := config.LoadLayered(configPath(), config.RepoPath(root), os.Getenv)
	if err != nil {
		return config.Config{}, nil, err
	}
	c = c.Resolved()
	if err := c.Validate(); err != nil {
		return config.Config{}, nil, fmt.Errorf("config: %w", err)
	}
	return c, src, nil
}

// newEmbedder builds the configured embedder, or nil when embeddings are not
// enabled. When the model's vector size is unknown and st is given, the
// index's stored size is used.
func newEmbedder(cfg config.Config, st *store.Store) (provider.Embedder, error) {
	if !cfg.EmbedEnabled() {
		return nil, nil
	}
	ec := cfg.Embed.EmbedConfig
	if ec.Dims == 0 && st != nil {
		if d, err := st.VecDims(context.Background()); err == nil {
			ec.Dims = d
		}
	}
	return provider.NewEmbedder(ec)
}

// openExisting opens the index of the repo that contains the working
// directory (found by walking up to the nearest .git, or CODE_INDEX_DB) and
// returns the repo root.
func openExisting() (*store.Store, string, error) {
	root := cwdRoot()
	p := workspace.DBPath(root, os.Getenv)
	if _, err := os.Stat(p); err != nil {
		return nil, "", fmt.Errorf("no index for %s (looked for %s); run `code-index index`", root, p)
	}
	st, err := store.Open(p)
	if err != nil {
		return nil, "", fmt.Errorf("open index: %w", err)
	}
	if r, _ := st.Meta(context.Background(), "root"); r != "" {
		root = r
	}
	return st, root, nil
}

func fail(errw io.Writer, format string, a ...any) int {
	fmt.Fprintf(errw, format+"\n", a...)
	return 1
}
