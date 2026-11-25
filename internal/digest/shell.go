// Package digest turns a test run's machine-readable report into a few
// lines an agent can act on: the failed tests, each with its file:line and
// first assertion or panic line, then the counts.
//
// It has two halves. In the box, Wrappers makes `go test` emit test2json
// events (go test -json) and vitest add its JSON reporter, keeps that report
// beside the job's log, and keeps the log close to what the tool prints and
// the exit code the tool's. On the laptop's side, Script cuts the reports
// down in the box and Parse writes the digest from them. Both halves key on
// the job's file prefix (/work/.box/<job>), which the job's shell exports as
// JobVar.
package digest

import (
	"fmt"
	"path"
	"strings"
)

// JobVar names the job's file prefix in the job's shell. The wrappers write
// <prefix>.gotest.json and <prefix>.vitest.json; unset, they do nothing.
const JobVar = "BOX_JOB_FILES"

// goRender is jq that prints go test -json's events as go test prints
// without -json: the output of failed tests, and package lines, while a
// passing test's output is dropped when it passes. A test's "--- FAIL" line,
// which -json prints last, goes before its output, as go test prints it.
// What a test holds back is bounded (its first 200 lines and last 100), so a
// chatty test costs linear time; $f names the report that has the rest. $v
// (the run asked for -v) prints all output. A line that isn't an event is
// printed as it is.
const goRender = `def key: (.Package // "") + " " + .Test;
def held($k): (.b[$k] // "")
  + (if (.n[$k] // 0) > 300 then "    ... box: \(.n[$k] - 300) lines left out here; \($f) has them\n" else "" end)
  + ((.t[$k] // []) | join(""));
def forget($k): del(.b[$k], .n[$k], .t[$k]);
def within($p): [(.b, .n) | keys[] | select(startswith($p))] | unique;
foreach inputs as $l ({b: {}, n: {}, t: {}, o: null};
  ($l | try fromjson catch null) as $e
  | if ($e | type) != "object" then .o = $l + "\n"
    elif $v then .o = $e.Output
    elif $e.Action == "output" or $e.Action == "build-output" then
      if $e.Test == null then .o = (if $e.Output == "PASS\n" then null else $e.Output end)
      elif ($e.Output // "" | startswith("=== ")) then .o = null
      elif ($e.Output | startswith("--- ")) then .o = null | .b[$e | key] |= $e.Output + (. // "")
      else ($e | key) as $k
        | .o = null
        | if (.n[$k] // 0) < 200 then .b[$k] += $e.Output else .t[$k] = ((.t[$k] // []) + [$e.Output])[-100:] end
        | .n[$k] += 1 end
    elif $e.Test != null then
      ($e | key) as $k
      | .o = (if $e.Action == "fail" then held($k) else null end)
      | if $e.Action == "pass" or $e.Action == "skip" or $e.Action == "fail" then forget($k) else . end
    elif $e.Action == "fail" or $e.Action == "pass" or $e.Action == "skip" then
      within(($e.Package // "") + " ") as $ks
      | .o = (if $e.Action == "fail" then [$ks[] as $k | held($k)] | add else null end)
      | reduce $ks[] as $k (.; forget($k))
    else .o = null end;
  .o // empty)`

// Wrappers is shell that defines go, vitest and the launchers that run
// vitest (npx, pnpm, yarn, npm test) as functions for the job's command.
// Each adds the machine-readable report and otherwise runs the real command
// with the same arguments and returns its exit code. A run whose shape it
// doesn't handle runs untouched: benchmarks (their result lines are a passing
// test's output), vitest after -- or with reporters in its config, an npm test
// script that isn't plain vitest or that runs in a workspace, and anything
// without jq. tee -p keeps the report and go test's exit code even if jq dies.
var Wrappers = fmt.Sprintf(`__box_gojq=%s
go() {
  if [ "${1-}" != test ] || [ -z "${%[2]s-}" ] || ! command -v jq >/dev/null 2>&1; then command go "$@"; return; fi
  shift
  local a v=false
  case " ${GOFLAGS-} " in *" -v "*|*" -v=true "*) v=true ;; esac
  for a in "$@"; do
    case $a in
      -args) break ;;
      -bench|-bench=*|-test.bench|-test.bench=*) command go test "$@"; return ;;
      -json|-json=true|--json|--json=true) command go test "$@" | tee -p -a "$%[2]s.gotest.json"; return "${PIPESTATUS[0]}" ;;
      -v|-v=true|--v|--v=true|-test.v|-test.v=true) v=true ;;
    esac
  done
  command go test -json "$@" | tee -p -a "$%[2]s.gotest.json" | jq -nRj --unbuffered --argjson v "$v" --arg f "$%[2]s.gotest.json" "$__box_gojq"
  return "${PIPESTATUS[0]}"
}
__box_vargs() {
  __box_va=()
  [ -n "${%[2]s-}" ] || return 1
  grep -qsE 'reporters|outputFile' vitest.config.* vite.config.* vitest.workspace.* && return 1
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
    for a in "$@"; do case $a in -w|--workspace*|--workspaces|--prefix*|-ws) command npm "$@"; return ;; esac; done
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

// goReduce is jq that cuts a go test -json report down to what Parse uses,
// smallest and most important first, so a cut loses only output lines:
// box-start, each failed test and each test still running when its package
// failed (a timeout: its run event), package and build ends, pass and skip counts
// per package, then the first lines of output of what failed, then box-end,
// which says nothing was cut.
const goReduce = `[inputs | fromjson? | objects] as $e
| ([$e[] | select(.Test != null and .Action == "fail") | (.Package // "") + " " + .Test] | INDEX(.)) as $f
| ([$e[] | select(.Test == null and .Action == "fail") | .Package // ""] | INDEX(.)) as $fp
| ([$e[] | select(.Test != null and (.Action == "pass" or .Action == "skip" or .Action == "fail")) | (.Package // "") + " " + .Test] | INDEX(.)) as $ended
| ([$e[] | select(.Test != null and .Action == "run" and $fp[.Package // ""] != null and $ended[(.Package // "") + " " + .Test] == null)
    | (.Package // "") + " " + .Test] | INDEX(.)) as $hung
| ($f + $hung) as $f
| {Action: "box-start"},
  ($e[] | select(.Test != null and .Action == "fail") | {Action, Package, Test}),
  ($e[] | select(.Test != null and .Action == "run" and $hung[(.Package // "") + " " + .Test] != null) | {Action, Package, Test}),
  ($e[] | select(.Test == null and (.Action == "pass" or .Action == "skip" or .Action == "fail" or .Action == "build-fail")) | {Action, Package, ImportPath, FailedBuild}),
  ([$e[] | select(.Test != null and (.Action == "pass" or .Action == "skip"))] | group_by(.Package // "") | .[]
    | {Action: "box-count", Package: .[0].Package, Pass: (map(select(.Action == "pass")) | length), Skip: (map(select(.Action == "skip")) | length)}),
  ([$e[] | select(.Action == "output" and .Test != null and .OutputType != "frame" and $f[(.Package // "") + " " + .Test] != null)]
    | group_by((.Package // "") + " " + .Test) | .[] | .[:40][] | {Action, Package, Test, Output: (.Output // "")[:300], OutputType}),
  ([$e[] | select(.Action == "output" and .Test == null and $fp[.Package // ""] != null)]
    | group_by(.Package // "") | .[] | .[:20][] | {Action, Package, Output: (.Output // "")[:300]}),
  ([$e[] | select(.Action == "build-output")] | group_by(.ImportPath) | .[] | .[:20][] | {Action, ImportPath, Output: (.Output // "")[:300]}),
  {Action: "box-end"}`

// vitestReduce keeps vitest's counts and the first of what failed. A digest
// shows only a message's first line, and its JSON must stay whole under
// maxReport.
const vitestReduce = `{numTotalTests, numPassedTests, numFailedTests, numPendingTests, numTodoTests,
  testResults: [.testResults[]? | select(.status != "passed" or any(.assertionResults[]?; .status == "failed"))
    | {name, status, message: (.message // "")[:1000],
       assertionResults: [.assertionResults[]? | select(.status == "failed")
         | {fullName: (.fullName // "")[:300], status, location, failureMessages: [(.failureMessages // [])[:1][] | .[:1000]]}][:20]}][:20]}`

// Section heads in Script's output.
const (
	goHead     = "#box-digest go"
	vitestHead = "#box-digest vitest"
)

// maxReport caps what Script prints of each report.
const maxReport = 1 << 20

// Script is bash for the job's box that prints the job's reports, cut down,
// each under its section head. It prints nothing when there are none. The go
// report ends with a box-module event naming the module in the checkout's
// go.mod (<root>/<repo>, the repo from <prefix>.repo), so Parse can make an
// assertion's package-relative file:line relative to the checkout.
func Script(prefix string) string {
	p := shq(prefix)
	root := shq(path.Dir(path.Dir(prefix))) // <root>/.box/<job>
	return fmt.Sprintf(`# digest
command -v jq >/dev/null 2>&1 || exit 0
if [ -s %[1]s.gotest.json ]; then
  echo %[2]s; jq -nRc %[4]s %[1]s.gotest.json | head -c %[6]d; echo
  m=$(sed -n 's/^module[[:space:]]\{1,\}//p' %[7]s/"$(cat %[1]s.repo 2>/dev/null)"/go.mod 2>/dev/null | head -n1 | tr -d '\r"')
  [ -n "$m" ] && jq -nc --arg m "$m" '{Action: "box-module", Package: $m}'
fi
if [ -s %[1]s.vitest.json ]; then echo %[3]s; jq -c %[5]s %[1]s.vitest.json | head -c %[6]d; echo; fi
exit 0
`, p, shq(goHead), shq(vitestHead), shq(goReduce), shq(vitestReduce), maxReport, root)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
