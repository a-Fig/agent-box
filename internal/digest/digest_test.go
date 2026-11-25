package digest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// go.json is testdata/gofail's go test -json report from a box (Go 1.27):
// an assertion with a second line, a failed subtest, a panic, a skip and a
// package that doesn't build.
const wantGo = `FAIL: 3 of 6 tests failed, 1 skipped (go test)
build failed: example.com/gofail/broken
  broken/broken_test.go:5:33: undefined: undefinedThing
example.com/gofail
  TestAssert  fail_test.go:12: queue length: expected 2 got 3
      extra detail line
  TestSub/bad  fail_test.go:18: subtest broke
  TestPanic  internal/digest/testdata/gofail/fail_test.go:23: panic: assignment to entry in nil map
`

func TestGo(t *testing.T) {
	if got := Go(read(t, "go.json")); got != wantGo {
		t.Fatalf("got:\n%s\nwant:\n%s", got, wantGo)
	}
}

func TestGoPass(t *testing.T) {
	report := `{"Action":"pass","Package":"p","Test":"TestA"}
{"Action":"skip","Package":"p","Test":"TestB"}
{"Action":"output","Package":"p","Output":"ok  \tp\t0.1s\n"}
{"Action":"pass","Package":"p"}
`
	if got, want := Go(report), "PASS: 1 tests passed, 1 skipped (go test)\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := Go("not json\n"); got != "" {
		t.Fatalf("no events gave %q", got)
	}
}

// A package can fail with every test passing: TestMain, a timeout.
func TestGoPackageFailure(t *testing.T) {
	report := `{"Action":"pass","Package":"p","Test":"TestA"}
{"Action":"output","Package":"p","Output":"panic: test timed out after 1s\n"}
{"Action":"output","Package":"p","Output":"FAIL\tp\t1.0s\n"}
{"Action":"fail","Package":"p"}
`
	want := "FAIL: 1 tests passed, but packages failed (go test)\nFAIL p\n  panic: test timed out after 1s\n"
	if got := Go(report); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Many failures stay within MaxBytes and say how many were left out.
func TestGoCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		n := "TestN" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
		b.WriteString(`{"Action":"output","Package":"p","Test":"` + n + `","Output":"    a_test.go:9: ` + strings.Repeat("long message ", 30) + `\n"}` + "\n")
		b.WriteString(`{"Action":"fail","Package":"p","Test":"` + n + `"}` + "\n")
	}
	got := Go(b.String())
	if len(got) > MaxBytes {
		t.Fatalf("digest is %d bytes", len(got))
	}
	if !strings.Contains(got, "more failures; the full log has them") {
		t.Fatalf("no count of what was left out:\n%s", got)
	}
}

const wantVitest = `FAIL: 1 of 3 tests failed, 1 skipped, 1 test files failed outside their tests (vitest)
internal/digest/testdata/vitestfail/broken.test.js: Cannot find module './missing.js' imported from '/work/agent-box/internal/digest/testdata/vitestfail/broken.test.js'
internal/digest/testdata/vitestfail/math.test.js
  math multiplies  internal/digest/testdata/vitestfail/math.test.js:8: AssertionError: expected 4 to be 5 // Object.is equality
`

// vitest.json is testdata/vitestfail's report from a box (vitest 3.2.4).
func TestVitest(t *testing.T) {
	if got := Vitest(read(t, "vitest.json")); got != wantVitest {
		t.Fatalf("got:\n%s\nwant:\n%s", got, wantVitest)
	}
	pass := `{"numTotalTests":4,"numPassedTests":4,"numFailedTests":0,"numPendingTests":0,"numTodoTests":0,"testResults":[]}`
	if got, want := Vitest(pass), "PASS: 4 tests passed (vitest)\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParse(t *testing.T) {
	out := goHead + "\n" + `{"Action":"pass","Package":"p","Test":"TestA"}` + "\n" + vitestHead + "\n" +
		`{"numTotalTests":1,"numPassedTests":1,"testResults":[]}` + "\n"
	want := "PASS: 1 tests passed (go test)\nPASS: 1 tests passed (vitest)\n"
	if got := Parse(out); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := Parse(""); got != "" {
		t.Fatalf("empty output gave %q", got)
	}
}

// needs skips a test without the tools a box has.
func needs(t *testing.T, tools ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the wrappers run in a Linux box")
	}
	for _, x := range tools {
		if _, err := exec.LookPath(x); err != nil {
			t.Skip(x + " not found")
		}
	}
}

// The cut-down report from the box gives the same digest as the whole one.
func TestReduceKeepsDigest(t *testing.T) {
	needs(t, "jq")
	for _, c := range []struct {
		file, prog, flags string
		parse             func(string) string
	}{
		{"go.json", goReduce, "-nRc", Go},
		{"vitest.json", vitestReduce, "-c", Vitest},
	} {
		out, err := exec.Command("jq", c.flags, c.prog, filepath.Join("testdata", c.file)).Output()
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		full, cut := c.parse(read(t, c.file)), c.parse(string(out))
		if full != cut || full == "" {
			t.Fatalf("%s: reduced digest differs:\n%s\nfull:\n%s", c.file, cut, full)
		}
	}
}

// run runs the wrappers and a command in dir as a job would, with the job's
// files under a temporary prefix.
func run(t *testing.T, dir, command string) (log string, code int, prefix string) {
	t.Helper()
	prefix = filepath.Join(t.TempDir(), "job")
	c := exec.Command("bash", "-c", Wrappers+"set -o pipefail\n"+command)
	c.Dir = dir
	c.Env = append(os.Environ(), JobVar+"="+prefix)
	b, err := c.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(b), code, prefix
}

// go test under the wrapper exits and prints as go test does, and leaves
// its JSON report for the digest.
func TestGoWrapper(t *testing.T) {
	needs(t, "bash", "jq", "go")
	dir, _ := filepath.Abs(filepath.Join("testdata", "gofail"))
	log, code, prefix := run(t, dir, "go test ./...")
	if code != 1 {
		t.Fatalf("exit %d, want go test's 1:\n%s", code, log)
	}
	for _, s := range []string{"--- FAIL: TestAssert (0.00s)\n    fail_test.go:12: queue length", "--- FAIL: TestSub/bad", "panic: assignment to entry in nil map", "FAIL\texample.com/gofail\t", "undefined: undefinedThing"} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q", s)
		}
	}
	for _, s := range []string{"=== RUN", "passing output is hidden", "--- PASS", `"Action"`} {
		if strings.Contains(log, s) {
			t.Errorf("log has %q, which go test without -v doesn't print", s)
		}
	}
	if t.Failed() {
		t.Fatalf("log:\n%s", log)
	}
	report, err := os.ReadFile(prefix + ".gotest.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := Go(string(report)); !strings.HasPrefix(got, "FAIL: 3 of 6 tests failed") {
		t.Fatalf("digest from the wrapper's report:\n%s", got)
	}
	out, err := exec.Command("bash", "-c", Script(prefix)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := Parse(string(out)); !strings.HasPrefix(got, "FAIL: 3 of 6 tests failed") {
		t.Fatalf("digest through Script:\n%s", got)
	}

	// -v shows everything; a passing run exits 0; other go commands and a
	// run without the job's prefix are untouched.
	if log, code, _ := run(t, dir, "go test -v -run TestPass ."); code != 0 || !strings.Contains(log, "passing output is hidden") {
		t.Fatalf("-v: exit %d\n%s", code, log)
	}
	if log, code, _ := run(t, dir, "go env GOOS"); code != 0 || strings.TrimSpace(log) != "linux" {
		t.Fatalf("go env: exit %d\n%s", code, log)
	}
	c := exec.Command("bash", "-c", Wrappers+"go test -run TestPass . | grep -c Action")
	c.Dir = dir
	c.Env = append(os.Environ(), JobVar+"=")
	if out, _ := c.Output(); strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("without the prefix, go test printed JSON: %s", out)
	}
}

// vitest under the wrappers keeps its own output and exit code and writes
// its JSON report. It needs testdata/vitestfail's node_modules (npm install
// there), so it runs where they are, as on a box.
func TestVitestWrapper(t *testing.T) {
	needs(t, "bash", "jq", "npx")
	dir, _ := filepath.Abs(filepath.Join("testdata", "vitestfail"))
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "vitest")); err != nil {
		t.Skip("no node_modules in testdata/vitestfail")
	}
	for _, command := range []string{"npx vitest run", "npm test", "npm run test -- --reporter=verbose"} {
		log, code, prefix := run(t, dir, command)
		if code != 1 || !strings.Contains(log, "expected 4 to be 5") {
			t.Fatalf("%s: exit %d\n%s", command, code, log)
		}
		report, err := os.ReadFile(prefix + ".vitest.json")
		if err != nil {
			t.Fatalf("%s: %v\n%s", command, err, log)
		}
		if got := Vitest(string(report)); got != wantVitest {
			t.Fatalf("%s: digest:\n%s", command, got)
		}
	}
}
