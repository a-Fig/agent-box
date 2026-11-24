package dash

import (
	"regexp"
	"strings"
)

// A kind says what a run was for. The table is docs/ui.md's, in one place;
// the first row with a pattern in the command wins, so `go vet ./... && go
// test ./...` is a go-test.
const (
	KindGoTest  = "go-test"
	KindGoBuild = "go-build"
	KindJSTest  = "js-test"
	KindPyTest  = "py-test"
	KindGradle  = "gradle"
	KindBuild   = "build"
	KindGPU     = "gpu"
	KindOther   = "other"
)

type kindRule struct {
	kind     string
	patterns []string
}

var kindTable = []kindRule{
	{KindGoTest, []string{"go test"}},
	{KindGoBuild, []string{"go build", "go vet"}},
	{KindJSTest, []string{"vitest", "jest", "npm test", "npm run test"}},
	{KindPyTest, []string{"pytest", "uv run pytest"}},
	{KindGradle, []string{"gradle", "./gradlew"}},
	{KindBuild, []string{"make", "cargo build", "npm run build"}},
}

// Kinds lists every kind, for filters and legends, in the table's order.
var Kinds = []string{KindGoTest, KindGoBuild, KindJSTest, KindPyTest, KindGradle, KindBuild, KindGPU, KindOther}

type compiledRule struct {
	kind string
	re   *regexp.Regexp
}

const boundary = "[\\s;&|()\"'`]"

var compiled = func() []compiledRule {
	var out []compiledRule
	for _, r := range kindTable {
		for _, p := range r.patterns {
			// A pattern matches whole words: `make` is not `makefile`, and
			// a space in it is any run of blanks.
			q := strings.ReplaceAll(regexp.QuoteMeta(p), " ", `\s+`)
			out = append(out, compiledRule{r.kind, regexp.MustCompile(`(?:^|` + boundary + `)` + q + `(?:$|` + boundary + `)`)})
		}
	}
	return out
}()

// Classify returns the kind of a command. A run on a GPU box is a gpu
// whatever it runs; a command that matches no pattern is other.
func Classify(command string, gpu bool) string {
	if gpu {
		return KindGPU
	}
	for _, r := range compiled {
		if r.re.MatchString(command) {
			return r.kind
		}
	}
	return KindOther
}
