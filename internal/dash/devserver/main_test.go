package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/dash/fixture"
)

var t0 = time.Date(2026, 11, 24, 17, 30, 0, 0, time.UTC)

func start(t *testing.T, scenario string) (*server, *httptest.Server) {
	t.Helper()
	sc, err := fixture.Build(scenario, t0)
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(sc, func() time.Time { return t0 })
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func call(t *testing.T, ts *httptest.Server, method, path, body string, control bool) (int, dash.ErrorBody, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if control {
		req.Header.Set(dash.ControlHeader, "1")
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var raw strings.Builder
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		raw.WriteString(sc.Text())
	}
	var e dash.ErrorBody
	json.Unmarshal([]byte(raw.String()), &e)
	return res.StatusCode, e, []byte(raw.String())
}

func key(j dash.Job) string { return url.PathEscape(j.Key) }

func TestPageAndState(t *testing.T) {
	_, ts := start(t, "busy")
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Errorf("page: %d, CSP %q", res.StatusCode, res.Header.Get("Content-Security-Policy"))
	}
	code, _, body := call(t, ts, "GET", "/api/state", "", false)
	var st dash.State
	if code != 200 || json.Unmarshal(body, &st) != nil || len(st.Boxes) != 3 {
		t.Errorf("state: %d, %d boxes", code, len(st.Boxes))
	}
}

// The controls answer as boxd will.
func TestControls(t *testing.T) {
	s, ts := start(t, "mypc")
	paused := s.sc.State.Jobs[0]
	lanternMade := s.sc.State.Boxes[s.findBox("lantern")].CreatedAt.Format(time.RFC3339)
	if paused.State != dash.JobPaused {
		t.Fatalf("first job is %s", paused.State)
	}
	cases := []struct {
		name, method, path, body string
		control                  bool
		status                   int
		code                     string
	}{
		{"no header", "POST", "/api/mode", `{"mode":"full"}`, false, 403, dash.CodeForbidden},
		{"bad mode", "POST", "/api/mode", `{"mode":"turbo"}`, true, 400, dash.CodeBadRequest},
		{"mode request", "POST", "/api/mode", `{"mode":"shared"}`, true, 202, ""},
		{"cancel while frozen", "POST", "/api/jobs/" + key(paused) + "/cancel", `{}`, true, 409, dash.CodeFrozen},
		{"cancel a finished job", "POST", "/api/jobs/" + key(s.done[0]) + "/cancel", `{}`, true, 409, dash.CodeNotRunning},
		{"cancel nothing", "POST", "/api/jobs/x%2Fy%2Fz/cancel", `{}`, true, 404, dash.CodeNotFound},
		{"extend by 2h", "POST", "/api/boxes/lantern/extend", `{"hours":2,"createdAt":"` + lanternMade + `"}`, true, 400, dash.CodeBadRequest},
		{"extend with no createdAt", "POST", "/api/boxes/lantern/extend", `{"hours":4}`, true, 400, dash.CodeBadRequest},
		{"extend a replaced box", "POST", "/api/boxes/lantern/extend", `{"hours":4,"createdAt":"2026-01-01T00:00:00Z"}`, true, 409, dash.CodeGone},
		{"extend by 4h", "POST", "/api/boxes/lantern/extend", `{"hours":4,"createdAt":"` + lanternMade + `"}`, true, 200, ""},
		{"delete nothing", "POST", "/api/boxes/nope/delete", `{"createdAt":"` + lanternMade + `"}`, true, 404, dash.CodeNotFound},
		{"delete a replaced box", "POST", "/api/boxes/lantern/delete", `{"createdAt":"2026-01-01T00:00:00Z"}`, true, 409, dash.CodeGone},
		{"log of a frozen box", "GET", "/api/jobs/" + key(paused) + "/log?offset=0", "", false, 200, ""},
		{"bad refusal", "POST", "/api/refusals", `{"why":"bored"}`, true, 400, dash.CodeBadRequest},
		{"refusal", "POST", "/api/refusals", `{"why":"mypc","reason":"Full suite before the PR.","size":"medium"}`, true, 204, ""},
		{"bad range", "GET", "/api/history?range=year", "", false, 400, dash.CodeBadRequest},
	}
	for _, c := range cases {
		status, e, _ := call(t, ts, c.method, c.path, c.body, c.control)
		if status != c.status || e.Code != c.code {
			t.Errorf("%s: %d %q, want %d %q", c.name, status, e.Code, c.status, c.code)
		}
	}
	if m := s.machine(); m.ModeRequested == nil || m.ModeRequested.Mode != dash.ModeShared || m.Mode != dash.ModeMyPC {
		t.Errorf("after asking for Shared: %+v", m)
	}
	if b := s.sc.State.Boxes[s.findBox("lantern")]; b.ExpiresAt.Sub(t0) != 23*time.Hour+10*time.Minute+4*time.Hour {
		t.Errorf("lantern expires %v", b.ExpiresAt)
	}
	_, _, body := call(t, ts, "GET", "/api/jobs/"+key(paused)+"/log", "", false)
	var c dash.LogChunk
	json.Unmarshal(body, &c)
	if !c.Paused || c.Data != "" {
		t.Errorf("a frozen box's log: %+v", c)
	}
}

func TestDeleteLosesItsJobs(t *testing.T) {
	s, ts := start(t, "busy")
	made := s.sc.State.Boxes[s.findBox("lantern")].CreatedAt.Format(time.RFC3339)
	if status, _, _ := call(t, ts, "POST", "/api/boxes/lantern/delete", `{"createdAt":"`+made+`"}`, true); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	if s.findBox("lantern") >= 0 {
		t.Error("lantern is still there")
	}
	for _, j := range s.sc.State.Jobs {
		if j.Box == "lantern" && j.State == dash.JobRunning {
			t.Error("its job still runs")
		}
	}
	if s.machine().Allocated.Instances != 2 {
		t.Errorf("allocated %+v", s.machine().Allocated)
	}
}

func TestLogPages(t *testing.T) {
	s, ts := start(t, "busy")
	j := s.sc.State.Jobs[0]
	s.logs[j.Key] = strings.Repeat("é", dash.MaxLogChunk) // two bytes each
	var off int64
	var got strings.Builder
	for range 10 {
		_, _, body := call(t, ts, "GET", "/api/jobs/"+key(j)+"/log?offset="+itoa(off), "", false)
		var c dash.LogChunk
		if err := json.Unmarshal(body, &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Data) > dash.MaxLogChunk {
			t.Fatalf("a page of %d bytes", len(c.Data))
		}
		got.WriteString(c.Data)
		off = c.Offset
		if c.EOF {
			break
		}
	}
	if got.String() != s.logs[j.Key] {
		t.Errorf("paged %d bytes of %d, or split a character", got.Len(), len(s.logs[j.Key]))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestJobsFilter(t *testing.T) {
	_, ts := start(t, "week")
	_, _, body := call(t, ts, "GET", "/api/jobs?q=RACE+SUITE&repo=lantern&limit=5", "", false)
	var p dash.JobsPage
	json.Unmarshal(body, &p)
	if len(p.Jobs) == 0 || len(p.Jobs) > 5 || !p.More {
		t.Fatalf("got %d jobs, more %v", len(p.Jobs), p.More)
	}
	for _, j := range p.Jobs {
		if !strings.Contains(strings.ToLower(j.Reason), "race suite") || !strings.HasSuffix(j.Repo, "/lantern") {
			t.Errorf("%q in %s doesn't match", j.Reason, j.Repo)
		}
	}
	last := p.Jobs[len(p.Jobs)-1].Key
	_, _, body = call(t, ts, "GET", "/api/jobs?q=race+suite&repo=lantern&before="+url.QueryEscape(last), "", false)
	var older dash.JobsPage
	json.Unmarshal(body, &older)
	for _, j := range older.Jobs {
		if !j.StartedAt.Before(p.Jobs[len(p.Jobs)-1].StartedAt) {
			t.Errorf("before: %v isn't older", j.StartedAt)
		}
	}
}

// The stream starts with the state, then sends what changes.
func TestEvents(t *testing.T) {
	s, ts := start(t, "busy")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	r := bufio.NewReader(res.Body)
	next := func() string {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "event: ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
		}
	}
	if e := next(); e != dash.EventState {
		t.Fatalf("first event %q", e)
	}
	s.mu.Lock()
	s.step()
	s.mu.Unlock()
	if e := next(); e != dash.EventJob && e != dash.EventMachine && e != dash.EventBox {
		t.Errorf("then %q", e)
	}
}

// Paging goes by (startedAt, key), so jobs that started in the same second all come back.
func TestJobsPageSameSecond(t *testing.T) {
	s, ts := start(t, "empty")
	at := t0.Add(-time.Hour)
	for _, box := range []string{"a", "b", "c"} {
		s.done = append(s.done, dash.Job{Key: dash.JobKey(box, "1", at), Box: box, ID: "1", StartedAt: at, State: dash.JobDone})
	}
	slices.SortStableFunc(s.done, newest)
	var seen []string
	before := ""
	for range 5 {
		_, _, body := call(t, ts, "GET", "/api/jobs?limit=1&before="+url.QueryEscape(before), "", false)
		var p dash.JobsPage
		json.Unmarshal(body, &p)
		if len(p.Jobs) == 0 {
			break
		}
		seen = append(seen, p.Jobs[0].Box)
		before = p.Jobs[0].Key
	}
	if strings.Join(seen, "") != "cba" {
		t.Errorf("paged %v, want c, b, a", seen)
	}
}

// One job by key; and a job whose box name a newer box took has no log there.
func TestJobAndReusedBox(t *testing.T) {
	s, ts := start(t, "busy")
	var old dash.Job
	for _, j := range s.done {
		if s.findBox(j.Box) < 0 {
			old = j
			break
		}
	}
	if code, _, _ := call(t, ts, "GET", "/api/jobs/"+key(old), "", false); code != 200 {
		t.Errorf("GET a finished job: %d", code)
	}
	if code, _, _ := call(t, ts, "GET", "/api/jobs/x%2F1%2F2026-01-01T00:00:00Z", "", false); code != 404 {
		t.Errorf("GET no job: %d", code)
	}
	// Made seconds after the job started, as box run --new does: still the job's box.
	s.sc.State.Boxes = append(s.sc.State.Boxes, dash.Box{Name: old.Box, Machine: "pc", Size: "small", State: dash.BoxRunning, CreatedAt: old.StartedAt.Add(5 * time.Second)})
	s.logs[old.Key] = "this job's\n"
	if code, _, _ := call(t, ts, "GET", "/api/jobs/"+key(old)+"/log", "", false); code != 200 {
		t.Errorf("log on a box made 5s after the job: %d", code)
	}
	// Made past the grace: a reuse of the name.
	s.sc.State.Boxes[len(s.sc.State.Boxes)-1].CreatedAt = old.StartedAt.Add(dash.JobBoxGrace + time.Second)
	s.logs[old.Key] = "not this job's\n"
	if code, e, _ := call(t, ts, "GET", "/api/jobs/"+key(old)+"/log", "", false); code != 404 || e.Code != dash.CodeNotFound {
		t.Errorf("log on a reused box name: %d %q", code, e.Code)
	}
}

// The search folds case one way, the way the page does: strings.ToLower, field by field.
func TestSearchFolding(t *testing.T) {
	cases := []struct {
		q, field string
		want     bool
	}{
		{"RACE", "Full -race suite", true},
		{"istanbul", "Deploy to İstanbul", true}, // U+0130's simple lower case is i
		{"İST", "istanbul", true},
		{"straße", "STRASSE", false}, // no full case folding
		{"suite\nmain", "suite", false},
	}
	for _, c := range cases {
		if got := matchesText(c.q, c.field); got != c.want {
			t.Errorf("matchesText(%q, %q) = %v, want %v", c.q, c.field, got, c.want)
		}
	}
	// Per field: a query can't match across the reason and the command.
	if matchesText("suite go", "Full suite", "go test") {
		t.Error("matched across fields")
	}
}

// box reports a run's end before deleting its throwaway, so the job ends rather than goes lost.
func TestJobEnded(t *testing.T) {
	s, ts := start(t, "busy")
	var run dash.Job
	for _, j := range s.sc.State.Jobs {
		if j.State == dash.JobRunning {
			run = j
			break
		}
	}
	body := func(id string) string {
		b, _ := json.Marshal(dash.JobEndedBody{Box: run.Box, ID: id, Exit: 1, EndedAt: t0, Tail: "FAIL\n"})
		return string(b)
	}
	if code, e, _ := call(t, ts, "POST", "/api/jobs/ended", body("nope"), true); code != 404 || e.Code != dash.CodeNotFound {
		t.Errorf("unknown job: %d %q", code, e.Code)
	}
	if code, _, _ := call(t, ts, "POST", "/api/jobs/ended", body(run.ID), true); code != 204 {
		t.Fatalf("ended: %d", code)
	}
	j, _ := s.findJob(run.Key)
	if j.State != dash.JobDone || j.Exit == nil || *j.Exit != 1 || !j.EndedAt.Equal(t0) || j.Tail != "FAIL\n" {
		t.Errorf("after ended: %+v", *j)
	}
	if code, _, _ := call(t, ts, "POST", "/api/jobs/ended", body(run.ID), true); code != 404 {
		t.Errorf("a second report for an ended job: %d", code)
	}
}

func TestQueue(t *testing.T) {
	s, ts := start(t, "full")
	type entry struct {
		name string
		pos  int
	}
	line := func() []entry {
		var out []entry
		for _, j := range s.queue() {
			out = append(out, entry{j.Box + "." + j.ID, j.Position})
		}
		return out
	}
	names := func(es []entry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.name)
		}
		return out
	}
	q := names(line())
	if len(q) != 3 {
		t.Fatalf("full's queue is %q", q)
	}
	waitingKey := s.queue()[0].Key
	cases := []struct {
		name, path string
		control    bool
		status     int
		code       string
	}{
		{"no header", "/api/queue/" + q[2] + "/front", false, 403, dash.CodeForbidden},
		{"front", "/api/queue/" + q[2] + "/front", true, 200, ""},
		{"cancel", "/api/queue/" + q[1] + "/cancel", true, 204, ""},
		{"cancel again", "/api/queue/" + q[1] + "/cancel", true, 409, dash.CodeNotRunning},
		{"cancel one never queued", "/api/queue/nope.1/cancel", true, 409, dash.CodeNotRunning},
		{"cancel a waiting job as a running one", "/api/jobs/" + url.PathEscape(waitingKey) + "/cancel", true, 409, dash.CodeNotRunning},
	}
	for _, c := range cases {
		status, e, _ := call(t, ts, "POST", c.path, `{}`, c.control)
		if status != c.status || e.Code != c.code {
			t.Errorf("%s: %d %q, want %d %q", c.name, status, e.Code, c.status, c.code)
		}
	}
	if got := line(); !slices.Equal(got, []entry{{q[2], 1}, {q[0], 2}}) {
		t.Fatalf("line is %v, want %s then %s", got, q[2], q[0])
	}

	// Room comes: the first job in line that fits starts with its ID, box and QueuedAt under a
	// running key; its waiting key goes, and the one left moves up, each sent as an event.
	events := make(chan dash.Event, 64)
	s.mu.Lock()
	s.subs[events] = true
	gpu := slices.IndexFunc(s.sc.State.Boxes, func(b dash.Box) bool { return b.GPU })
	s.removeBox(s.sc.State.Boxes[gpu].Name)
	front := *s.queue()[0]
	for len(events) > 0 {
		<-events
	}
	s.stepQueue()
	delete(s.subs, events)
	s.mu.Unlock()
	close(events)
	if got := line(); !slices.Equal(got, []entry{{q[0], 1}}) {
		t.Fatalf("after room came the line is %v", got)
	}
	var gone []string
	sent := map[string]dash.Job{}
	for e := range events {
		switch d := e.Data.(type) {
		case dash.JobGone:
			gone = append(gone, d.Key)
		case dash.Job:
			if d.Key == "" {
				t.Errorf("a job event with no job: %+v", d)
			}
			sent[d.Key] = d
		}
	}
	if !slices.Equal(gone, []string{front.Key}) {
		t.Errorf("job-gone for %q, want the started job's waiting key %q", gone, front.Key)
	}
	var started *dash.Job
	for k, j := range sent {
		if j.ID == front.ID && j.State == dash.JobRunning {
			started = &j
			if k == front.Key || j.Box != front.Box || !j.QueuedAt.Equal(*front.QueuedAt) || j.Position != 0 || j.Why != "" {
				t.Errorf("started: %+v", j)
			}
		}
	}
	if started == nil || s.findBox(started.Box) < 0 {
		t.Fatalf("no running job for %s among %v", front.ID, sent)
	}
	if left := s.queue()[0]; sent[left.Key].Position != 1 {
		t.Errorf("the job left in line wasn't sent at its new place: %+v", sent[left.Key])
	}
	if status, e, _ := call(t, ts, "POST", "/api/queue/"+front.Box+"."+front.ID+"/front", `{}`, true); status != 409 || e.Code != dash.CodeNotRunning {
		t.Errorf("front for a job that has started: %d %q", status, e.Code)
	}
}
