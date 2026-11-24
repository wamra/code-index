package indexer

import (
	"testing"
)

func edgeKeys(rows []*edgeRow) (out []string) {
	for _, e := range rows {
		out = append(out, e.sourceName()+" "+e.conf+" "+e.reason)
	}
	return out
}

// The merge rules of edgeSet, in every add order.
func TestEdgeSetMerge(t *testing.T) {
	site := func(dst int64, line int) edgeRow {
		return edgeRow{src: 1, dst: dst, kind: EdgeCalls, file: 1, line: line, col: 2}
	}
	lspE := func(dst int64, conf string) edgeRow {
		e := site(dst, 5)
		e.lsp, e.conf = true, conf
		return e
	}
	astE := func(dst int64, conf, reason string, hasTo bool) edgeRow {
		e := site(dst, 5)
		e.ast, e.conf, e.reason, e.hasTo = true, conf, reason, hasTo
		return e
	}
	for _, c := range []struct {
		name string
		in   []edgeRow
		want []string // sorted rows: "source confidence reason"
	}{
		{"name-only exact + lsp exact", []edgeRow{lspE(10, ConfExact), astE(10, ConfExact, "", false)}, []string{"lsp+ast exact "}},
		{"lsp candidate + name-only exact: stronger wins", []edgeRow{lspE(10, ConfCandidate), astE(10, ConfExact, "direct", false)}, []string{"lsp+ast exact direct"}},
		{"lsp exact + name-only candidate: stronger wins", []edgeRow{lspE(10, ConfExact), astE(10, ConfCandidate, "guess", false)}, []string{"lsp+ast exact "}},
		{"tie keeps the record's reason", []edgeRow{lspE(10, ConfCandidate), astE(10, ConfCandidate, "name-match", false)}, []string{"lsp+ast candidate name-match"}},
		{"lsp alias vs record alias", []edgeRow{lspE(10, ConfAlias), astE(10, ConfAlias, "alias", false)}, []string{"lsp+ast alias alias"}},
		{"to-record keeps its confidence over a stronger lsp edge", []edgeRow{lspE(10, ConfExact), astE(10, ConfCandidate, "string-eval", true)}, []string{"lsp+ast candidate string-eval"}},
		{"to-record drops an lsp edge to another destination", []edgeRow{lspE(11, ConfExact), astE(10, ConfExact, "direct", true)}, []string{"ast exact direct"}},
		{"to-record, same destination merges, other lsp edge dropped", []edgeRow{lspE(11, ConfExact), lspE(10, ConfExact), astE(10, ConfCandidate, "x", true)}, []string{"lsp+ast candidate x"}},
		{"several to-records at one site are all kept", []edgeRow{lspE(12, ConfExact), astE(10, ConfExact, "direct", true), astE(11, ConfCandidate, "name", true)}, []string{"ast exact direct", "ast candidate name"}},
		{"a name-only record does not make the site authoritative", []edgeRow{lspE(11, ConfExact), astE(10, ConfExact, "", false)}, []string{"lsp exact ", "ast exact "}},
		{"other call sites are untouched", []edgeRow{func() edgeRow { e := lspE(11, ConfExact); e.line = 9; return e }(), astE(10, ConfExact, "direct", true)}, []string{"ast exact direct", "lsp exact "}},
	} {
		// every permutation of the add order gives the same rows
		perms := permutations(len(c.in))
		for _, p := range perms {
			var s edgeSet
			for _, i := range p {
				s.add(c.in[i])
			}
			rows := s.sorted()
			got := edgeKeys(rows)
			if len(got) != len(c.want) {
				t.Errorf("%s %v: rows %q, want %q", c.name, p, got, c.want)
				continue
			}
			set := map[string]int{}
			for _, g := range got {
				set[g]++
			}
			for _, w := range c.want {
				set[w]--
			}
			for k, n := range set {
				if n != 0 {
					t.Errorf("%s %v: rows %q, want %q (%q)", c.name, p, got, c.want, k)
					break
				}
			}
		}
	}
}

func permutations(n int) [][]int {
	var out [][]int
	var rec func(cur []int, used []bool)
	rec = func(cur []int, used []bool) {
		if len(cur) == n {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for i := 0; i < n; i++ {
			if !used[i] {
				used[i] = true
				rec(append(cur, i), used)
				used[i] = false
			}
		}
	}
	rec(nil, make([]bool, n))
	return out
}
