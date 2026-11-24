package extract

// Noter is implemented by a Session that dropped part of what its extractor
// printed and wants the indexer to say so. The indexer calls Notes after the
// session's last phase and adds each line to the run's notes as
// "<lang>: <line>". The exec session reports non-JSON output lines this way:
// "ignored N non-JSON line(s) of extractor output".
type Noter interface {
	Notes() []string
}

// Describer is an optional interface of a Session: Describe returns what the
// run log shows after the language name when the session is ready, such as a
// language server's name and version. Sessions without it show the extractor's
// name.
type Describer interface {
	Describe() string
}
