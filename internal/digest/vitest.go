package digest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// vitestReport is the part of vitest's JSON reporter (Jest's shape) that a
// digest uses.
type vitestReport struct {
	NumTotalTests   *int `json:"numTotalTests"`
	NumPassedTests  int  `json:"numPassedTests"`
	NumFailedTests  int  `json:"numFailedTests"`
	NumPendingTests int  `json:"numPendingTests"`
	NumTodoTests    int  `json:"numTodoTests"`
	TestResults     []struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		Message          string `json:"message"`
		AssertionResults []struct {
			FullName string `json:"fullName"`
			Status   string `json:"status"`
			Location *struct {
				Line int `json:"line"`
			} `json:"location"`
			FailureMessages []string `json:"failureMessages"`
		} `json:"assertionResults"`
	} `json:"testResults"`
}

// Vitest digests vitest's JSON report, whole or cut down by vitestReduce.
func Vitest(report string) string {
	var r vitestReport
	if json.Unmarshal([]byte(strings.TrimSpace(report)), &r) != nil || r.NumTotalTests == nil {
		return ""
	}
	skip := r.NumPendingTests + r.NumTodoTests
	// A file can fail with no failed test: it didn't load (a syntax error,
	// a missing import) or a hook outside the tests threw. It is counted on
	// its own.
	failedIn := func(i int) int {
		n := 0
		for _, a := range r.TestResults[i].AssertionResults {
			if a.Status == "failed" {
				n++
			}
		}
		return n
	}
	var broken int
	for i, f := range r.TestResults {
		if f.Status == "failed" && failedIn(i) == 0 {
			broken++
		}
	}
	var d writer
	if r.NumFailedTests == 0 && broken == 0 {
		d.head(fmt.Sprintf("PASS: %d tests passed%s (vitest)", r.NumPassedTests, skipped(skip)))
		return d.String()
	}
	h := fmt.Sprintf("FAIL: %d of %d tests failed%s", r.NumFailedTests, *r.NumTotalTests, skipped(skip))
	if broken > 0 {
		h += fmt.Sprintf(", %d test files failed outside their tests", broken)
	}
	d.head(h + " (vitest)")
	left := r.NumFailedTests
	for i, f := range r.TestResults {
		file := rel(f.Name)
		if failedIn(i) == 0 {
			if f.Status == "failed" {
				d.item(file+": "+firstLine(f.Message), stackLine(f.Message, f.Name)...)
			}
			continue
		}
		if !d.line(file) {
			break
		}
		for _, a := range f.AssertionResults {
			if a.Status != "failed" {
				continue
			}
			msg := ""
			if len(a.FailureMessages) > 0 {
				msg = a.FailureMessages[0]
			}
			s := "  " + a.FullName + "  "
			if a.Location != nil && a.Location.Line > 0 {
				s += file + ":" + strconv.Itoa(a.Location.Line) + ":"
			} else if l := lineIn(msg, f.Name); l != "" {
				s += file + ":" + l + ":"
			}
			if !d.item(s + " " + firstLine(msg)) {
				break
			}
			left--
		}
		if d.full {
			break
		}
	}
	if left > 0 && d.full {
		d.more(left, "failed tests")
	}
	return d.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(ansi.ReplaceAllString(s, ""))
	l, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(l)
}

// lineIn finds the line in file that a failure's stack names first.
func lineIn(msg, file string) string {
	if file == "" {
		return ""
	}
	m := regexp.MustCompile(regexp.QuoteMeta(file) + `:(\d+):\d+`).FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	return m[1]
}

// stackLine is the first stack line of a load failure that points into the
// repo, if any.
func stackLine(msg, file string) []string {
	for _, l := range strings.Split(msg, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "at ") && strings.Contains(l, "/work/") && !strings.Contains(l, "/node_modules/") {
			return []string{"    " + strings.ReplaceAll(l, workPrefix(file), "")}
		}
	}
	return nil
}

// workPrefix is /work/<repo>/ for a file in a box's checkout.
func workPrefix(file string) string {
	if r, ok := strings.CutPrefix(file, "/work/"); ok {
		if repo, _, ok := strings.Cut(r, "/"); ok {
			return "/work/" + repo + "/"
		}
	}
	return "\x00"
}
