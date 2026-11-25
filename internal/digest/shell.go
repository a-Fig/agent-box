// Package digest turns a test run's machine-readable report into a few
// lines an agent can act on: the failed tests, each with its file:line and
// first assertion or panic line, then the counts.
//
// It has two halves. In the box, Wrappers makes `go test` emit test2json
// events (go test -json) and vitest add its JSON reporter, keeps that report
// beside the job's log, and leaves the log and the exit code as they were.
// On the laptop, Script reads the report back, cut down by jq in the box,
// and Parse writes the digest from it. Both halves key on the job's file
// prefix (/work/.box/<job>), which the job's shell exports as JobVar.
package digest

import (
	"fmt"
	"strings"
)

// JobVar names the job's file prefix in the job's shell. The wrappers write
// <prefix>.gotest.json and <prefix>.vitest.json; unset, they do nothing.
const JobVar = "BOX_JOB_FILES"

// goRender is jq that prints go test -json's events as go test prints
// without -json: the output of failed tests, and package lines, while a
// passing test's output is dropped when it passes. A test's "--- FAIL" line,
// which -json prints last, goes before its output, as go test prints it.
// $v (the run asked for
// -v) prints all output. A line that isn't an event is printed as it is.
const goRender = `def key: (.Package // "") + " " + .Test;
def drop($p): .b |= with_entries(select(.key | startswith($p) | not));
foreach inputs as $l ({b: {}, o: null};
  ($l | try fromjson catch null) as $e
  | if ($e | type) != "object" then .o = $l + "\n"
    elif $v then .o = $e.Output
    elif $e.Action == "output" or $e.Action == "build-output" then
      if $e.Test == null then .o = (if $e.Output == "PASS\n" then null else $e.Output end)
      elif ($e.Output // "" | startswith("=== ")) then .o = null
      elif ($e.Output | startswith("--- ")) then .o = null | .b[$e | key] |= $e.Output + (. // "")
      else .o = null | .b[$e | key] += $e.Output end
    elif $e.Test != null then
      ($e | key) as $k
      | .o = (if $e.Action == "fail" then .b[$k] else null end)
      | if $e.Action == "pass" or $e.Action == "skip" or $e.Action == "fail" then del(.b[$k]) else . end
    elif $e.Action == "fail" then
      (($e.Package // "") + " ") as $p
      | .o = ([.b | to_entries[] | select(.key | startswith($p)) | .value] | add) | drop($p)
    elif $e.Action == "pass" or $e.Action == "skip" then
      .o = null | drop(($e.Package // "") + " ")
    else .o = null end;
  .o // empty)`

// Wrappers is shell that defines go, vitest and the launchers that run
// vitest (npx, pnpm, yarn, npm test) as functions for the job's command.
// Each adds the machine-readable report and otherwise runs the real command
// with the same arguments and returns its exit code. A run whose shape it
// doesn't know (vitest after --, a test script that isn't plain vitest)
// runs untouched, and so does everything without jq.
var Wrappers = fmt.Sprintf(`__box_gojq=%s
go() {
  if [ "${1-}" != test ] || [ -z "${%[2]s-}" ] || ! command -v jq >/dev/null 2>&1; then command go "$@"; return; fi
  shift
  local a v=false
  for a in "$@"; do
    case $a in
      -args) break ;;
      -json|-json=true|--json|--json=true) command go test "$@" | tee -a "$%[2]s.gotest.json"; return "${PIPESTATUS[0]}" ;;
      -v|-v=true|--v|--v=true|-test.v|-test.v=true) v=true ;;
    esac
  done
  command go test -json "$@" | tee -a "$%[2]s.gotest.json" | jq -nRj --unbuffered --argjson v "$v" "$__box_gojq"
  return "${PIPESTATUS[0]}"
}
__box_vargs() {
  __box_va=()
  [ -n "${%[2]s-}" ] || return 1
  local a r=--reporter=default
  for a in "$@"; do case $a in --) return 1 ;; --reporter|--reporter=*) r= ;; esac; done
  [ -n "$r" ] && __box_va+=("$r")
  __box_va+=(--reporter=json "--outputFile.json=$%[2]s.vitest.json")
}
vitest() { if __box_vargs "$@"; then command vitest "$@" "${__box_va[@]}"; else command vitest "$@"; fi; }
__box_vrun() {
  local l=$1 p=()
  shift
  if [ "${1-}" = exec ]; then p=(exec); shift; fi
  if [ "${1-}" = vitest ] && __box_vargs "${@:2}"; then command "$l" "${p[@]}" "$@" "${__box_va[@]}"; else command "$l" "${p[@]}" "$@"; fi
}
npx() { __box_vrun npx "$@"; }
pnpm() { __box_vrun pnpm "$@"; }
yarn() { __box_vrun yarn "$@"; }
npm() {
  local s w a dd=-- re='^vitest([[:space:]][^;&|<>$%[3]s(){}]*)?$'
  if [ "${1-}" = test ] || [ "${1-}" = t ] || { [ "${1-}" = run ] && [ "${2-}" = test ]; }; then
    s=$(jq -r '.scripts.test // empty' package.json 2>/dev/null)
    if [[ $s =~ $re ]]; then
      read -ra w <<<"$s"
      for a in "$@"; do [ "$a" = -- ] && dd=; done
      if __box_vargs "${w[@]:1}"; then command npm "$@" $dd "${__box_va[@]}"; return; fi
    fi
  fi
  command npm "$@"
}
`, shq(goRender), JobVar, "`")

// GoRender is the jq program that renders go test -json as go test's own
// output, for tests.
const GoRender = goRender

// goReduce is jq that cuts a go test -json report down to what Parse uses:
// every test's pass, skip or fail, package and build ends, and the first
// lines of output of what failed.
const goReduce = `[inputs | fromjson? | objects] as $e
| ([$e[] | select(.Test != null and .Action == "fail") | (.Package // "") + " " + .Test] | INDEX(.)) as $f
| ([$e[] | select(.Test == null and .Action == "fail") | .Package // ""] | INDEX(.)) as $fp
| ($e[] | select(.Test != null and (.Action == "pass" or .Action == "skip" or .Action == "fail")) | {Action, Package, Test}),
  ($e[] | select(.Test == null and (.Action == "pass" or .Action == "skip" or .Action == "fail" or .Action == "build-fail")) | {Action, Package, ImportPath, FailedBuild}),
  ([$e[] | select(.Action == "output" and .Test != null and .OutputType != "frame" and $f[(.Package // "") + " " + .Test] != null)]
    | group_by((.Package // "") + " " + .Test) | .[] | .[:40][] | {Action, Package, Test, Output: (.Output // "")[:300], OutputType}),
  ([$e[] | select(.Action == "output" and .Test == null and $fp[.Package // ""] != null)]
    | group_by(.Package // "") | .[] | .[:20][] | {Action, Package, Output: (.Output // "")[:300]}),
  ([$e[] | select(.Action == "build-output")] | group_by(.ImportPath) | .[] | .[:20][] | {Action, ImportPath, Output: (.Output // "")[:300]})`

// vitestReduce keeps vitest's counts and what failed.
const vitestReduce = `{numTotalTests, numPassedTests, numFailedTests, numPendingTests, numTodoTests,
  testResults: [.testResults[]? | select(.status != "passed" or any(.assertionResults[]?; .status == "failed"))
    | {name, status, message: (.message // "")[:2000],
       assertionResults: [.assertionResults[]? | select(.status == "failed")
         | {fullName, status, location, failureMessages: [(.failureMessages // [])[:1][] | .[:2000]]}][:50]}][:50]}`

// Section heads in Script's output.
const (
	goHead     = "#box-digest go"
	vitestHead = "#box-digest vitest"
)

// maxReport caps what Script prints of each report.
const maxReport = 1 << 20

// Script is bash for the job's box that prints the job's reports, cut down,
// each under its section head. It prints nothing when there are none.
func Script(prefix string) string {
	p := shq(prefix)
	return fmt.Sprintf(`# digest
command -v jq >/dev/null 2>&1 || exit 0
if [ -s %[1]s.gotest.json ]; then echo %[2]s; jq -nRc %[4]s %[1]s.gotest.json | head -c %[6]d; echo; fi
if [ -s %[1]s.vitest.json ]; then echo %[3]s; jq -c %[5]s %[1]s.vitest.json | head -c %[6]d; echo; fi
exit 0
`, p, shq(goHead), shq(vitestHead), shq(goReduce), shq(vitestReduce), maxReport)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
