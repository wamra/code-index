import type { EngineInterface, Register } from 'claude-code'

const DEFAULT_BIN = 'code-index'

const SEARCH = 'mcp__code-index-search__search'
const SHOW = 'mcp__code-index-search__show'
const MAX_TEXT = 12000

type Hit = {
  target_kind?: string
  name?: string
  qualified_name?: string
  kind?: string
  lang?: string
  path?: string
  line?: number
  signature?: string
  doc?: string
  score?: number
  stale?: boolean
}

type SearchInput = {
  query: string
  mode?: 'auto' | 'vector' | 'fts' | 'hybrid'
  kind?: string
  lang?: string
  path?: string
  k?: number
  repo?: string
}

type ShowInput = { symbol: string; source?: boolean; repo?: string }

const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)}\n[cut: ${s.length - n} more characters]` : s)

const one = (s: string | undefined, n: number) => (s ?? '').replace(/\s+/g, ' ').trim().slice(0, n)

// Flags go first and the query after "--", so a query that starts with a dash
// is never read as a flag.
function searchArgv(i: SearchInput): string[] {
  const argv = ['search', '--json', '--k', String(Math.min(Math.max(i.k ?? 10, 1), 30))]
  if (i.mode) argv.push('--mode', i.mode)
  if (i.kind) argv.push('--kind', i.kind)
  if (i.lang) argv.push('--lang', i.lang)
  if (i.path) argv.push('--path', i.path)
  argv.push('--', i.query)
  return argv
}

function formatHits(stdout: string): string {
  let parsed: { hits?: Hit[]; route?: string }
  try {
    parsed = JSON.parse(stdout)
  } catch {
    return clip(stdout.trim() || 'No output from code-index.', MAX_TEXT)
  }
  const hits = parsed.hits ?? []
  if (hits.length === 0) return 'No matching symbols or files.'
  const lines = hits.map((h, n) => {
    const where = `${h.path ?? '?'}${h.line ? `:${h.line}` : ''}`
    const what = h.target_kind === 'file' ? 'file' : `${h.kind ?? 'symbol'} ${h.qualified_name || h.name || '?'}`
    const rows = [`${n + 1}. ${what}  ${where}  score: ${(h.score ?? 0).toFixed(3)}${h.stale ? '  [file changed since indexing]' : ''}`]
    if (h.signature) rows.push(`   ${one(h.signature, 200)}`)
    if (h.doc) rows.push(`   ${one(h.doc, 200)}`)
    return rows.join('\n')
  })
  const route = parsed.route ? `route: ${parsed.route}\n` : ''
  return `${route}${lines.join('\n')}\n\nRead one with the show tool (qualified name, name or path:line). Paths are relative to the repo root.`
}

// Without a "binary" option the plugin runs its bundled launcher (bin/ next to plugin.json) and
// falls back to code-index on PATH when the launcher is absent (sh exits 127).
async function run($: EngineInterface, bin: string, argv: string[], repo?: string, timeoutMs = 90_000) {
  try {
    const opts = { timeoutMs, ...(repo ? { cwd: repo } : {}) }
    let r = await $.process.run(bin === DEFAULT_BIN ? ['sh', `${$.plugin.root}/bin/code-index`, ...argv] : [bin, ...argv], opts)
    if (bin === DEFAULT_BIN && r.exitCode === 127) r = await $.process.run([bin, ...argv], opts)
    if (r.exitCode !== 0) return { ok: false as const, text: clip(`code-index exited ${r.exitCode}: ${r.stderr.trim() || r.stdout.trim()}`, MAX_TEXT) }
    return { ok: true as const, text: r.stdout }
  } catch (err) {
    return { ok: false as const, text: `Could not run '${bin}': ${String(err)}. Set the code-index-search "binary" option to the executable's full path.` }
  }
}

const REPO = { type: 'string', description: 'Absolute path inside the repo to search. Default: the session directory.' } as const

export const register: Register = (on, options) => {
  const bin = String((options as { binary?: string } | undefined)?.binary || DEFAULT_BIN)

  on('session.start', async ($, e, next) => {
    await $.tool.register({
      name: 'search',
      description:
        "Search the code index of the current repo (<repo>/.Wammy/code.sqlite): functions, classes, methods and files with their signatures and doc comments. Use it to find where something is defined or which code does a thing, before grepping. Default mode routes identifiers and paths to name search and plain-language questions to semantic search. If the repo has no index, the result says so: run 'code-index index' in the repo with Bash, then search again.",
      inputSchema: {
        type: 'object',
        properties: {
          query: { type: 'string', description: 'An identifier, a path fragment, or a plain-language description of the code.' },
          mode: { type: 'string', enum: ['auto', 'vector', 'fts', 'hybrid'], description: "Default 'auto'." },
          kind: { type: 'string', description: "Only this kind: 'function', 'class', 'method', ... or 'file'." },
          lang: { type: 'string', description: "Only this language, as named in the config, for example 'powershell'." },
          path: { type: 'string', description: 'Only files whose path contains this text.' },
          k: { type: 'number', description: 'Hits to return, 1 to 30 (default 10).' },
          repo: REPO,
        },
        required: ['query'],
      },
      isDeferred: false,
    })
    await $.tool.register({
      name: 'show',
      description:
        "Show one indexed symbol found by the search tool: kind, file and line range, signature, doc comment and children. Pass a qualified name, a name, or path:line. Set source to also get the symbol's source lines.",
      inputSchema: {
        type: 'object',
        properties: {
          symbol: { type: 'string', description: 'Qualified name, name, or path:line.' },
          source: { type: 'boolean', description: "Also return the symbol's source lines from disk." },
          repo: REPO,
        },
        required: ['symbol'],
      },
      isDeferred: false,
    })
    await $.command.register({
      name: 'code',
      description: 'Search the code index: /code <query>, /code show <symbol>, /code index to (re)index this repo, or /code alone for index stats',
      argumentHint: '[query | show <symbol> | index]',
    })
    return next(e)
  })

  on('tool.call', { tool: SEARCH }, async ($, e) => {
    const i = e as unknown as SearchInput
    if (typeof i.query !== 'string' || i.query.trim() === '') return { deny: 'query is required.' }
    const r = await run($, bin, searchArgv(i), i.repo)
    return { result: r.ok ? formatHits(r.text) : r.text, ...(r.ok ? {} : { isError: true as const }) } as never
  }).catch(() => ({ deny: 'code-index-search failed unexpectedly; try the code-index command in Bash.' }))

  on('tool.call', { tool: SHOW }, async ($, e) => {
    const i = e as unknown as ShowInput
    if (typeof i.symbol !== 'string' || i.symbol.trim() === '') return { deny: 'symbol is required.' }
    const r = await run($, bin, ['show', ...(i.source ? ['--source'] : []), '--', i.symbol], i.repo)
    return { result: clip(r.text.trim() || 'No such symbol.', MAX_TEXT), ...(r.ok ? {} : { isError: true as const }) } as never
  }).catch(() => ({ deny: 'code-index-search failed unexpectedly; try the code-index command in Bash.' }))

  on('command.run', { command: 'code' }, async ($, e) => {
    const args = e.args.trim()
    if (args === '') {
      const r = await run($, bin, ['stats'])
      return { text: clip(r.text.trim() || 'No output.', MAX_TEXT) }
    }
    if (args === 'index') {
      // --embed is a no-op with a note when no embedder is configured.
      const r = await run($, bin, ['index', '--embed'], undefined, 600_000)
      return { text: clip(r.text.trim() || 'No output.', MAX_TEXT) }
    }
    const m = /^show\s+(.+)$/.exec(args)
    if (m) {
      const r = await run($, bin, ['show', '--', m[1] ?? ''])
      return { text: clip(r.text.trim() || 'No such symbol.', MAX_TEXT) }
    }
    const r = await run($, bin, searchArgv({ query: args }))
    return { text: r.ok ? formatHits(r.text) : r.text }
  })
}
