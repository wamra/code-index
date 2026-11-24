package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/extract"
	"github.com/wamra/code-index/internal/store"
)

// liveSession is a started extractor of one language in one run.
type liveSession struct {
	ne   config.NamedExtractor
	sess extract.Session
	prov extract.Provides
}

func closeSessions(live []*liveSession) {
	for _, ls := range live {
		_ = ls.sess.Close()
	}
}

// workItem is a changed file that is to be indexed.
type workItem struct {
	c    candidate
	data []byte
	sha  string
}

// startSessions builds the extractors nes of lang (extract.New, in config
// order) and starts each once; the sessions serve both phases of the run and
// the caller closes them. needSymbols says the language has files to index,
// needEdges that its edges are rebuilt (when they are not, no extractor is
// started after the first one that provides symbols).
//
// A command that is not installed (ErrExtractorUnavailable) and any other
// start failure leave that extractor out with a note. The returned error is
// set when the language cannot be used: an extractor cannot be built, a start
// fails while files need indexing, or files need indexing and no started
// session provides symbols. Sessions started before the error are returned
// (the caller closes them).
func startSessions(ctx context.Context, o Options, root, lang string, nes []config.NamedExtractor, needSymbols, needEdges bool) (live []*liveSession, notes []string, err error) {
	if len(nes) == 0 {
		return nil, nil, fmt.Errorf("no extractor configured for language %q", lang)
	}
	env := extract.Env{Root: root, OpenLSP: o.Open, Exec: o.Extract, EmptyRetryDelays: o.EmptyRetryDelays, Logf: o.logf}
	var problems []string // why extractors were left out, for the language error
	haveSymbols := false
	for _, ne := range nes {
		if haveSymbols && !needEdges {
			break
		}
		ex, err := extract.New(lang, ne.Name, ne.Extractor)
		if err != nil {
			return live, notes, err
		}
		sess, err := ex.Start(ctx, env)
		if err != nil {
			if ctx.Err() != nil {
				return live, notes, ctx.Err()
			}
			unavailable := errors.Is(err, extract.ErrExtractorUnavailable)
			if !unavailable && needSymbols {
				return live, notes, err
			}
			problems = append(problems, err.Error())
			switch {
			case ne.Type == config.ExtractorLSP:
				notes = append(notes, fmt.Sprintf("%s: no language server (%v); LSP edges skipped", lang, err))
			case unavailable:
				notes = append(notes, fmt.Sprintf("%s: %v; skipping the AST extractor %s", lang, err, ne.Name))
			default:
				notes = append(notes, fmt.Sprintf("%s: extractor %s: %v; skipped", lang, ne.Name, err))
			}
			continue
		}
		ls := &liveSession{ne: ne, sess: sess, prov: sess.Provides()}
		live = append(live, ls)
		if ls.prov.Symbols {
			haveSymbols = true
		}
	}
	if needSymbols && !haveSymbols {
		for _, ls := range live {
			if ls.ne.Type == config.ExtractorLSP {
				problems = append(problems, fmt.Sprintf("%s does not advertise documentSymbol", ls.ne.Argv[0]))
			} else {
				problems = append(problems, fmt.Sprintf("extractor %s does not provide symbols", ls.ne.Name))
			}
		}
		return live, notes, errors.New(strings.Join(problems, "; "))
	}
	return live, notes, nil
}

// symbolSession is the first session, in run order, that provides symbols.
func symbolSession(live []*liveSession) *liveSession {
	for _, ls := range live {
		if ls.prov.Symbols {
			return ls
		}
	}
	return nil
}

// hasExtension reports whether rel's extension (case-insensitive) is in exts.
func hasExtension(exts []string, rel string) bool {
	e := path.Ext(rel)
	for _, x := range exts {
		if strings.EqualFold(x, e) {
			return true
		}
	}
	return false
}

// indexItems is phase 1: the changed files go to the symbols session sym, and
// each file it emits is committed at once (indexFromRecords). An error record
// fails the file; a file the session never emitted fails with "extractor
// returned nothing for the file"; an error returned by the session fails the
// files not yet emitted and marks the language unusable for this run. A file
// outside the session's extensions is stored without symbols. A cancelled
// context stops quietly: the files not reached stay as they were.
func indexItems(ctx context.Context, st *store.Store, o Options, lang string, sym *liveSession, items []workItem, existing map[string]fileRow, hasVec bool, res *Result) {
	byAbs := make(map[string]*workItem, len(items))
	done := map[string]bool{}
	var give []string
	fail := func(w *workItem, cause error) {
		_ = recordFailure(ctx, st, w.c, w.sha, existing[w.c.rel].id, hasVec, cause)
		res.Failed++
		res.FileErrors = append(res.FileErrors, w.c.rel+": "+cause.Error())
		o.logf("  failed %s: %v", w.c.rel, cause)
	}
	commit := func(w *workItem, recs []extract.Record) error {
		fs, err := indexFromRecords(ctx, st, w.c, w.data, w.sha, existing[w.c.rel].id, hasVec, recs)
		if err != nil {
			fail(w, err)
			return err
		}
		res.Indexed++
		res.Symbols += fs.symbols
		res.Chunks += fs.chunks
		o.logf("  %s: %d symbol(s)", w.c.rel, fs.symbols)
		return nil
	}
	for i := range items {
		w := &items[i]
		byAbs[w.c.abs] = w
		if hasExtension(sym.ne.Extensions, w.c.rel) {
			give = append(give, w.c.abs)
			continue
		}
		done[w.c.abs] = true
		if ctx.Err() != nil {
			return
		}
		if err := commit(w, nil); err != nil {
			res.LangErrors[lang] = err.Error()
			return
		}
	}
	emit := func(file string, recs []extract.Record) error {
		w := byAbs[file]
		if w == nil || done[file] {
			return nil
		}
		done[file] = true
		for _, r := range recs {
			if r.T == extract.TypeError {
				fail(w, errors.New(r.Message))
				return nil
			}
		}
		return commit(w, recs)
	}
	err := sym.sess.Symbols(ctx, give, emit)
	if ctx.Err() != nil {
		return
	}
	cause := errors.New("extractor returned nothing for the file")
	if err != nil {
		res.LangErrors[lang] = err.Error()
		cause = err
	}
	for i := range items {
		if w := &items[i]; !done[w.c.abs] {
			fail(w, cause)
		}
	}
}
