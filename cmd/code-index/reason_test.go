package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/store"
)

// The edge tag gains the reason only when the edge has one; the JSON gains
// "reason" (omitted when empty); stats counts new kinds and confidences
// without being told about them.
func TestEdgeReasonCandidateAndInvokesOutput(t *testing.T) {
	e := setupOpts(t, "", "refs")
	graphTree(t, e)
	mustOK(t, "index", e.root, "--quiet")
	if got := mustOK(t, "callers", "Gamma"); got != "cycle.ps1:5  Beta  [lsp candidate references]\ncycle.ps1:11  Delta  [lsp candidate references]\n" {
		t.Fatalf("a references-only server gives candidate edges with the reason references:\n%s", got)
	}

	st, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE edges SET confidence='candidate', reason='call-hierarchy' WHERE kind='calls' AND line=4`, // Beta -> Gamma at cycle.ps1:5
		`UPDATE edges SET reason=NULL WHERE kind='calls' AND line=10`,                                    // Delta -> Gamma at cycle.ps1:11: an edge without a reason prints no reason
		`INSERT INTO edges(src_symbol_id, dst_symbol_id, dst_name, kind, file_id, line, col, source, confidence, reason)
			VALUES (NULL, NULL, 'd1.ps1', 'invokes', (SELECT id FROM files WHERE path='cycle.ps1'), 20, 0, 'ast', 'exact', 'direct')`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	if got := mustOK(t, "callers", "Gamma"); got != "cycle.ps1:5  Beta  [lsp candidate call-hierarchy]\ncycle.ps1:11  Delta  [lsp candidate]\n" {
		t.Errorf("callers:\n%s", got)
	}
	if got := mustOK(t, "impact", "Gamma", "--depth", "1"); !strings.Contains(got, "d1  cycle.ps1:5  Beta -> Gamma  [lsp candidate call-hierarchy]\n") {
		t.Errorf("impact:\n%s", got)
	}
	var js struct {
		Edges []map[string]any `json:"edges"`
	}
	if err := json.Unmarshal([]byte(mustOK(t, "callers", "Gamma", "--json")), &js); err != nil || len(js.Edges) != 2 {
		t.Fatalf("json: %v %+v", err, js)
	}
	if js.Edges[0]["reason"] != "call-hierarchy" || js.Edges[0]["confidence"] != "candidate" {
		t.Errorf("edge 0 = %v", js.Edges[0])
	}
	if _, has := js.Edges[1]["reason"]; has {
		t.Errorf("an edge without a reason must not carry the key: %v", js.Edges[1])
	}
	stats := mustOK(t, "stats")
	for _, want := range []string{"invokes", "candidate"} {
		if !strings.Contains(stats, want) {
			t.Errorf("stats lacks %q:\n%s", want, stats)
		}
	}
	// file-level edges other than calls (dot_sources, invokes) are not listed by callees
	if got := mustOK(t, "callees", "cycle.ps1"); strings.Contains(got, "d1.ps1") {
		t.Errorf("callees lists the invokes edge:\n%s", got)
	}
}
