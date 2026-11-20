import { expect, test } from 'claude-code/testing'

const HITS = JSON.stringify({
  hits: [
    {
      target_kind: 'symbol',
      name: 'Get-Widget',
      qualified_name: 'Get-Widget',
      kind: 'function',
      lang: 'powershell',
      path: 'Widgets/Widgets.psm1',
      line: 6,
      signature: 'function Get-Widget ()',
      score: 3.19,
    },
  ],
  route: 'name',
})

const OK = (stdout: string) => ({ value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })

test('the search tool runs the binary with --json, puts the query after -- and formats hits', async ($, on) => {
  let seen: readonly string[] = []
  let cwd: string | undefined
  on('process.run', (_, e) => {
    seen = e.argv
    cwd = e.init?.cwd
    return OK(HITS)
  })
  on('tool.register', () => ({ value: {} }))
  on('command.register', () => ({ value: { command: 'code' } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })

  const out = await $.tool.call({ tool: 'mcp__code-index-search__search', query: '-Name widget', kind: 'function', k: 3, repo: '/work/repo' })
  expect(seen[0]).toBe('sh')
  expect(seen[1]).toBe('-c')
  expect(seen[3]).toMatch(/\/bin\/code-index$/)
  expect(seen.slice(4)).toEqual(['search', '--json', '--k', '3', '--kind', 'function', '--', '-Name widget'])
  expect(cwd).toBe('/work/repo')
  const text = String((out as { result?: unknown }).result)
  expect(text).toContain('function Get-Widget  Widgets/Widgets.psm1:6')
  expect(text).toContain('route: name')
})

test('the show tool passes --source and the symbol after --', async ($, on) => {
  let seen: readonly string[] = []
  on('process.run', (_, e) => {
    seen = e.argv
    return OK('function Get-Widget\n  file: Widgets/Widgets.psm1:6-16')
  })
  on('tool.register', () => ({ value: {} }))
  on('command.register', () => ({ value: { command: 'code' } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })

  const out = await $.tool.call({ tool: 'mcp__code-index-search__show', symbol: 'lib.ps1:5', source: true })
  expect(seen[0]).toBe('sh')
  expect(seen.slice(4)).toEqual(['show', '--source', '--', 'lib.ps1:5'])
  expect(String((out as { result?: unknown }).result)).toContain('Widgets/Widgets.psm1:6-16')
})

test('an empty query is refused and a failing binary is an error result', async ($, on) => {
  on('process.run', () => ({ value: { exitCode: 1, stdout: '', stderr: 'search: no index for /work/repo; run `code-index index`', isStdoutTruncated: false, isStderrTruncated: false } }))
  on('tool.register', () => ({ value: {} }))
  on('command.register', () => ({ value: { command: 'code' } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })

  const empty = await $.tool.call({ tool: 'mcp__code-index-search__search', query: ' ' })
  expect((empty as { deny?: string }).deny).toBeDefined()
  const bad = await $.tool.call({ tool: 'mcp__code-index-search__search', query: 'x' })
  expect(JSON.stringify(bad)).toContain('no index')
})

test('without the bundled launcher (sh exits 127) it falls back to code-index on PATH', async ($, on) => {
  const calls: string[] = []
  on('process.run', (_, e) => {
    calls.push(e.argv[0])
    return e.argv[0] === 'sh'
      ? { value: { exitCode: 127, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
      : OK('function Get-Widget\n  file: Widgets/Widgets.psm1:6-16')
  })
  on('tool.register', () => ({ value: {} }))
  on('command.register', () => ({ value: { command: 'code' } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })

  const out = await $.tool.call({ tool: 'mcp__code-index-search__show', symbol: 'lib.ps1:5' })
  expect(calls).toEqual(['sh', 'code-index'])
  expect(String((out as { result?: unknown }).result)).toContain('Widgets/Widgets.psm1:6-16')
})
