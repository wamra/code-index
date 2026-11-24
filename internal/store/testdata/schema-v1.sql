CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT
);

CREATE TABLE IF NOT EXISTS files (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,      -- relative to the workspace root, '/' separators
  lang TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  mtime INTEGER,                  -- unix nanoseconds
  size INTEGER,
  indexed_at TEXT,
  status TEXT NOT NULL DEFAULT 'ok',  -- ok | failed
  fail_reason TEXT
);

CREATE TABLE IF NOT EXISTS symbols (
  id INTEGER PRIMARY KEY,
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  qualified_name TEXT NOT NULL,
  kind TEXT NOT NULL,             -- lowercase LSP SymbolKind name; 'alias' is reserved for milestone 2
  parent_id INTEGER REFERENCES symbols(id) ON DELETE CASCADE,
  start_line INTEGER, start_col INTEGER,
  end_line INTEGER, end_col INTEGER,
  sel_line INTEGER, sel_col INTEGER,
  signature TEXT,
  doc TEXT,
  body_sha256 TEXT,
  source TEXT NOT NULL DEFAULT 'lsp'
);
CREATE INDEX IF NOT EXISTS symbols_file ON symbols(file_id);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS symbols_qname ON symbols(qualified_name);

-- Milestone 2 fills edges; the table exists now so later milestones need no migration.
CREATE TABLE IF NOT EXISTS edges (
  id INTEGER PRIMARY KEY,
  src_symbol_id INTEGER REFERENCES symbols(id) ON DELETE CASCADE,
  dst_symbol_id INTEGER REFERENCES symbols(id) ON DELETE SET NULL,
  dst_name TEXT,
  kind TEXT NOT NULL,             -- calls | references | alias_of | imports | dot_sources
  file_id INTEGER REFERENCES files(id) ON DELETE CASCADE,
  line INTEGER, col INTEGER,
  source TEXT NOT NULL,           -- lsp | ast | lsp+ast
  confidence TEXT NOT NULL        -- exact | alias | dynamic | unresolved
);
CREATE INDEX IF NOT EXISTS edges_src ON edges(src_symbol_id);
CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst_symbol_id);

-- Milestone 3 fills summaries.
CREATE TABLE IF NOT EXISTS summaries (
  id INTEGER PRIMARY KEY,
  target_kind TEXT NOT NULL,      -- symbol | file | module
  target_id INTEGER NOT NULL,
  body_sha256 TEXT,
  model TEXT,
  text TEXT,
  status TEXT NOT NULL DEFAULT 'ok',
  fail_reason TEXT,
  UNIQUE(target_kind, target_id)
);

CREATE TABLE IF NOT EXISTS chunks (
  id INTEGER PRIMARY KEY,
  target_kind TEXT NOT NULL,      -- symbol | file | module
  target_id INTEGER NOT NULL,
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  text TEXT NOT NULL,
  text_sha256 TEXT NOT NULL,
  embed_fingerprint TEXT,
  embedded_at TEXT                -- NULL = pending, 'failed' = parked
);
CREATE INDEX IF NOT EXISTS chunks_target ON chunks(target_kind, target_id);
CREATE INDEX IF NOT EXISTS chunks_file ON chunks(file_id);
CREATE INDEX IF NOT EXISTS chunks_pending ON chunks(id) WHERE embedded_at IS NULL;

-- chunk_vec is created by store.EnsureVec once the embedding size is known.

-- Both FTS tables use the chunk / symbol id as rowid.
CREATE VIRTUAL TABLE IF NOT EXISTS chunk_fts USING fts5(
  text,
  tokenize='unicode61'
);

CREATE VIRTUAL TABLE IF NOT EXISTS name_fts USING fts5(
  name,
  qualified_name,
  path,
  tokenize='trigram'
);
