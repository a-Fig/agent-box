package digest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Multiple vitest invocations in one job each write their own report file, and
// Script merges them so an earlier failure is never overwritten by a later pass.
func TestVitestMultipleRunsMerged(t *testing.T) {
	needs(t, "bash", "jq")
	dir := t.TempDir()
	// Fake vitest parses --outputFile.json=<path> and writes a canned report
	// depending on whether its first arg is "fail" or "pass".
	fakeVitest := `#!/bin/sh
out=""
for a in "$@"; do
  case "$a" in --outputFile.json=*) out="${a#--outputFile.json=}" ;; esac
done
if [ "$1" = fail ]; then
  cat > "$out" <<'EOF'
{"numTotalTests":2,"numPassedTests":1,"numFailedTests":1,"numPendingTests":0,"numTodoTests":0,"testResults":[{"name":"/work/r/a.test.js","status":"failed","assertionResults":[{"fullName":"a fails","status":"failed","location":{"line":7},"failureMessages":["AssertionError: expected 1 to be 2"]}]}]}
EOF
  exit 1
fi
cat > "$out" <<'EOF'
{"numTotalTests":1,"numPassedTests":1,"numFailedTests":0,"numPendingTests":0,"numTodoTests":0,"testResults":[{"name":"/work/r/b.test.js","status":"passed","assertionResults":[]}]}
EOF
`
	if err := os.WriteFile(filepath.Join(dir, "vitest"), []byte(fakeVitest), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// Subshell for the second run checks that file-based numbering survives subshells.
	_, code, prefix := run(t, dir, "vitest fail; rc=$?; (vitest pass); exit $rc")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	out, err := exec.Command("bash", "-c", Script(prefix)).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := Parse(string(out))
	if !strings.HasPrefix(got, "FAIL: 1 of 3 tests failed (vitest)\n") || !strings.Contains(got, "a.test.js:7: AssertionError: expected 1 to be 2") {
		t.Fatalf("merged digest:\n%s", got)
	}
}

// npm test -- --reporter=verbose forwards the caller's --reporter check so
// __box_vargs does not inject a duplicate --reporter=default.
func TestNpmForwardsCallerFlagsAndSupportsRunAndWorkspace(t *testing.T) {
	needs(t, "bash", "jq")
	dir := t.TempDir()
	ws := filepath.Join(dir, "packages", "a")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"vitest run","test:unit":"vitest run tests/unit","build":"vite build"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte(`{"scripts":{"test":"vitest run"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "npm"), []byte("#!/bin/sh\necho \"npm: $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// 1. Caller passes --reporter=verbose after -- -> no duplicate --reporter=default.
	log, _, _ := run(t, dir, "npm test -- --reporter=verbose tests/a.test.js")
	if strings.Contains(log, "--reporter=default") || !strings.Contains(log, "--reporter=json") {
		t.Errorf("npm test -- --reporter=verbose: %q", log)
	}

	// 2. npm run <other vitest script> gets wrapped; non-vitest script does not.
	log, _, _ = run(t, dir, "npm run test:unit")
	if !strings.Contains(log, "-- --reporter=default --reporter=json") {
		t.Errorf("npm run test:unit: %q", log)
	}
	log, _, _ = run(t, dir, "npm run build")
	if strings.Contains(log, "--reporter=json") {
		t.Errorf("npm run build should not be wrapped: %q", log)
	}

	// 3. Single-dir workspace (-w packages/a) and --prefix packages/a get wrapped;
	// --workspaces and multiple -w flags stay untouched.
	for _, cmd := range []string{"npm test -w packages/a", "npm -w packages/a test", "npm --prefix packages/a test"} {
		log, _, _ = run(t, dir, cmd)
		if !strings.Contains(log, "-- --reporter=default --reporter=json") {
			t.Errorf("%s: %q", cmd, log)
		}
	}
	for _, cmd := range []string{"npm test --workspaces", "npm test -w packages/a -w packages/b", "npm test -w @df/a"} {
		log, _, _ = run(t, dir, cmd)
		if strings.Contains(log, "--reporter=json") {
			t.Errorf("%s should stay untouched: %q", cmd, log)
		}
	}

	// 4. Workspace with its own reporters config stays untouched.
	if err := os.WriteFile(filepath.Join(ws, "vitest.config.ts"), []byte("export default { test: { reporters: ['dot'] } }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log, _, _ = run(t, dir, "npm test -w packages/a")
	if strings.Contains(log, "--reporter=json") {
		t.Errorf("workspace with custom reporters should stay untouched: %q", log)
	}
}

// A comment mentioning reporters/outputFile in vitest.config.* does not disable
// the digest, while a real reporters/outputFile key still does.
func TestVitestConfigCommentDoesNotDisableDigest(t *testing.T) {
	needs(t, "bash")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vitest"), []byte("#!/bin/sh\necho \"args: $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	cfg := filepath.Join(dir, "vitest.config.mjs")
	if err := os.WriteFile(cfg, []byte("// reporters: ['verbose'], outputFile: 'out.json'\nexport default {} // reporters: default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if log, _, _ := run(t, dir, "vitest run"); !strings.Contains(log, "--reporter=json") {
		t.Fatalf("comment-only config disabled digest: %q", log)
	}

	if err := os.WriteFile(cfg, []byte("export default { test: { outputFile: 'out.json' } }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if log, _, _ := run(t, dir, "vitest run"); strings.TrimSpace(log) != "args: run" {
		t.Fatalf("real outputFile config was wrapped: %q", log)
	}
}
