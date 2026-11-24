package indexer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wamra/code-index/internal/indexer"
	"github.com/wamra/code-index/internal/store"
)

// synthRepo writes nFiles shell files of nFuncs functions each, every function
// calling callsPer others (in this and the next file), and returns the root and
// the JSON lines a wammybash-like extractor would print for it (func records and
// call records with "to"). It stresses code-index's own loops (JSON decoding,
// symbol rows, edge resolution) rather than a subprocess.
func synthRepo(tb testing.TB, nFiles, nFuncs, callsPer int) (string, []byte) {
	root := tb.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	name := func(i, j int) string { return fmt.Sprintf("fn_%d_%d", i, j) }
	for i := 0; i < nFiles; i++ {
		file := filepath.Join(root, fmt.Sprintf("d%d", i%50), fmt.Sprintf("f%d.sh", i))
		var src bytes.Buffer
		src.WriteString("#!/usr/bin/env bash\n")
		line := 2
		var recs []map[string]any
		for j := 0; j < nFuncs; j++ {
			fmt.Fprintf(&src, "# Function %s does step %d of file %d.\n", name(i, j), j, i)
			line++
			start := line
			fmt.Fprintf(&src, "%s() {\n", name(i, j))
			recs = append(recs, map[string]any{"t": "func", "file": file, "name": name(i, j), "line": start, "col": 1, "endLine": start + callsPer + 1, "endCol": 2})
			for c := 0; c < callsPer; c++ {
				ti, tj := (i+c)%nFiles, (j+c+1)%nFuncs
				fmt.Fprintf(&src, "  %s \"$1\"\n", name(ti, tj))
				to := filepath.Join(root, fmt.Sprintf("d%d", ti%50), fmt.Sprintf("f%d.sh", ti))
				recs = append(recs, map[string]any{"t": "call", "file": file, "line": start + 1 + c, "col": 3, "name": name(ti, tj), "to": to,
					"resolution": "resolved", "reason": "direct", "in": name(i, j)})
			}
			src.WriteString("}\n")
			line += callsPer + 2
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(file, src.Bytes(), 0o644); err != nil {
			tb.Fatal(err)
		}
		for _, r := range recs {
			enc.Encode(r)
		}
	}
	return root, out.Bytes()
}

// BenchmarkRunSynthetic is `index --force` over 2000 generated files (10 000
// functions, 40 000 calls) with the extractor replaced by recorded output:
//
//	go test ./internal/indexer -run '^$' -bench RunSynthetic -benchtime 3x -cpuprofile cpu.out
func BenchmarkRunSynthetic(b *testing.B) {
	root, recs := synthRepo(b, 2000, 5, 4)
	cfg := bashExecCfg()
	extract := func(context.Context, string, []string) ([]byte, error) { return recs, nil }
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		st, err := store.Open(filepath.Join(b.TempDir(), "db", "index.sqlite"))
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		res, err := indexer.Run(context.Background(), st, cfg, indexer.Options{Root: root, Force: true, Log: io.Discard, Extract: extract})
		b.StopTimer()
		if err != nil || res.Failed != 0 || res.Indexed != 2000 || res.Edges < 40000 {
			b.Fatalf("%+v %v", res, err)
		}
		st.Close()
	}
}
