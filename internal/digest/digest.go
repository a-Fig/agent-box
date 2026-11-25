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

// maxLine caps one line of a digest.
const maxLine = 240

// Parse writes the digest from Script's output: one block per report, or ""
// when there was none.
func Parse(out string) string {
	var parts []string
	for _, sec := range strings.Split(out, "#box-digest ")[1:] {
		kind, body, _ := strings.Cut(sec, "\n")
		var d string
		switch strings.TrimSpace(kind) {
		case "go":
			d = Go(body)
		case "vitest":
			d = Vitest(body)
		}
		if d != "" {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, "")
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
func Go(report string) string {
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
		any        bool
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
		case e.Action == "pass":
			pass++
		case e.Action == "skip":
			skip++
		case e.Action == "fail" && !seen[t]:
			seen[t] = true
			failed = append(failed, t)
		case e.Action == "output" && e.OutputType != "frame":
			out[t] = append(out[t], e.Output)
		}
	}
	if !any {
		return ""
	}
	// A test that failed only because a subtest did isn't a failure of its
	// own: list and count the subtest.
	var leaves []test
	for _, t := range failed {
		parent := false
		for _, u := range failed {
			if u.pkg == t.pkg && strings.HasPrefix(u.name, t.name+"/") {
				parent = true
				break
			}
		}
		if !parent {
			leaves = append(leaves, t)
		}
	}
	// A failed package whose tests all passed broke outside them (TestMain,
	// a timeout, a crash between tests).
	failedIn := map[string]bool{}
	for _, t := range leaves {
		failedIn[t.pkg] = true
	}

	var d writer
	total := pass + skip + len(leaves)
	switch {
	case len(leaves) > 0:
		d.head(fmt.Sprintf("FAIL: %d of %d tests failed%s (go test)", len(leaves), total, skipped(skip)))
	case len(buildFail) > 0 || len(pkgFailed) > 0:
		d.head(fmt.Sprintf("FAIL: %d tests passed%s, but packages failed (go test)", pass, skipped(skip)))
	default:
		d.head(fmt.Sprintf("PASS: %d tests passed%s (go test)", pass, skipped(skip)))
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
	for i, t := range leaves {
		if t.pkg != pkg {
			pkg = t.pkg
			if !d.line(pkg) {
				d.more(len(leaves)-i, "failed tests")
				break
			}
		}
		loc, msg, more := goProblem(out[t])
		s := "  " + t.name
		if loc != "" {
			s += "  " + loc + ":"
		}
		if msg != "" {
			s += " " + msg
		}
		if !d.item(s, more...) {
			d.more(len(leaves)-i, "failed tests")
			break
		}
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

var (
	goAssert = regexp.MustCompile(`^\s*([\w.@+/-]+\.go):(\d+): ?(.*)$`)
	goFrame  = regexp.MustCompile(`^\t(\S+\.go):(\d+)(?: \+0x[0-9a-f]+)?$`)
)

// goProblem finds a failed test's first problem in its output: a panic and
// the frame it came from, else its first file:line message (t.Error,
// t.Fatal and the like), else its first line. more is up to two lines that
// continue the message.
func goProblem(lines []string) (loc, msg string, more []string) {
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
	return strings.Contains(file, "/go/src/") || strings.HasPrefix(file, "/usr/local/go/") || strings.HasPrefix(file, "/usr/lib/go")
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

// writer builds a digest within MaxBytes, always keeping room for the
// line that says what didn't fit.
type writer struct {
	b    strings.Builder
	full bool
}

const reserve = 120

func (w *writer) head(s string) { w.b.WriteString(clip(s) + "\n") }

// line adds one line if it fits.
func (w *writer) line(s string) bool {
	s = clip(s) + "\n"
	if w.full || w.b.Len()+len(s) > MaxBytes-reserve {
		w.full = true
		return false
	}
	w.b.WriteString(s)
	return true
}

// item adds a line and its continuation lines, all or none.
func (w *writer) item(s string, more ...string) bool {
	all := clip(s) + "\n"
	for _, m := range more {
		all += clip(m) + "\n"
	}
	if w.full || w.b.Len()+len(all) > MaxBytes-reserve {
		w.full = true
		return false
	}
	w.b.WriteString(all)
	return true
}

func (w *writer) more(n int, what string) {
	fmt.Fprintf(&w.b, "  ...and %d more %s; the full log has them\n", n, what)
}

func (w *writer) String() string { return w.b.String() }

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
