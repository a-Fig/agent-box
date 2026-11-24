package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	s, ts := start(t, "yielding")
	paused := s.sc.State.Jobs[0]
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
		{"mode waits for the game", "POST", "/api/mode", `{"mode":"shared"}`, true, 202, ""},
		{"cancel while yielding", "POST", "/api/jobs/" + key(paused) + "/cancel", `{}`, true, 409, dash.CodeYielding},
		{"cancel a finished job", "POST", "/api/jobs/" + key(s.done[0]) + "/cancel", `{}`, true, 409, dash.CodeNotRunning},
		{"cancel nothing", "POST", "/api/jobs/x%2Fy%2Fz/cancel", `{}`, true, 404, dash.CodeNotFound},
		{"extend by 2h", "POST", "/api/boxes/lantern/extend", `{"hours":2}`, true, 400, dash.CodeBadRequest},
		{"extend by 4h", "POST", "/api/boxes/lantern/extend", `{"hours":4}`, true, 200, ""},
		{"delete nothing", "POST", "/api/boxes/nope/delete", `{}`, true, 404, dash.CodeNotFound},
		{"log of a frozen box", "GET", "/api/jobs/" + key(paused) + "/log?offset=0", "", false, 200, ""},
		{"bad refusal", "POST", "/api/refusals", `{"why":"bored"}`, true, 400, dash.CodeBadRequest},
		{"refusal", "POST", "/api/refusals", `{"why":"yielding","reason":"Full suite before the PR.","size":"medium"}`, true, 204, ""},
		{"bad range", "GET", "/api/history?range=year", "", false, 400, dash.CodeBadRequest},
	}
	for _, c := range cases {
		status, e, _ := call(t, ts, c.method, c.path, c.body, c.control)
		if status != c.status || e.Code != c.code {
			t.Errorf("%s: %d %q, want %d %q", c.name, status, e.Code, c.status, c.code)
		}
	}
	if m := s.machine(); m.ModeRequested == nil || m.ModeRequested.Mode != dash.ModeShared || m.Mode != dash.ModeMyPC {
		t.Errorf("after asking for Shared mid-game: %+v", m)
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
	if status, _, _ := call(t, ts, "POST", "/api/boxes/lantern/delete", `{}`, true); status != 204 {
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
	last := p.Jobs[len(p.Jobs)-1].StartedAt.Format(time.RFC3339)
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
