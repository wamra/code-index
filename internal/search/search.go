package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/wamra/code-index/internal/embed"
	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/store"
)

// Modes accepted by Opts.Mode.
const (
	ModeAuto   = "auto"
	ModeVector = "vector"
	ModeFTS    = "fts"
	ModeHybrid = "hybrid"
)

// Opts configures Run.
type Opts struct {
	Mode string
	K    int
	// Kind filters by symbol kind ("function", "class", ..., or "file").
	Kind string
	Lang string
	// Path keeps hits whose file path contains this text.
	Path string
	// Embedder is nil when none is configured.
	Embedder provider.Embedder
	// Root is the workspace root, used for the stale marker ("" disables it).
	Root string
	Warn func(string)
}

// Hit is one search result: a symbol or a file.
type Hit struct {
	TargetKind string  `json:"target_kind"` // symbol | file
	SymbolID   int64   `json:"symbol_id,omitempty"`
	FileID     int64   `json:"file_id"`
	Name       string  `json:"name"`
	Qualified  string  `json:"qualified_name"`
	Kind       string  `json:"kind"`
	Lang       string  `json:"lang"`
	Path       string  `json:"path"`
	Line       int     `json:"line"` // 1-based
	Signature  string  `json:"signature,omitempty"`
	Summary    string  `json:"summary,omitempty"`
	Score      float64 `json:"score"`
	Stale      string  `json:"stale,omitempty"` // "", "modified" or "deleted"
}

type cand struct {
	kind  string // symbol | file
	id    int64
	score float64
}

func (c cand) key() string { return fmt.Sprintf("%s:%d", c.kind, c.id) }

func warn(o Opts, format string, a ...any) {
	if o.Warn != nil {
		o.Warn(fmt.Sprintf(format, a...))
	}
}

// ErrNoVectors means vector search was requested but is not available; the
// message says why.
var ErrNoVectors = errors.New("vector search unavailable")

// Run searches q and returns the hits and the route taken.
func Run(ctx context.Context, st *store.Store, q string, o Opts) ([]Hit, string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, "", errors.New("empty query")
	}
	k := o.K
	if k <= 0 {
		k = 10
	}
	n := max(3*k, 30)
	mode := o.Mode
	if mode == "" {
		mode = ModeAuto
	}
	db := st.DB()

	vecOK, vecWhy := vectorReady(ctx, st, o.Embedder)

	var ranked []cand
	route := ""
	var err error
	switch mode {
	case ModeFTS:
		ranked, err = lexical(ctx, db, q, n, o)
		route = "fts"
	case ModeVector:
		if !vecOK {
			return nil, "", fmt.Errorf("%w: %s", ErrNoVectors, vecWhy)
		}
		ranked, err = vectorLeg(ctx, db, o.Embedder, q, n, o)
		route = "vector"
	case ModeHybrid:
		var legs [][]cand
		var l []cand
		if l, err = lexical(ctx, db, q, n, o); err != nil {
			return nil, "", err
		}
		legs = append(legs, l)
		route = "hybrid (fts"
		if vecOK {
			if l, err = vectorLeg(ctx, db, o.Embedder, q, n, o); err != nil {
				if !isDown(err) {
					return nil, "", err
				}
				warn(o, "embedder unreachable, skipping the vector leg: %v", err)
			} else {
				legs = append(legs, l)
				route += "+vector"
			}
		} else if o.Embedder != nil {
			warn(o, "skipping the vector leg: %s", vecWhy)
		}
		route += ")"
		ranked = rrf(legs...)
	case ModeAuto:
		if LooksLikeIdentifier(q) {
			ranked, err = nameLeg(ctx, db, q, n, o)
			if err != nil {
				return nil, "", err
			}
			route = "name"
			if len(ranked) > 0 {
				break
			}
		}
		if vecOK {
			ranked, err = vectorLeg(ctx, db, o.Embedder, q, n, o)
			if err == nil {
				route = "vector"
				break
			}
			if !isDown(err) {
				return nil, "", err
			}
			warn(o, "embedder unreachable, using full-text search: %v", err)
		} else if o.Embedder != nil {
			warn(o, "using full-text search: %s", vecWhy)
		}
		ranked, err = chunkFTSLeg(ctx, db, q, n, o)
		route = "fts"
	default:
		return nil, "", fmt.Errorf("unknown mode %q (want auto, vector, fts or hybrid)", mode)
	}
	if err != nil {
		return nil, route, err
	}
	hits, err := hydrate(ctx, db, ranked, o, k)
	return hits, route, err
}

func isDown(err error) bool {
	return errors.Is(err, provider.ErrDown) || errors.Is(err, context.DeadlineExceeded)
}

// vectorReady reports whether the vector leg can run, and if not, why.
func vectorReady(ctx context.Context, st *store.Store, e provider.Embedder) (bool, string) {
	if e == nil {
		return false, "no embedder is configured (set embed.enabled in the config file)"
	}
	dims, err := st.VecDims(ctx)
	if err != nil || dims == 0 {
		return false, "no embeddings stored yet (run `code-index index --embed`)"
	}
	if e.Dims() != 0 && e.Dims() != dims {
		return false, fmt.Sprintf("the embedder produces %d dimensions, the index stores %d (run `code-index index --embed` to re-embed)", e.Dims(), dims)
	}
	fp, _ := st.Meta(ctx, "embed_fingerprint")
	if fp != e.Fingerprint() {
		return false, fmt.Sprintf("the index was embedded with %q, the configured embedder is %q (run `code-index index --embed` to re-embed)", fp, e.Fingerprint())
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE embedded_at IS NOT NULL AND embedded_at <> 'failed'`).Scan(&n); err != nil || n == 0 {
		return false, "no embeddings stored yet (run `code-index index --embed`)"
	}
	return true, ""
}

func rrf(legs ...[]cand) []cand {
	score := map[string]float64{}
	first := map[string]cand{}
	for _, leg := range legs {
		for i, c := range leg {
			score[c.key()] += 1.0 / float64(60+i+1)
			if _, ok := first[c.key()]; !ok {
				first[c.key()] = c
			}
		}
	}
	out := make([]cand, 0, len(score))
	for key, s := range score {
		c := first[key]
		c.score = s
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].key() < out[j].key()
	})
	return out
}

// lexical fuses the name leg and the chunk full-text leg.
func lexical(ctx context.Context, db *sql.DB, q string, n int, o Opts) ([]cand, error) {
	a, err := nameLeg(ctx, db, q, n, o)
	if err != nil {
		return nil, err
	}
	b, err := chunkFTSLeg(ctx, db, q, n, o)
	if err != nil {
		return nil, err
	}
	return rrf(a, b), nil
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// filters renders the kind / lang / path filters on the aliases f (files) and
// kindExpr (the SQL expression for the symbol kind).
func filters(o Opts, kindExpr string, args *[]any) string {
	var sb strings.Builder
	if o.Kind != "" {
		sb.WriteString(` AND ` + kindExpr + ` = ?`)
		*args = append(*args, strings.ToLower(o.Kind))
	}
	if o.Lang != "" {
		sb.WriteString(` AND f.lang = ?`)
		*args = append(*args, o.Lang)
	}
	if o.Path != "" {
		sb.WriteString(` AND f.path LIKE ? ESCAPE '\'`)
		*args = append(*args, "%"+likeEscape(o.Path)+"%")
	}
	return sb.String()
}

func quoteFTS(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// nameLeg searches name_fts (trigram over name, qualified name and path).
// Exact name matches rank first, then exact qualified-name matches, then BM25
// with the name column weighted highest. Trigram needs at least 3 characters
// per term; shorter terms are dropped.
func nameLeg(ctx context.Context, db *sql.DB, q string, n int, o Opts) ([]cand, error) {
	var terms []string
	for _, t := range strings.Fields(q) {
		if len([]rune(t)) >= 3 {
			terms = append(terms, quoteFTS(t))
		}
	}
	if len(terms) == 0 {
		return nil, nil
	}
	args := []any{strings.ToLower(q), strings.ToLower(q), strings.Join(terms, " AND ")}
	where := filters(o, "s.kind", &args)
	args = append(args, n)
	rows, err := db.QueryContext(ctx, `SELECT s.id, -bm25(name_fts, 10.0, 5.0, 1.0)
FROM name_fts JOIN symbols s ON s.id = name_fts.rowid JOIN files f ON f.id = s.file_id
WHERE name_fts MATCH ?3`+where+`
ORDER BY (lower(s.name) = ?1) DESC, (lower(s.qualified_name) = ?2) DESC, bm25(name_fts, 10.0, 5.0, 1.0), s.id
LIMIT ?`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("name search: %w", err)
	}
	return scanCands(rows, "symbol")
}

func scanCands(rows *sql.Rows, kind string) ([]cand, error) {
	defer rows.Close()
	var out []cand
	for rows.Next() {
		c := cand{kind: kind}
		if err := rows.Scan(&c.id, &c.score); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true, "on": true, "for": true,
	"by": true, "and": true, "or": true, "that": true, "which": true, "with": true, "is": true,
	"are": true, "it": true, "this": true, "what": true, "how": true, "does": true, "do": true,
}

// ftsTokens splits q into the lowercase words FTS5's unicode61 tokenizer
// would see, minus stopwords. Underscores split words, as unicode61 does.
func ftsTokens(q string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range wordRe.FindAllString(q, -1) {
		for _, t := range strings.FieldsFunc(w, func(r rune) bool { return r == '_' || !(unicode.IsLetter(r) || unicode.IsDigit(r)) }) {
			t = strings.ToLower(t)
			if len([]rune(t)) < 2 || stopwords[t] || seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// chunkFTSLeg is BM25 over chunk_fts (unicode61) with OR semantics.
func chunkFTSLeg(ctx context.Context, db *sql.DB, q string, n int, o Opts) ([]cand, error) {
	toks := ftsTokens(q)
	if len(toks) == 0 {
		return nil, nil
	}
	for i, t := range toks {
		toks[i] = quoteFTS(t)
	}
	args := []any{strings.Join(toks, " OR ")}
	where := filters(o, "COALESCE(s.kind, c.target_kind)", &args)
	args = append(args, n)
	rows, err := db.QueryContext(ctx, `SELECT c.id, -bm25(chunk_fts)
FROM chunk_fts JOIN chunks c ON c.id = chunk_fts.rowid JOIN files f ON f.id = c.file_id
LEFT JOIN symbols s ON c.target_kind = 'symbol' AND s.id = c.target_id
WHERE chunk_fts MATCH ?`+where+`
ORDER BY bm25(chunk_fts), c.id LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("full-text search: %w", err)
	}
	defer rows.Close()
	var chunks []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.score); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunksToTargets(ctx, db, chunks)
}

// chunksToTargets maps chunk ids to (symbol|file, id) candidates keeping order.
func chunksToTargets(ctx context.Context, db *sql.DB, chunks []cand) ([]cand, error) {
	out := make([]cand, 0, len(chunks))
	seen := map[string]bool{}
	for _, c := range chunks {
		var tk string
		var tid int64
		if err := db.QueryRowContext(ctx, `SELECT target_kind, target_id FROM chunks WHERE id=?`, c.id).Scan(&tk, &tid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		t := cand{kind: tk, id: tid, score: c.score}
		if tk != "symbol" && tk != "file" {
			continue // module-level summaries are not produced yet
		}
		if seen[t.key()] {
			continue
		}
		seen[t.key()] = true
		out = append(out, t)
	}
	return out, nil
}

// vectorLeg embeds q and runs a KNN over chunk_vec; kind and lang filters run
// inside the KNN through the metadata columns, the path filter afterwards.
func vectorLeg(ctx context.Context, db *sql.DB, e provider.Embedder, q string, n int, o Opts) ([]cand, error) {
	v, err := embed.Query(ctx, e, q)
	if err != nil {
		return nil, err
	}
	knn := n
	if o.Path != "" {
		knn = min(knn*5, 1000)
	}
	sqlText := `SELECT rowid, distance FROM chunk_vec WHERE embedding MATCH ? AND k = ?`
	args := []any{embed.VecBytes(v), knn}
	if o.Kind != "" {
		sqlText += ` AND kind = ?`
		args = append(args, store.MetaText(strings.ToLower(o.Kind)))
	}
	if o.Lang != "" {
		sqlText += ` AND lang = ?`
		args = append(args, store.MetaText(o.Lang))
	}
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()
	var chunks []cand
	for rows.Next() {
		var c cand
		var d float64
		if err := rows.Scan(&c.id, &d); err != nil {
			return nil, err
		}
		c.score = 1 - d
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunksToTargets(ctx, db, chunks)
}

// chunkFTSLeg and nameLeg already apply every filter in SQL; the vector leg's
// path filter is applied here, and stale markers are computed.
func hydrate(ctx context.Context, db *sql.DB, ranked []cand, o Opts, k int) ([]Hit, error) {
	var hits []Hit
	for _, c := range ranked {
		if len(hits) >= k {
			break
		}
		var h Hit
		var mtime, size int64
		var sha string
		var sig, doc sql.NullString
		var err error
		switch c.kind {
		case "symbol":
			h.TargetKind = "symbol"
			h.SymbolID = c.id
			err = db.QueryRowContext(ctx, `SELECT s.name, s.qualified_name, s.kind, s.start_line, s.signature, s.doc,
				f.id, f.path, f.lang, COALESCE(f.mtime,0), COALESCE(f.size,0), f.sha256
				FROM symbols s JOIN files f ON f.id = s.file_id WHERE s.id = ?`, c.id).
				Scan(&h.Name, &h.Qualified, &h.Kind, &h.Line, &sig, &doc, &h.FileID, &h.Path, &h.Lang, &mtime, &size, &sha)
			h.Line++
		case "file":
			h.TargetKind = "file"
			h.Kind = "file"
			h.Line = 1
			err = db.QueryRowContext(ctx, `SELECT id, path, lang, COALESCE(mtime,0), COALESCE(size,0), sha256 FROM files WHERE id = ?`, c.id).
				Scan(&h.FileID, &h.Path, &h.Lang, &mtime, &size, &sha)
			h.Name, h.Qualified = h.Path, h.Path
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if o.Path != "" && !strings.Contains(h.Path, o.Path) {
			continue
		}
		if o.Kind != "" && !strings.EqualFold(h.Kind, o.Kind) {
			continue
		}
		if o.Lang != "" && h.Lang != o.Lang {
			continue
		}
		h.Signature = sig.String
		h.Score = c.score
		var sum sql.NullString
		_ = db.QueryRowContext(ctx, `SELECT text FROM summaries WHERE target_kind=? AND target_id=? AND status='ok'`, h.TargetKind, c.id).Scan(&sum)
		if sum.Valid {
			h.Summary = firstLine(sum.String)
		}
		if o.Root != "" {
			h.Stale = Staleness(o.Root, h.Path, mtime, size, sha)
		}
		hits = append(hits, h)
	}
	return hits, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Staleness compares the file on disk with what was indexed: "deleted" when it
// is gone, "modified" when its content hash differs, "" otherwise. The hash is
// only computed when size or mtime differ.
func Staleness(root, rel string, mtime, size int64, sha string) string {
	p := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(p)
	if err != nil {
		return "deleted"
	}
	if info.Size() == size && info.ModTime().UnixNano() == mtime {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "deleted"
	}
	if hashHex(data) != sha {
		return "modified"
	}
	return ""
}
