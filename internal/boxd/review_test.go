package boxd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

// box cancel kills the wrapper and writes .exit up to 5s later: a job that
// looks died for one scan is still running, and ends cancelled.
func TestDiedIsLostOnlyOnceItLasts(t *testing.T) {
	c := &clock{t: t0.Add(time.Minute)}
	job := "w.20261124-180100-ab12"
	f := &fakeIncus{
		ins:  []Instance{inst("w", "Running", "small", t0)},
		cfg:  map[string]string{"user.agent-box.mode": "full"},
		jobs: map[string][]fakeJob{"w": {{name: job, state: "running", log: "x", meta: metaJSON}}},
	}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(context.Background())
	f.set(func() { f.jobs["w"][0].state = "died" })
	c.add(3 * time.Second)
	w.Poll(context.Background())
	if j := w.State().Jobs[0]; j.State != dash.JobRunning {
		t.Fatalf("lost at once: %+v", j)
	}
	f.set(func() { f.jobs["w"][0].state, f.jobs["w"][0].exit = "done", "130" })
	c.add(3 * time.Second)
	w.Poll(context.Background())
	if j := w.State().Jobs[0]; j.Result() != dash.ResultCancelled {
		t.Fatalf("cancel: %+v", j)
	}

	// A wrapper that really died is lost after diedAfter.
	f.set(func() {
		f.jobs["w"] = append(f.jobs["w"], fakeJob{name: "w.20261124-180200-cd34", state: "died", log: "y", meta: metaJSON})
	})
	for i := 0; i < 5; i++ {
		c.add(3 * time.Second)
		w.Poll(context.Background())
	}
	for _, j := range w.State().Jobs {
		if j.ID == "20261124-180200-cd34" && j.State != dash.JobLost {
			t.Fatalf("died job: %+v", j)
		}
	}
}

// A job box reports before boxd ever saw it is scanned then, so its result
// and .meta are kept.
func TestEndReportScansAJobNotSeenYet(t *testing.T) {
	c := &clock{t: t0.Add(time.Minute)}
	f := &fakeIncus{ins: []Instance{inst("tmp-1", "Running", "small", t0)}, cfg: map[string]string{"user.agent-box.mode": "full"}}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(context.Background())
	f.set(func() {
		f.jobs = map[string][]fakeJob{"tmp-1": {{name: "tmp-1.20261124-180100-ab12", state: "done", exit: "0", log: "ok\n", meta: metaJSON, endedAt: t0.Add(70 * time.Second).Unix()}}}
	})
	if !w.EndJob(context.Background(), dash.JobEndedBody{Box: "tmp-1", ID: "20261124-180100-ab12", Exit: 0, EndedAt: t0.Add(70 * time.Second)}) {
		// It was found by the scan already done; either way it is recorded.
		t.Log("the scan recorded the end first")
	}
	f.set(func() { f.ins = nil })
	w.Poll(context.Background())
	st := w.State()
	if len(st.Jobs) != 1 || st.Jobs[0].Result() != dash.ResultPass || st.Jobs[0].Reason == "" {
		t.Fatalf("jobs: %+v", st.Jobs)
	}
}

// A log whose last bytes are half a character still reaches eof.
func TestLogEndsInAPartialCharacter(t *testing.T) {
	c := &clock{t: t0.Add(time.Minute)}
	job := "w.20261124-180100-ab12"
	f := &fakeIncus{
		ins:  []Instance{inst("w", "Running", "small", t0)},
		cfg:  map[string]string{},
		jobs: map[string][]fakeJob{"w": {{name: job, state: "done", exit: "1", log: "ok\xc3", meta: metaJSON}}},
	}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(context.Background())
	srv := httptest.NewServer((&Server{W: w}).Handler())
	defer srv.Close()
	key := url.PathEscape(dash.JobKey("w", "20261124-180100-ab12", t0.Add(time.Minute)))
	resp, err := http.Get(srv.URL + "/api/jobs/" + key + "/log?offset=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var chunk dash.LogChunk
	json.NewDecoder(resp.Body).Decode(&chunk)
	if !chunk.EOF || chunk.Offset != 3 || !strings.HasPrefix(chunk.Data, "ok") {
		t.Fatalf("%d %+v", resp.StatusCode, chunk)
	}
}

func TestCappedKeepsTheStart(t *testing.T) {
	c := &capped{max: 4}
	c.Write([]byte("abc"))
	if n, _ := c.Write([]byte("defg")); n != 4 || c.String() != "abcd" {
		t.Fatalf("%d %q", n, c.String())
	}
}
