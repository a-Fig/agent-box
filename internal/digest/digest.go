package digest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxBytes caps a digest, so it costs an agent's context little: past it,
// the rest of the failures are counted, not listed.
const MaxBytes = 3 << 10

// noRoom is what Parse keeps back from each report's digest, for the line
// that names a later report it had no room for.
const noRoom = 80

// maxLine caps one line of a digest.
const maxLine = 240

// Parse writes the digest from Script's output: one block per report, all
// within MaxBytes, or "" when there was none.
func Parse(out string) string {
	var b strings.Builder
	for _, sec := range strings.Split(out, "#box-digest ")[1:] {
		kind, body, _ := strings.Cut(sec, "\n")
		room := MaxBytes - b.Len()
		if room < 2*reserve {
			if room >= noRoom {
				fmt.Fprintf(&b, "(no room for the %s report's digest; the full log has it)\n", strings.TrimSpace(kind))
			}
			continue
		}
		switch strings.TrimSpace(kind) {
		case "go":
			b.WriteString(goDigest(body, room-noRoom))
		case "vitest":
			b.WriteString(vitestDigest(body, room-noRoom))
		}
	}
	return b.String()
}

type goEvent struct {
	Action      string
	Package     string
	Test        string
	Output      string
	OutputType  string
	ImportPath  string
	FailedBuild string
}

// Go digests a go test -json report, whole or cut down by goReduce.
func Go(report string) string { return goDigest(report, MaxBytes) }

func goDigest(report string, max int) string {
	type test struct{ pkg, name string }
	var (
		failed     []test
		seen       = map[test]bool{}
		out        = map[test][]string{}
		pkgFailed  []string
		pkgSeen    = map[string]bool{}
		pkgOut     = map[string][]string{}
		buildFail  []string
		buildSeen  = map[string]bool{}
		buildOut   = map[string][]string{}
		pass, skip int
		ran        []test            // tests that started (run)
		ended      = map[test]bool{} // and passed, skipped or failed
		module     string            // the checkout's module path (box-module)
		any        bool
		reduced    bool // the report is goReduce's (box-start)
		whole      bool // and it ends with box-end: nothing was cut off
	)
	sc := bufio.NewScanner(strings.NewReader(report))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e goEvent
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Action == "" {
			continue
		}
		any = true
		t := test{e.Package, e.Test}
		switch {
		case e.Action == "box-start":
			reduced = true
		case e.Action == "box-count":
			var c struct{ Pass, Skip int }
			json.Unmarshal(sc.Bytes(), &c)
			pass, skip = pass+c.Pass, skip+c.Skip
		case e.Action == "box-end":
			whole = true
		case e.Action == "box-module":
			module = e.Package
		case e.Action == "build-output":
			buildOut[e.ImportPath] = append(buildOut[e.ImportPath], e.Output)
		case e.Action == "build-fail" && !buildSeen[e.ImportPath]:
			buildSeen[e.ImportPath] = true
			buildFail = append(buildFail, e.ImportPath)
		case e.Test == "" && e.Action == "fail":
			if e.FailedBuild != "" {
				if !buildSeen[e.FailedBuild] {
					buildSeen[e.FailedBuild] = true
					buildFail = append(buildFail, e.FailedBuild)
				}
			} else if !pkgSeen[e.Package] {
				pkgSeen[e.Package] = true
				pkgFailed = append(pkgFailed, e.Package)
			}
		case e.Test == "" && e.Action == "output":
			pkgOut[e.Package] = append(pkgOut[e.Package], e.Output)
		case e.Test == "":
		case e.Action == "run":
			ran = append(ran, t)
		case e.Action == "pass":
			pass++
			ended[t] = true
		case e.Action == "skip":
			skip++
			ended[t] = true
		case e.Action == "fail" && !seen[t]:
			seen[t] = true
			ended[t] = true
			failed = append(failed, t)
		case e.Action == "output" && e.OutputType != "frame":
			out[t] = append(out[t], e.Output)
		}
	}
	if !any {
		return ""
	}
	// A test still running when its package failed (a timeout, a crash) gets
	// no end of its own; its output says what happened (#110).
	for _, t := range ran {
		if !ended[t] && pkgSeen[t.pkg] && !seen[t] {
			seen[t] = true
			failed = append(failed, t)
		}
	}
	// A test that failed only because a subtest did isn't a failure of its
	// own: list and count the subtest. One with a message of its own is.
	var leaves []test
	for _, t := range failed {
		parent := false
		for _, u := range failed {
			if u.pkg == t.pkg && strings.HasPrefix(u.name, t.name+"/") {
				parent = true
				break
			}
		}
		if !parent || hasOwn(out[t]) {
			leaves = append(leaves, t)
		}
	}
	// A failed package whose tests all passed broke outside them (TestMain,
	// a timeout, a crash between tests).
	failedIn := map[string]bool{}
	for _, t := range leaves {
		failedIn[t.pkg] = true
	}

	d := writer{max: max}
	total := pass + skip + len(leaves)
	cut := ""
	if reduced && !whole {
		cut = ", report cut short"
	}
	switch {
	case len(leaves) > 0:
		d.head(fmt.Sprintf("FAIL: %d of %d tests failed%s%s (go test)", len(leaves), total, skipped(skip), cut))
	case len(buildFail) > 0 || len(pkgFailed) > 0:
		d.head(fmt.Sprintf("FAIL: %d tests passed%s%s, but packages failed (go test)", pass, skipped(skip), cut))
	default:
		d.head(fmt.Sprintf("PASS: %d tests passed%s%s (go test)", pass, skipped(skip), cut))
		return d.String()
	}
	for _, p := range buildFail {
		var lines []string
		for _, l := range buildOut[p] {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "# ") {
				lines = append(lines, "  "+l)
			}
		}
		if len(lines) > 3 {
			lines = lines[:3]
		}
		name, _, _ := strings.Cut(p, " [") // "pkg [pkg.test]"
		d.item("build failed: "+name, lines...)
	}
	pkg := ""
	for _, t := range leaves {
		if t.pkg != pkg {
			pkg = t.pkg
			d.line(pkg)
		}
		loc, msg, more := goProblem(out[t])
		loc = inPackage(loc, t.pkg, module)
		s := "  " + t.name
		if loc != "" {
			s += "  " + loc + ":"
		}
		if msg != "" {
			s += " " + msg
		}
		d.item(s, more...)
	}
	for _, p := range pkgFailed {
		if failedIn[p] {
			continue
		}
		var lines []string
		for _, l := range pkgOut[p] {
			if l = strings.TrimSpace(l); l != "" && l != "FAIL" && !strings.HasPrefix(l, "FAIL\t") && !strings.HasPrefix(l, "exit status ") {
				lines = append(lines, "  "+l)
			}
		}
		if len(lines) > 3 {
			lines = lines[:3]
		}
		d.item("FAIL "+p, lines...)
	}
	return d.String()
}

// inPackage makes a file:line that go test printed relative to its
// package's directory (an assertion's) relative to the checkout, as a
// panic's or a build error's is, when the package is in the checkout's
// module (#110).
func inPackage(loc, pkg, module string) string {
	if module == "" || loc == "" || strings.Contains(loc, "/") {
		return loc
	}
	dir, ok := strings.CutPrefix(pkg, module)
	if !ok || (dir != "" && !strings.HasPrefix(dir, "/")) {
		return loc
	}
	if dir = strings.TrimPrefix(dir, "/"); dir == "" {
		return loc
	}
	return dir + "/" + loc
}

// hasOwn says whether a test's output has a line of its own, not just the
// "--- FAIL" frames of its subtests.
func hasOwn(lines []string) bool {
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "--- ") && !strings.HasPrefix(l, "=== ") {
			return true
		}
	}
	return false
}

var (
	goAssert = regexp.MustCompile(`^\s*([\w.@+/-]+\.go):(\d+): ?(.*)$`)
	goFrame  = regexp.MustCompile(`^\t(\S+\.go):(\d+)(?: \+0x[0-9a-f]+)?$`)
	// raceFrame is a frame in a race report, indented with spaces.
	raceFrame = regexp.MustCompile(`^\s+(/\S+\.go):(\d+)(?: \+0x[0-9a-f]+)?$`)
)

// goProblem finds a failed test's first problem in its output: a panic and
// the frame it came from, else its first file:line message (t.Error,
// t.Fatal and the like), else its first line. more is up to two lines that
// continue the message.
func goProblem(lines []string) (loc, msg string, more []string) {
	// A data race fails its test through the testing package's own line: the
	// race report's first frame in the checkout is where it is (#110).
	for i, l := range lines {
		if strings.TrimSpace(l) != "WARNING: DATA RACE" {
			continue
		}
		for _, f := range lines[i+1:] {
			if m := raceFrame.FindStringSubmatch(strings.TrimRight(f, "\n")); m != nil && !goroot(m[1]) {
				return rel(m[1]) + ":" + m[2], "data race", nil
			}
		}
		return "", "data race", nil
	}
	for i, l := range lines {
		l = strings.TrimRight(l, "\n")
		if !strings.HasPrefix(l, "panic: ") {
			continue
		}
		msg = strings.TrimSuffix(strings.TrimSuffix(l, " [recovered, repanicked]"), " [recovered]")
		// The frame under the last panic( call is where it panicked, unless
		// that is in the runtime or testing (a nil map write, say): then the
		// first frame of the caller's own code.
		start := i + 1
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "panic(") {
				start = j + 1
			}
		}
		for j := start; j < len(lines); j++ {
			if m := goFrame.FindStringSubmatch(strings.TrimRight(lines[j], "\n")); m != nil && !goroot(m[1]) {
				return rel(m[1]) + ":" + m[2], msg, nil
			}
		}
		return "", msg, nil
	}
	for i, l := range lines {
		m := goAssert.FindStringSubmatch(strings.TrimRight(l, "\n"))
		if m == nil {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " \t"))
		for _, c := range lines[i+1:] {
			ci := len(c) - len(strings.TrimLeft(c, " \t"))
			c = strings.TrimSpace(c)
			if ci <= indent || c == "" || len(more) == 2 {
				break
			}
			more = append(more, "      "+c)
		}
		return rel(m[1]) + ":" + m[2], m[3], more
	}
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "--- ") && !strings.HasPrefix(l, "=== ") {
			return "", l, nil
		}
	}
	return "", "", nil
}

// goroot says whether a frame's file is Go's own (the runtime, testing).
func goroot(file string) bool {
	return strings.Contains(file, "/go/src/") || strings.HasPrefix(file, "/usr/local/go/") || strings.HasPrefix(file, "/usr/lib/go") ||
		file == "_testmain.go" // go test's generated main, in a timeout's stacks
}

// rel makes a path in a box's checkout (/work/<repo>/...) relative to it.
func rel(p string) string {
	if r, ok := strings.CutPrefix(p, "/work/"); ok {
		if _, after, ok := strings.Cut(r, "/"); ok {
			return after
		}
	}
	return p
}

func skipped(n int) string {
	if n == 0 {
		return ""
	}
	return ", " + strconv.Itoa(n) + " skipped"
}

// writer builds a digest within max bytes, always keeping room for the
// line that says how many failures didn't fit.
type writer struct {
	b       strings.Builder
	max     int
	full    bool
	dropped int
}

const reserve = 120

func (w *writer) head(s string) { w.b.WriteString(clip(s) + "\n") }

// line adds one line if it fits.
func (w *writer) line(s string) bool {
	s = clip(s) + "\n"
	if w.full || w.b.Len()+len(s) > w.max-reserve {
		w.full = true
		return false
	}
	w.b.WriteString(s)
	return true
}

// item adds one failure, a line and its continuation lines, all or none;
// once the digest is full, it counts the failure instead.
func (w *writer) item(s string, more ...string) bool {
	all := clip(s) + "\n"
	for _, m := range more {
		all += clip(m) + "\n"
	}
	if w.full || w.b.Len()+len(all) > w.max-reserve {
		w.full = true
		w.dropped++
		return false
	}
	w.b.WriteString(all)
	return true
}

func (w *writer) String() string {
	if w.dropped == 0 {
		return w.b.String()
	}
	return w.b.String() + fmt.Sprintf("  ...and %d more failures; the full log has them\n", w.dropped)
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// clip makes s one line of at most maxLine bytes, from whole characters.
func clip(s string) string {
	s = ansi.ReplaceAllString(s, "")
	s = strings.TrimRight(strings.ReplaceAll(s, "\r", ""), "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) <= maxLine {
		return s
	}
	cut := maxLine
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
