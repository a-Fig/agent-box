package dash

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		cmd  string
		gpu  bool
		want string
	}{
		{"go test ./...", false, KindGoTest},
		{"go  test -race ./internal/mode", false, KindGoTest},
		{"cd cmd/box && go test ./...", false, KindGoTest},
		{"go vet ./... && go test ./...", false, KindGoTest},
		{"go build ./...", false, KindGoBuild},
		{"go vet ./...", false, KindGoBuild},
		{"npx vitest run --maxWorkers=2", false, KindJSTest},
		{"npm test", false, KindJSTest},
		{"npm run test:unit", false, KindOther},
		{"npm run test -- --ci", false, KindJSTest},
		{"pytest -q", false, KindPyTest},
		{"uv run pytest tests/", false, KindPyTest},
		{"./gradlew :vi:testDebugUnitTest", false, KindGradle},
		{"gradle build", false, KindGradle},
		{"make -j4", false, KindBuild},
		{"cargo build --release", false, KindBuild},
		{"npm run build", false, KindBuild},
		{"bash -lc 'make test'", false, KindBuild},
		{"cat Makefile", false, KindOther},
		{"echo hello", false, KindOther},
		{"", false, KindOther},
		{"python train.py", true, KindGPU},
		{"go test ./...", true, KindGPU},
	}
	for _, c := range cases {
		if got := Classify(c.cmd, c.gpu); got != c.want {
			t.Errorf("Classify(%q, %v) = %q, want %q", c.cmd, c.gpu, got, c.want)
		}
	}
}

func TestKindsCoverTable(t *testing.T) {
	in := map[string]bool{}
	for _, k := range Kinds {
		in[k] = true
	}
	for _, r := range kindTable {
		if !in[r.kind] {
			t.Errorf("kind %q is in the table but not Kinds", r.kind)
		}
	}
}

func TestResult(t *testing.T) {
	z, f, c := 0, 1, 130
	cases := []struct {
		j    Job
		want string
	}{
		{Job{State: JobRunning}, ""},
		{Job{State: JobPaused}, ""},
		{Job{State: JobDone, Exit: &z}, ResultPass},
		{Job{State: JobDone, Exit: &f}, ResultFail},
		{Job{State: JobDone, Exit: &c}, ResultCancelled},
		{Job{State: JobLost}, ResultLost},
	}
	for _, c := range cases {
		if got := c.j.Result(); got != c.want {
			t.Errorf("%+v: result %q, want %q", c.j, got, c.want)
		}
	}
}
