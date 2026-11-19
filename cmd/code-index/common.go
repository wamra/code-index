package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

func configPath() string { return config.Path(homeDir(), os.Getenv) }

// loadConfig returns the effective (resolved, validated) configuration.
func loadConfig() (config.Config, error) {
	c, _, err := config.Load(configPath(), os.Getenv)
	if err != nil {
		return config.Config{}, err
	}
	c = c.Resolved()
	if err := c.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("config %s: %w", configPath(), err)
	}
	return c, nil
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

// openExisting opens the index of the workspace that contains the working
// directory (or the one named by CODE_INDEX_DB) and returns its root.
func openExisting() (*store.Store, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	root := workspace.Find(cwd, homeDir(), os.Getenv)
	p := workspace.DBPath(root, homeDir(), os.Getenv)
	if _, err := os.Stat(p); err != nil {
		return nil, "", fmt.Errorf("no index for %s (looked for %s); run `code-index index PATH` first", root, p)
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

// realPath resolves p to an absolute path with symlinks resolved.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	return abs, nil
}

func fail(errw io.Writer, format string, a ...any) int {
	fmt.Fprintf(errw, format+"\n", a...)
	return 1
}
