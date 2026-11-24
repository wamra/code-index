package lsp_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/lsptest"
)

func TestCallHierarchy(t *testing.T) {
	root := t.TempDir()
	src := "function Alpha {\n  Beta\n}\nfunction Beta {\n}\n"
	file := filepath.Join(root, "a.ps1")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := lsp.PathToURI(file)
	ctx := context.Background()

	cl := pipeClient(t, lsptest.Options{})
	if err := cl.Initialize(ctx, root); err != nil {
		t.Fatal(err)
	}
	if cl.Capabilities().CallHierarchy {
		t.Fatal("call hierarchy must not be advertised by default")
	}
	if _, err := cl.PrepareCallHierarchy(ctx, uri, lsp.Position{Line: 3, Character: 9}); !errors.Is(err, lsp.ErrNotAdvertised) {
		t.Fatalf("PrepareCallHierarchy without capability: %v", err)
	}
	if _, err := cl.IncomingCalls(ctx, lsp.CallHierarchyItem{}); !errors.Is(err, lsp.ErrNotAdvertised) {
		t.Fatalf("IncomingCalls without capability: %v", err)
	}

	cl = pipeClient(t, lsptest.Options{CallHier: true})
	if err := cl.Initialize(ctx, root); err != nil {
		t.Fatal(err)
	}
	if !cl.Capabilities().CallHierarchy {
		t.Fatal("callHierarchyProvider not read")
	}
	cl.DidOpen(uri, "powershell", 1, src)
	items, err := cl.PrepareCallHierarchy(ctx, uri, lsp.Position{Line: 3, Character: 9}) // Beta
	if err != nil || len(items) != 1 || items[0].Name != "Beta" || items[0].URI != uri || len(items[0].Data) == 0 {
		t.Fatalf("items = %+v, err = %v", items, err)
	}
	calls, err := cl.IncomingCalls(ctx, items[0])
	if err != nil {
		t.Fatal(err)
	}
	want := lsp.Range{Start: lsp.Position{Line: 1, Character: 2}, End: lsp.Position{Line: 1, Character: 6}}
	if len(calls) != 1 || calls[0].From.Name != "Alpha" || calls[0].From.URI != uri || len(calls[0].FromRanges) != 1 || calls[0].FromRanges[0] != want {
		t.Fatalf("calls = %+v", calls)
	}
	// no callable at the position: null, no items
	items, err = cl.PrepareCallHierarchy(ctx, uri, lsp.Position{Line: 1, Character: 2})
	if err != nil || len(items) != 0 {
		t.Fatalf("items at a call site = %+v, err = %v", items, err)
	}
}
