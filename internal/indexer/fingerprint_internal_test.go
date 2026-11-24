package indexer

import (
	"context"
	"testing"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/extract"
)

// The fingerprint takes each extractor's Version through the built extractor
// (extractors/README.md section 5): the built-in powershell-ast has no
// configured version, its version is the hash of its embedded script.
func TestLangFingerprintUsesExtractorVersion(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{Languages: map[string]config.Language{
		"ps": {Extensions: []string{".ps1"}, Extractor: "powershell-ast"},
	}}
	nes := cfg.ExtractorsFor("ps")
	if len(nes) != 1 {
		t.Fatalf("extractors = %v", nes)
	}
	ex, err := extract.New("ps", nes[0].Name, nes[0].Extractor)
	if err != nil {
		t.Fatal(err)
	}
	v := ex.Version(ctx)
	if v == "" {
		t.Fatal("the built-in extractor must have a version (hash of the embedded script)")
	}
	argv, err := extract.IdentityArgv(nes[0].Argv, "/w")
	if err != nil {
		t.Fatal(err)
	}
	want := extract.Fingerprint([]extract.Part{{Name: "powershell-ast", Type: config.ExtractorExec, Argv: argv, Version: v}})
	if got := langFingerprint(ctx, cfg, "ps", "/w"); got != want {
		t.Errorf("fingerprint = %s, want %s (built from the extractor's Version)", got, want)
	}
}

// An extractor whose identity cannot be computed must not be fingerprinted as
// if its raw argv were fine: validation rejects such a config, so this is a
// failsafe, and the value must differ from the raw-argv one and from a good one.
func TestLangFingerprintErrorIsNotSwallowed(t *testing.T) {
	ctx := context.Background()
	cfgWith := func(argv ...string) config.Config {
		return config.Config{
			Languages:  map[string]config.Language{"x": {Extensions: []string{".x"}, Extractor: "ext"}},
			Extractors: map[string]config.Extractor{"ext": {Type: config.ExtractorExec, Argv: argv}},
		}
	}
	bad := cfgWith("tool", "{embedded:nope.ps1}")
	raw := extract.Fingerprint([]extract.Part{{Name: "ext", Type: config.ExtractorExec, Argv: []string{"tool", "{embedded:nope.ps1}"}}})
	got := langFingerprint(ctx, bad, "x", "/w")
	if got == raw {
		t.Error("an IdentityArgv error fell back to the raw argv fingerprint")
	}
	if good := langFingerprint(ctx, cfgWith("tool", "{root}"), "x", "/w"); good == got {
		t.Error("a broken extractor has the fingerprint of a good one")
	}
	if other := langFingerprint(ctx, cfgWith("tool", "{embedded:other.ps1}"), "x", "/w"); other == got {
		t.Error("two different errors give the same fingerprint")
	}
	if again := langFingerprint(ctx, bad, "x", "/w"); again != got {
		t.Error("the fingerprint of a broken extractor is not stable")
	}
	// an extractor that cannot even be built (empty argv) is also marked
	if g := langFingerprint(ctx, cfgWith(""), "x", "/w"); g == extract.Fingerprint([]extract.Part{{Name: "ext", Type: config.ExtractorExec, Argv: []string{""}}}) {
		t.Error("a build error fell back to the raw argv fingerprint")
	}
}
