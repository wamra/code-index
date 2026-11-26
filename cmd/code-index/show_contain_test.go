package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadInsideRoot(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	outside := filepath.Join(tmp, "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(root, "sub", "ok.txt"), []byte("fine"), 0o644)

	if b, err := readInsideRoot(root, "sub/ok.txt"); err != nil || string(b) != "fine" {
		t.Fatalf("inside file: %q %v", b, err)
	}
	if _, err := readInsideRoot(root, "sub/missing.txt"); err == nil || errors.Is(err, errOutsideRoot) {
		t.Errorf("missing inside file must be a plain error: %v", err)
	}
	for name, rel := range map[string]string{
		"dotdot":        "../secret.txt",
		"nested dotdot": "sub/../../secret.txt",
		"absolute":      outside,
		"slash":         "/etc/hosts",
	} {
		if _, err := readInsideRoot(root, rel); !errors.Is(err, errOutsideRoot) {
			t.Errorf("%s (%q): want errOutsideRoot, got %v", name, rel, err)
		}
	}
	if runtime.GOOS == "windows" {
		return // symlinks need privileges there
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tmp, filepath.Join(root, "sub", "dirlink")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"link.txt", "sub/dirlink/secret.txt"} {
		if _, err := readInsideRoot(root, rel); !errors.Is(err, errOutsideRoot) {
			t.Errorf("symlink %q: want errOutsideRoot, got %v", rel, err)
		}
	}
	// a symlink that stays inside the root is fine
	os.Symlink(filepath.Join(root, "sub", "ok.txt"), filepath.Join(root, "inlink.txt"))
	if b, err := readInsideRoot(root, "inlink.txt"); err != nil || string(b) != "fine" {
		t.Errorf("inside symlink: %q %v", b, err)
	}
}
