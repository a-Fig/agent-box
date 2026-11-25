package digest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A parent that fails with a message of its own is listed beside its
// failed subtest; one that fails only through it is not.
func TestGoParentWithOwnFailure(t *testing.T) {
	report := `{"Action":"output","Package":"p","Test":"TestA/x","Output":"    a_test.go:3: child broke\n"}
{"Action":"fail","Package":"p","Test":"TestA/x"}
{"Action":"output","Package":"p","Test":"TestA","Output":"    a_test.go:5: parent broke too\n"}
{"Action":"fail","Package":"p","Test":"TestA"}
{"Action":"fail","Package":"p","Test":"TestB/y"}
{"Action":"fail","Package":"p","Test":"TestB"}
`
	got := Go(report)
	if !strings.HasPrefix(got, "FAIL: 3 of 3 tests failed") || !strings.Contains(got, "TestA  a_test.go:5: parent broke too") || strings.Contains(got, "  TestB ") {
		t.Fatalf("got:\n%s", got)
	}
}

// Two reports share one MaxBytes, and failures that don't fit are counted,
// build failures included.
func TestParseCap(t *testing.T) {
	var g, v strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&g, `{"Action":"build-fail","ImportPath":"example.com/p%d [example.com/p%d.test]"}`+"\n", i, i)
	}
	v.WriteString(`{"numTotalTests":200,"numFailedTests":200,"testResults":[{"name":"/work/r/a.test.js","status":"failed","assertionResults":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			v.WriteString(",")
		}
		fmt.Fprintf(&v, `{"fullName":"case %d %s","status":"failed","failureMessages":["Error: no"]}`, i, strings.Repeat("y", 40))
	}
	v.WriteString("]}]}")
	got := Parse(goHead + "\n" + g.String() + vitestHead + "\n" + v.String() + "\n")
	if len(got) > MaxBytes || !strings.Contains(got, "more failures; the full log has them") {
		t.Fatalf("%d bytes:\n%s", len(got), got)
	}
}

// module writes a Go module with one test file into a temporary directory.
func module(t *testing.T, test string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "m_test.go"), []byte("package m\n\nimport \"testing\"\n\n"+test), 0o644)
	return dir
}

// Passes are counted, not listed, before the cut, so thousands of them
// can't push a failure out of the report and make the digest say PASS.
func TestReduceManyPassesKeepsTheFailure(t *testing.T) {
	needs(t, "jq")
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, `{"Action":"pass","Package":"example.com/some/long/package/path","Test":"TestMany/case_number_%d","Elapsed":0}`+"\n", i)
	}
	b.WriteString(`{"Action":"fail","Package":"example.com/some/long/package/path","Test":"TestLast"}` + "\n")
	b.WriteString(`{"Action":"fail","Package":"example.com/some/long/package/path"}` + "\n")
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(b.String()), 0o644)
	out, err := exec.Command("jq", "-nRc", goReduce, f).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := Go(string(out)); !strings.HasPrefix(got, "FAIL: 1 of 20001 tests failed (go test)") {
		t.Fatalf("got:\n%s", got)
	}
}

// Benchmarks run unwrapped, so their result lines stay in the log.
func TestGoWrapperBenchmark(t *testing.T) {
	needs(t, "bash", "jq", "go")
	dir := module(t, "func BenchmarkX(b *testing.B) { for i := 0; i < b.N; i++ {} }\n")
	log, code, _ := run(t, dir, "go test -run '^$' -bench . -benchtime 10x")
	if code != 0 || !strings.Contains(log, "BenchmarkX") || !strings.Contains(log, "ns/op") {
		t.Fatalf("exit %d:\n%s", code, log)
	}
}

// A chatty passing test costs the renderer linear time, and a chatty
// failing one prints its first and last lines with a count between.
func TestGoWrapperChattyTest(t *testing.T) {
	needs(t, "bash", "jq", "go")
	dir := module(t, `func TestChatty(t *testing.T) { for i := 0; i < 60000; i++ { t.Log("line", i) } }
func TestChattyFail(t *testing.T) { for i := 0; i < 1000; i++ { t.Log("noise", i) }; t.Fatal("the end") }
`)
	start := time.Now()
	log, code, _ := run(t, dir, "go test .")
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("took %s", took)
	}
	if code != 1 || !strings.Contains(log, "noise 0") || !strings.Contains(log, "lines left out here") || !strings.Contains(log, "the end") || strings.Contains(log, "line 5") {
		t.Fatalf("exit %d:\n%.3000s", code, log)
	}
}

// A vitest config that sets reporters keeps them: vitest runs untouched.
func TestVitestConfigReportersKept(t *testing.T) {
	needs(t, "bash")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "vitest.config.ts"), []byte("export default { test: { reporters: ['junit'] } }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "vitest"), []byte("#!/bin/sh\necho \"args: $*\"\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	log, _, _ := run(t, dir, "vitest run")
	if strings.TrimSpace(log) != "args: run" {
		t.Fatalf("got %q", log)
	}
	os.Remove(filepath.Join(dir, "vitest.config.ts"))
	if log, _, _ := run(t, dir, "vitest run"); !strings.Contains(log, "--reporter=json") {
		t.Fatalf("without a config: %q", log)
	}
}

// A cut-down report that lost its end says so, and a second report with no
// room left is named, not dropped.
func TestGoCutShortAndNoRoom(t *testing.T) {
	cut := `{"Action":"box-start"}
{"Action":"fail","Package":"p","Test":"TestA"}
{"Action":"output","Package":"p","Test":"TestA","Output":"    a_test.go:3: no\n"}
`
	if got := Go(cut); !strings.HasPrefix(got, "FAIL: 1 of 1 tests failed, report cut short (go test)") {
		t.Fatalf("got:\n%s", got)
	}
	var g strings.Builder
	g.WriteString(`{"Action":"box-start"}` + "\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&g, `{"Action":"fail","Package":"p","Test":"TestN%d"}`+"\n", i)
	}
	g.WriteString(`{"Action":"box-end"}` + "\n")
	got := Parse(goHead + "\n" + g.String() + vitestHead + "\n" + `{"numTotalTests":1,"numPassedTests":1,"testResults":[]}` + "\n")
	if len(got) > MaxBytes || !strings.Contains(got, "no room for the vitest report's digest") {
		t.Fatalf("%d bytes:\n%s", len(got), got)
	}
}

// racetimeout.json is go test -json -race -timeout 5s from a box (Go 1.27)
// on three packages of module example.com/dd: a data race, a test that hangs
// until the timeout, and an assertion (#110).
func TestGoRaceTimeoutAndPaths(t *testing.T) {
	report := read(t, "racetimeout.json")
	got := Parse(goHead + "\n" + report + `{"Action":"box-module","Package":"example.com/dd"}` + "\n")
	for _, want := range []string{
		"FAIL: 3 of 4 tests failed (go test)",
		"TestRace  r/r_test.go:8: data race",
		"TestHangs  h/h_test.go:10: panic: test timed out after 5s",
		"TestAdd  a/a_test.go:5: 1+1 = 2, want 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q:\n%s", want, got)
		}
	}
	// Without the module, an assertion keeps go test's own path.
	if got := Go(report); !strings.Contains(got, "TestAdd  a_test.go:5:") {
		t.Errorf("without box-module:\n%s", got)
	}
	if inPackage("x_test.go:3", "other.org/p", "example.com/dd") != "x_test.go:3" || inPackage("x_test.go:3", "example.com/ddx/p", "example.com/dd") != "x_test.go:3" {
		t.Error("inPackage moved a file outside the module")
	}
}

// A panic in TestMain fails its package with no test; its frame names it.
func TestGoTestMainPanic(t *testing.T) {
	report := `{"Action":"start","Package":"example.com/oddgo/mainpanic"}
{"Action":"output","Package":"example.com/oddgo/mainpanic","Output":"panic: boom in TestMain\n"}
{"Action":"output","Package":"example.com/oddgo/mainpanic","Output":"\n"}
{"Action":"output","Package":"example.com/oddgo/mainpanic","Output":"goroutine 1 [running]:\n"}
{"Action":"output","Package":"example.com/oddgo/mainpanic","Output":"example.com/oddgo/mainpanic.TestMain(0xc000102000)\n"}
{"Action":"output","Package":"example.com/oddgo/mainpanic","Output":"\t/work/oddgo/mainpanic/p_test.go:4 +0x25\n"}
{"Action":"output","Package":"example.com/oddgo/mainpanic","Output":"main.main()\n"}
{"Action":"output","Package":"example.com/oddgo/mainpanic","Output":"\t_testmain.go:45 +0x9b\n"}
{"Action":"fail","Package":"example.com/oddgo/mainpanic","Elapsed":0.01}
`
	if got := Go(report); !strings.Contains(got, "FAIL example.com/oddgo/mainpanic  mainpanic/p_test.go:4: panic: boom in TestMain") {
		t.Fatalf("got:\n%s", got)
	}
}

// Script names the checkout's module after the go report, from the repo the
// job's .repo file names (#110).
func TestScriptNamesTheModule(t *testing.T) {
	needs(t, "jq", "bash")
	root := t.TempDir()
	for name, body := range map[string]string{
		".box/j.gotest.json": `{"Action":"fail","Package":"example.com/dd/a","Test":"TestA"}` + "\n",
		".box/j.repo":        "dd\n",
		"dd/go.mod":          "module example.com/dd\n\ngo 1.22\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("bash", "-c", Script(root+"/.box/j")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `{"Action":"box-module","Package":"example.com/dd"}`) {
		t.Errorf("Script's output:\n%s", out)
	}
}
