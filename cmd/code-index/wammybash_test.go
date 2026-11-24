package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/extractest"
)

const fixtureBashDir = "../../testdata/fixture-bash-wammybash"

// bashEnv copies the bash corpus into a workspace and writes a config whose
// only language is bash, served by one exec extractor entry running argv with
// extra environment env. It returns the workspace root.
func bashEnv(t *testing.T, argv []string, env map[string]string) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "ws")
	filepath.WalkDir(fixtureBashDir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(fixtureBashDir, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(root, rel), 0o755)
		}
		b, _ := os.ReadFile(p)
		return os.WriteFile(filepath.Join(root, rel), b, 0o644)
	})
	root, _ = filepath.EvalSymlinks(root)
	ex := map[string]any{"type": "exec", "argv": argv, "symbols": true}
	if len(env) > 0 {
		ex["env"] = env
	}
	cfg := map[string]any{
		"languages":  map[string]any{"bash": map[string]any{"extensions": []string{".sh", ".bash"}, "extractor": "wammybash"}},
		"extractors": map[string]any{"wammybash": ex},
	}
	b, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(tmp, "config.json")
	os.WriteFile(cfgPath, b, 0o644)
	t.Setenv("CODE_INDEX_CONFIG", cfgPath)
	t.Setenv("CODE_INDEX_DB", "")
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("LocalAppData", filepath.Join(tmp, "localappdata"))
	t.Chdir(root)
	return root
}

// checkBashGraph asserts what `callers` prints for the corpus once indexed by
// a wammybash-compatible extractor (the real one or its recorded output).
func checkBashGraph(t *testing.T, root string) {
	t.Helper()
	// the bare name is ambiguous: two functions called build_release
	code, _, errw := cli(t, "callers", "build_release")
	if code == 0 || !strings.Contains(errw, "ambiguous (2 candidates)") || !strings.Contains(errw, "lib/util.sh:4") || !strings.Contains(errw, "orphan.sh:2") {
		t.Errorf("callers build_release: %d %q", code, errw)
	}
	got := mustOK(t, "callers", "lib/util.sh:4")
	for _, want := range []string{
		"dyn.sh:4  dyn.sh (top level)  [ast exact direct]\n",
		"main.sh:6  deploy  [ast exact direct]\n",
		"main.sh:7  deploy (via build_release)  [ast alias alias]\n",
		"main.sh:9  deploy  [ast candidate variable-constant]\n",
		"main.sh:10  deploy  [ast candidate string-eval]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("callers lib/util.sh:4 lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "orphan.sh") {
		t.Errorf("orphan.sh must not appear among the callers of lib/util.sh's build_release:\n%s", got)
	}
	if got := mustOK(t, "callers", "orphan.sh:2"); got != "orphan.sh:3  orphan.sh (top level)  [ast exact direct]\n" {
		t.Errorf("callers orphan.sh:2:\n%s", got)
	}
	var js struct {
		Edges []struct {
			Path, From, Source, Confidence, Reason string
			Line                                   int
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(mustOK(t, "callers", "lib/util.sh:4", "--json")), &js); err != nil {
		t.Fatal(err)
	}
	reasons := map[int]string{}
	for _, e := range js.Edges {
		if e.Path == "main.sh" {
			reasons[e.Line] = e.Confidence + "/" + e.Reason
		}
	}
	for line, want := range map[int]string{6: "exact/direct", 7: "alias/alias", 9: "candidate/variable-constant", 10: "candidate/string-eval"} {
		if reasons[line] != want {
			t.Errorf("json main.sh:%d = %q, want %q", line, reasons[line], want)
		}
	}
}

// The CLI end to end with a bash language that has only an exec extractor,
// served by the recorded wammybash output (the test binary plays the tool).
func TestBashExecExtractorRecordedOutput(t *testing.T) {
	golden, err := filepath.Abs("../../internal/extract/testdata/wammybash-extract.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	root := bashEnv(t, []string{os.Args[0], "-test.run=^$", "--root", "{root}", "--list-file", "{listFile}"},
		map[string]string{extractest.EnvServe: "1", extractest.EnvGolden: golden})
	out := mustOK(t, "index", root, "--quiet")
	if !strings.Contains(out, "9 file(s) of configured languages; indexed 9") || !strings.Contains(out, "failed 0") || !strings.Contains(out, "edges:") {
		t.Fatalf("index summary: %s", out)
	}
	checkBashGraph(t, root)

	// doctor names the exec extractor and its command
	doc := mustOK(t, "doctor")
	if !strings.Contains(doc, "language bash") || !strings.Contains(doc, "wammybash (exec): ") || strings.Contains(doc, "FAIL") {
		t.Errorf("doctor:\n%s", doc)
	}
}

// A missing command is reported by doctor with the extractor's name.
func TestDoctorReportsMissingExecExtractor(t *testing.T) {
	bashEnv(t, []string{"definitely-not-installed-extractor", "{root}"}, nil)
	code, out, _ := cli(t, "doctor", "--no-index")
	if code != 0 || !strings.Contains(out, "WARN  language bash") || !strings.Contains(out, "wammybash (exec)") || !strings.Contains(out, "definitely-not-installed-extractor") {
		t.Errorf("doctor --no-index: %d\n%s", code, out)
	}
	code, out, errw := cli(t, "index", "--quiet")
	if code == 0 || !strings.Contains(errw, "bash") {
		t.Errorf("index with a missing extractor: %d %q %q", code, out, errw)
	}
}

// With CODE_INDEX_REAL_WAMMYBASH=/path/to/wammybash the real tool indexes the
// corpus and the same graph comes out.
func TestRealWammybash(t *testing.T) {
	bin := os.Getenv("CODE_INDEX_REAL_WAMMYBASH")
	if bin == "" {
		t.Skip("set CODE_INDEX_REAL_WAMMYBASH=/path/to/wammybash to run the real tool")
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("CODE_INDEX_REAL_WAMMYBASH=%s: %v", bin, err)
	}
	root := bashEnv(t, []string{bin, "extract", "--root", "{root}", "--list-file", "{listFile}"}, nil)
	out := mustOK(t, "index", root, "--quiet")
	if !strings.Contains(out, "indexed 9") || !strings.Contains(out, "failed 0") {
		t.Fatalf("index summary: %s", out)
	}
	checkBashGraph(t, root)
}
