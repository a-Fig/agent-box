package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

type fakeCloudJob struct {
	spec       cloudJobSpec
	bundle     []byte
	job        cloudJob
	log        []byte
	doneAt     time.Time
	fileGone   bool
	fileTar    []byte
	cancelCode int
}

type fakeRunnerV1 struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	wantHeaders   map[string]string
	sizes         []string
	maxConcurrent int
	paused        bool
	maxTimeoutSec int
	budget        cloudBudget
	mode          string
	jobs          map[string]*fakeCloudJob
	jobOrder      []string

	probeHits    int
	startHits    int
	startStatus  int
	startErrCode string
	startErrMsg  string
	startNetErr  error
	jobDelay     time.Duration
}

func newFakeRunnerV1(t *testing.T) *fakeRunnerV1 {
	t.Helper()
	f := &fakeRunnerV1{
		t:             t,
		wantHeaders:   map[string]string{"X-Runner-Key": "test-key", "X-Runner-Secret": "test-secret"},
		sizes:         []string{"small", "medium", "large", "gpu"},
		maxConcurrent: 4,
		maxTimeoutSec: 3600,
		mode:          "function",
		budget: cloudBudget{
			Month:      "2026-11",
			SpentUSD:   3.12,
			RunningUSD: 0.04,
			CapUSD:     25,
			CreditUSD:  30,
			WarnAt:     0.75,
		},
		jobs: make(map[string]*fakeCloudJob),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRunnerV1) endpoint() string {
	return f.srv.URL
}

func (f *fakeRunnerV1) writeTokenFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cloudTokenFile{Headers: f.wantHeaders})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *fakeRunnerV1) doHTTP(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	if req.Method == http.MethodPost && req.URL.Path == "/v1/jobs" && f.startNetErr != nil {
		err := f.startNetErr
		f.startHits++
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()
	return http.DefaultClient.Do(req)
}

func (f *fakeRunnerV1) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeRunnerV1) writeError(w http.ResponseWriter, status int, code, msg string) {
	f.writeJSON(w, status, cloudErrorEnvelope{Error: code, Message: msg})
}

func (f *fakeRunnerV1) runningCountLocked() int {
	n := 0
	for _, j := range f.jobs {
		if j.job.State == "queued" || j.job.State == "running" {
			n++
		}
	}
	return n
}

func (f *fakeRunnerV1) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	for k, want := range f.wantHeaders {
		if r.Header.Get(k) != want {
			f.mu.Unlock()
			f.writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid auth headers")
			return
		}
	}
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/probe":
		f.mu.Lock()
		f.probeHits++
		running := f.runningCountLocked()
		resp := struct {
			Protocol      int          `json:"protocol"`
			Sizes         []string     `json:"sizes"`
			Running       int          `json:"running"`
			MaxConcurrent int          `json:"maxConcurrent"`
			Paused        bool         `json:"paused"`
			MaxTimeoutSec int          `json:"maxTimeoutSec"`
			Budget        *cloudBudget `json:"budget"`
		}{
			Protocol:      1,
			Sizes:         slices.Clone(f.sizes),
			Running:       running,
			MaxConcurrent: f.maxConcurrent,
			Paused:        f.paused,
			MaxTimeoutSec: f.maxTimeoutSec,
			Budget:        &f.budget,
		}
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, resp)

	case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
		mr, err := r.MultipartReader()
		if err != nil {
			f.writeError(w, http.StatusBadRequest, "invalid", "expected multipart/form-data")
			return
		}
		var spec cloudJobSpec
		var gotSpec bool
		var bundle []byte
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				f.writeError(w, http.StatusBadRequest, "invalid", err.Error())
				return
			}
			switch part.FormName() {
			case "spec":
				if err := json.NewDecoder(part).Decode(&spec); err != nil {
					f.writeError(w, http.StatusBadRequest, "invalid", "bad spec JSON: "+err.Error())
					return
				}
				gotSpec = true
			case "bundle":
				bundle, err = io.ReadAll(part)
				if err != nil {
					f.writeError(w, http.StatusBadRequest, "invalid", err.Error())
					return
				}
			default:
				_, _ = io.Copy(io.Discard, part)
			}
		}

		f.mu.Lock()
		f.startHits++
		if f.startStatus != 0 {
			status, code, msg := f.startStatus, f.startErrCode, f.startErrMsg
			f.mu.Unlock()
			f.writeError(w, status, code, msg)
			return
		}
		if !gotSpec || !validJob.MatchString(spec.ID) || len(bundle) == 0 {
			f.mu.Unlock()
			f.writeError(w, http.StatusBadRequest, "invalid", "missing spec, valid id, or bundle")
			return
		}
		if f.maxTimeoutSec > 0 && spec.TimeoutSec > f.maxTimeoutSec {
			f.mu.Unlock()
			f.writeError(w, http.StatusBadRequest, "timeout_cap", "timeoutSec exceeds maxTimeoutSec")
			return
		}
		if existing, ok := f.jobs[spec.ID]; ok {
			j := existing.job
			f.mu.Unlock()
			f.writeJSON(w, http.StatusOK, j)
			return
		}
		if f.paused {
			f.mu.Unlock()
			f.writeError(w, http.StatusLocked, "paused", "Runner is paused; not accepting new jobs.")
			return
		}
		if f.budget.CapUSD >= 0 && f.budget.SpentUSD+f.budget.RunningUSD >= f.budget.CapUSD {
			msg := fmt.Sprintf("Monthly spend cap ($%.2f) reached.", f.budget.CapUSD)
			f.mu.Unlock()
			f.writeError(w, http.StatusPaymentRequired, "budget", msg)
			return
		}
		if f.runningCountLocked() >= f.maxConcurrent {
			msg := fmt.Sprintf("Runner is at maxConcurrent (%d/%d).", f.maxConcurrent, f.maxConcurrent)
			f.mu.Unlock()
			f.writeError(w, http.StatusConflict, "busy", msg)
			return
		}

		now := time.Now().UTC()
		var doneAt time.Time
		if f.jobDelay > 0 {
			doneAt = now.Add(f.jobDelay)
		}
		logBytes := []byte("cloud-ok: " + spec.ID + "\n")
		zero := 0
		fj := &fakeCloudJob{
			spec:   spec,
			bundle: bundle,
			log:    logBytes,
			doneAt: doneAt,
			job: cloudJob{
				ID:        spec.ID,
				State:     "running",
				Attempt:   1,
				StartedAt: &now,
				LogBytes:  int64(len(logBytes)),
				Tail:      string(logBytes),
				Mode:      f.mode,
				Cost:      &cloudJobCost{USD: 0.012, CPUSec: 4.0},
				Budget:    &f.budget,
			},
		}
		if f.jobDelay == 0 {
			ended := now
			fj.job.State = "done"
			fj.job.Exit = &zero
			fj.job.EndedAt = &ended
		}
		f.jobs[spec.ID] = fj
		f.jobOrder = append([]string{spec.ID}, f.jobOrder...)
		f.mu.Unlock()

		f.writeJSON(w, http.StatusCreated, map[string]string{"id": spec.ID, "state": "queued"})

	case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
		limit := 20
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				limit = n
			}
		}
		f.mu.Lock()
		jobs := make([]cloudJob, 0, min(limit, len(f.jobOrder)))
		for _, id := range f.jobOrder {
			if len(jobs) >= limit {
				break
			}
			jobs = append(jobs, f.jobs[id].job)
		}
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, cloudJobList{Jobs: jobs})

	case r.Method == http.MethodPost && r.URL.Path == "/v1/pause":
		var req cloudPauseState
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		f.mu.Lock()
		f.paused = req.Paused
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, cloudPauseState{Paused: req.Paused})

	case r.Method == http.MethodPost && r.URL.Path == "/v1/stop":
		f.mu.Lock()
		stopped := 0
		code := 137
		now := time.Now().UTC()
		for _, fj := range f.jobs {
			if fj.job.State == "queued" || fj.job.State == "running" {
				fj.job.State = "cancelled"
				fj.job.Exit = &code
				fj.job.EndedAt = &now
				stopped++
			}
		}
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, cloudStopResponse{Stopped: stopped})

	case strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
		rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
		id, tail, hasTail := strings.Cut(rest, "/")
		if !hasTail && r.Method == http.MethodGet {
			waitSec, _ := strconv.Atoi(r.URL.Query().Get("wait"))
			deadline := time.Now().Add(time.Duration(min(waitSec, 25)) * time.Second)
			for {
				f.mu.Lock()
				fj, ok := f.jobs[id]
				if !ok {
					f.mu.Unlock()
					f.writeError(w, http.StatusNotFound, "not_found", "unknown job "+id)
					return
				}
				if (fj.job.State == "running" || fj.job.State == "queued") && !fj.doneAt.IsZero() && !time.Now().Before(fj.doneAt) {
					zero := 0
					now := time.Now().UTC()
					fj.job.State = "done"
					fj.job.Exit = &zero
					fj.job.EndedAt = &now
				}
				j := fj.job
				f.mu.Unlock()
				if waitSec <= 0 || (j.State != "queued" && j.State != "running") || !time.Now().Before(deadline) {
					f.writeJSON(w, http.StatusOK, j)
					return
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if tail == "log" && r.Method == http.MethodGet {
			offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
			limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
			if limit <= 0 || limit > dash.MaxLogChunk {
				limit = dash.MaxLogChunk
			}
			f.mu.Lock()
			fj, ok := f.jobs[id]
			if !ok {
				f.mu.Unlock()
				f.writeError(w, http.StatusNotFound, "not_found", "unknown job "+id)
				return
			}
			raw := slices.Clone(fj.log)
			stopped := fj.job.State != "queued" && fj.job.State != "running"
			f.mu.Unlock()

			total := int64(len(raw))
			if offset < 0 {
				offset = 0
			}
			if offset > total {
				offset = total
			}
			end := min(total, offset+limit)
			slice := raw[offset:end]
			chunk := dash.LogChunk{
				Offset:   end,
				Data:     strings.ToValidUTF8(string(slice), ""),
				EOF:      end >= total,
				Stopped:  stopped,
				LogBytes: total,
			}
			if chunk.Data != string(slice) {
				chunk.DataBase64 = base64.StdEncoding.EncodeToString(slice)
			}
			f.writeJSON(w, http.StatusOK, chunk)
			return
		}
		if tail == "cancel" && r.Method == http.MethodPost {
			f.mu.Lock()
			fj, ok := f.jobs[id]
			if !ok {
				f.mu.Unlock()
				f.writeError(w, http.StatusNotFound, "not_found", "unknown job "+id)
				return
			}
			cancelled := fj.job.State == "queued" || fj.job.State == "running"
			code := 137
			if fj.job.Exit != nil {
				code = *fj.job.Exit
			}
			if cancelled {
				now := time.Now().UTC()
				fj.job.State = "cancelled"
				fj.job.Exit = &code
				fj.job.EndedAt = &now
			}
			f.mu.Unlock()
			f.writeJSON(w, http.StatusOK, cloudCancelResponse{Cancelled: cancelled, Exit: code})
			return
		}
		if tail == "file" && r.Method == http.MethodGet {
			f.mu.Lock()
			fj, ok := f.jobs[id]
			if !ok {
				f.mu.Unlock()
				f.writeError(w, http.StatusNotFound, "not_found", "unknown job "+id)
				return
			}
			if fj.fileGone || f.mode == "function" {
				f.mu.Unlock()
				f.writeError(w, http.StatusConflict, "gone", "Sandbox is no longer kept; files are unavailable.")
				return
			}
			tarData := slices.Clone(fj.fileTar)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/x-tar")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tarData)
			return
		}
		http.NotFound(w, r)

	default:
		http.NotFound(w, r)
	}
}

func readRunnerFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "protocol", "runner-v1", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func TestRunnerV1GoldenFixturesDecode(t *testing.T) {
	var probe cloudProbeResponse
	if err := json.Unmarshal(readRunnerFixture(t, "probe.json"), &probe); err != nil || probe.Protocol != 1 || len(probe.Sizes) != 4 {
		t.Fatalf("probe.json decode = %+v, %v", probe, err)
	}
	var spec cloudJobSpec
	if err := json.Unmarshal(readRunnerFixture(t, "job-spec.json"), &spec); err != nil || spec.ID != "lantern.20261127-150405-a1b2" || !spec.Source.Valid() {
		t.Fatalf("job-spec.json decode = %+v, %v", spec, err)
	}
	var queued cloudJob
	if err := json.Unmarshal(readRunnerFixture(t, "job-queued.json"), &queued); err != nil || queued.State != "queued" {
		t.Fatalf("job-queued.json decode = %+v, %v", queued, err)
	}
	var done cloudJob
	if err := json.Unmarshal(readRunnerFixture(t, "job-done.json"), &done); err != nil || done.State != "done" || done.Exit == nil || *done.Exit != 0 || done.Cost == nil {
		t.Fatalf("job-done.json decode = %+v, %v", done, err)
	}
	var list cloudJobList
	if err := json.Unmarshal(readRunnerFixture(t, "job-list.json"), &list); err != nil || len(list.Jobs) != 2 {
		t.Fatalf("job-list.json decode = %+v, %v", list, err)
	}
	var chunk dash.LogChunk
	if err := json.Unmarshal(readRunnerFixture(t, "log-chunk.json"), &chunk); err != nil || !chunk.EOF || !chunk.Stopped || chunk.LogBytes != 1842 {
		t.Fatalf("log-chunk.json decode = %+v, %v", chunk, err)
	}
	var cancel cloudCancelResponse
	if err := json.Unmarshal(readRunnerFixture(t, "cancel.json"), &cancel); err != nil || !cancel.Cancelled || cancel.Exit != 137 {
		t.Fatalf("cancel.json decode = %+v, %v", cancel, err)
	}
	var pause cloudPauseState
	if err := json.Unmarshal(readRunnerFixture(t, "pause.json"), &pause); err != nil || !pause.Paused {
		t.Fatalf("pause.json decode = %+v, %v", pause, err)
	}
	var stop cloudStopResponse
	if err := json.Unmarshal(readRunnerFixture(t, "stop.json"), &stop); err != nil || stop.Stopped != 2 {
		t.Fatalf("stop.json decode = %+v, %v", stop, err)
	}
	for _, name := range []string{"error-budget.json", "error-busy.json", "error-gone.json", "error-paused.json"} {
		var env cloudErrorEnvelope
		if err := json.Unmarshal(readRunnerFixture(t, name), &env); err != nil || env.Error == "" || env.Message == "" {
			t.Fatalf("%s decode = %+v, %v", name, env, err)
		}
	}
}

func TestCloudConfiguredHostsValidationAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		wantErr string
		check   func(t *testing.T, hosts []fleetHost, configDir string)
	}{
		{
			name: "cloud defaults priority -1, mode function, resolves relative token_file",
			data: `{"hosts":[{"name":"cloud","kind":"modal","endpoint":"https://runner.example.com","token_file":"tokens/modal.json"}]}`,
			check: func(t *testing.T, hosts []fleetHost, configDir string) {
				want := fleetHost{
					Name:      "cloud",
					Kind:      "modal",
					Endpoint:  "https://runner.example.com",
					TokenFile: filepath.Join(configDir, "tokens", "modal.json"),
					Mode:      "function",
					Priority:  -1,
				}
				if len(hosts) != 1 || hosts[0] != want {
					t.Fatalf("hosts = %+v, want [%+v]", hosts, want)
				}
			},
		},
		{
			name: "explicit priority 0 preserved for cloud host and sandbox mode allowed",
			data: `{"hosts":[{"name":"cloud","kind":"modal","endpoint":"https://runner.example.com","token_file":"/abs/tok.json","mode":"sandbox","priority":0}]}`,
			check: func(t *testing.T, hosts []fleetHost, _ string) {
				if len(hosts) != 1 || hosts[0].Priority != 0 || hosts[0].Mode != "sandbox" {
					t.Fatalf("hosts = %+v, want priority 0 and mode sandbox", hosts)
				}
			},
		},
		{
			name: "loopback http endpoints allowed for tests",
			data: `{"hosts":[
				{"name":"c1","kind":"modal","endpoint":"http://127.0.0.1:8080","token_file":"a.json"},
				{"name":"c2","kind":"modal","endpoint":"http://localhost:8081","token_file":"b.json"},
				{"name":"c3","kind":"modal","endpoint":"http://[::1]:8082","token_file":"c.json"}
			]}`,
			check: func(t *testing.T, hosts []fleetHost, _ string) {
				if len(hosts) != 3 {
					t.Fatalf("len(hosts) = %d, want 3", len(hosts))
				}
			},
		},
		{name: "unknown kind", data: `{"hosts":[{"name":"c","kind":"aws","endpoint":"https://runner.example.com","token_file":"t.json"}]}`, wantErr: `unknown kind "aws"`},
		{name: "non-https non-loopback endpoint", data: `{"hosts":[{"name":"c","kind":"modal","endpoint":"http://runner.example.com","token_file":"t.json"}]}`, wantErr: "endpoint"},
		{name: "endpoint with credentials", data: `{"hosts":[{"name":"c","kind":"modal","endpoint":"https://user:pw@runner.example.com","token_file":"t.json"}]}`, wantErr: "endpoint"},
		{name: "endpoint with query", data: `{"hosts":[{"name":"c","kind":"modal","endpoint":"https://runner.example.com?x=1","token_file":"t.json"}]}`, wantErr: "endpoint"},
		{name: "endpoint with fragment", data: `{"hosts":[{"name":"c","kind":"modal","endpoint":"https://runner.example.com#x","token_file":"t.json"}]}`, wantErr: "endpoint"},
		{name: "cloud host with remote", data: `{"hosts":[{"name":"c","kind":"modal","remote":"pc","endpoint":"https://runner.example.com","token_file":"t.json"}]}`, wantErr: "cannot set remote or project"},
		{name: "cloud host with project", data: `{"hosts":[{"name":"c","kind":"modal","project":"agents","endpoint":"https://runner.example.com","token_file":"t.json"}]}`, wantErr: "cannot set remote or project"},
		{name: "missing token_file", data: `{"hosts":[{"name":"c","kind":"modal","endpoint":"https://runner.example.com"}]}`, wantErr: "requires token_file"},
		{name: "invalid mode", data: `{"hosts":[{"name":"c","kind":"modal","endpoint":"https://runner.example.com","token_file":"t.json","mode":"vm"}]}`, wantErr: "cloud mode"},
		{name: "conflicting alias priorities", data: `{"hosts":[{"name":"c1","kind":"modal","endpoint":"https://runner.example.com","token_file":"t.json","priority":0},{"name":"c2","kind":"modal","endpoint":"https://runner.example.com/","token_file":"t.json"}]}`, wantErr: "conflicting priorities"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := hostsTestPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			a := &App{Project: mode.Project}
			hosts, err := a.configuredHosts()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("configuredHosts() = %+v, %v; want error containing %q", hosts, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("configuredHosts() error = %v", err)
			}
			tc.check(t, hosts, filepath.Dir(path))
		})
	}
}

func TestCloudIdentityTagStabilityAndPropagation(t *testing.T) {
	c1 := fleetHost{Name: "cloud-a", Kind: "modal", Endpoint: "https://runner.example.com", TokenFile: "/tmp/a.json", Mode: "function", Priority: -1}
	c2 := fleetHost{Name: "cloud-b", Kind: "modal", Endpoint: "https://runner.example.com/", TokenFile: "/tmp/a.json", Mode: "function", Priority: -1}
	cOther := fleetHost{Name: "cloud-other", Kind: "modal", Endpoint: "https://other.example.com", TokenFile: "/tmp/b.json", Mode: "sandbox", Priority: -1}

	id1 := requireQualifiedJobID(t, c1, testBareJobID)
	id2 := requireQualifiedJobID(t, c2, testBareJobID)
	idOther := requireQualifiedJobID(t, cOther, testBareJobID)
	if id1.ID != id2.ID {
		t.Fatalf("trailing slash changed cloud tag: %s vs %s", id1.ID, id2.ID)
	}
	if id1.ID == idOther.ID {
		t.Fatalf("distinct cloud endpoints collided: %s", id1.ID)
	}

	// Incus tag must stay byte-for-byte identical to existing S1 expectation.
	incus := fleetHost{Name: "West", Remote: "west", Project: "agents"}
	if got := requireQualifiedJobID(t, incus, testBareJobID).ID; got != "h1-230811a1fc37a254d30a5e16d2edf57b~"+testBareJobID {
		t.Fatalf("Incus tag changed: %s", got)
	}

	// resolveFleetTargets deduplicates aliases by tag, keeps distinct cloud endpoints separate, and propagates fields.
	targets, bypass, err := resolveFleetTargets(fleetHost{Name: "pc", Remote: "pc", Project: "agents"}, []fleetHost{c1, c2, cOther}, "", "")
	if err != nil || bypass || len(targets) != 2 || targets[0] != c1 || targets[1] != cOther {
		t.Fatalf("resolveFleetTargets = %+v, bypass=%v, err=%v", targets, bypass, err)
	}

	// locateJobHost -> watchHost round-trip preserves all cloud fields.
	loc := locateJobHost(cOther)
	gotWatch, err := (jobRecord{Host: loc}).watchHost(idOther)
	if err != nil || gotWatch != cOther {
		t.Fatalf("watchHost round-trip = %+v, %v; want %+v", gotWatch, err, cOther)
	}
}

func TestCloudProbeMappingAndPick(t *testing.T) {
	srv := newFakeRunnerV1(t)
	tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")
	host := fleetHost{Name: "cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1}

	a := newApp()
	a.HTTP = srv.doHTTP
	ch, err := a.newCloudHost(host)
	if err != nil {
		t.Fatal(err)
	}

	// Healthy probe: full mode, GPU available, Fits = maxConcurrent - running.
	cand := ch.probe(context.Background())
	if !cand.Reachable || cand.Error != "" || cand.Mode != "full" || cand.BoxdAvailable || !cand.HasGPU || cand.Unavailable != "" {
		t.Fatalf("healthy probe = %+v", cand)
	}
	if !sizeFits(cand.Sizes, "medium") || cand.Sizes[0].Fits != 4 {
		t.Fatalf("sizes = %+v, want Fits=4", cand.Sizes)
	}

	// Paused.
	srv.mu.Lock()
	srv.paused = true
	srv.mu.Unlock()
	cand = ch.probe(context.Background())
	if cand.Unavailable != "cloud paused" {
		t.Fatalf("paused Unavailable = %q", cand.Unavailable)
	}
	if pick := pickFleetHost([]fleetCandidate{cand}, pickRequest{}); pick.Action != pickRefuse || !strings.Contains(pick.Reason, "cloud paused") {
		t.Fatalf("paused pick = %+v", pick)
	}

	// Budget cap reached.
	srv.mu.Lock()
	srv.paused = false
	srv.budget.SpentUSD = 24.98
	srv.budget.RunningUSD = 0.05
	srv.budget.CapUSD = 25.00
	srv.mu.Unlock()
	cand = ch.probe(context.Background())
	if cand.Unavailable != "monthly cap reached ($25.03 of $25.00)" {
		t.Fatalf("budget Unavailable = %q", cand.Unavailable)
	}
	if pick := pickFleetHost([]fleetCandidate{cand}, pickRequest{}); pick.Action != pickRefuse || !strings.Contains(pick.Reason, "monthly cap reached") {
		t.Fatalf("budget pick = %+v", pick)
	}

	// Busy (running >= maxConcurrent).
	srv.mu.Lock()
	srv.budget.SpentUSD = 1.0
	srv.budget.RunningUSD = 0.0
	srv.jobs["repo.20261127-120000-0001"] = &fakeCloudJob{job: cloudJob{ID: "repo.20261127-120000-0001", State: "running"}}
	srv.jobs["repo.20261127-120000-0002"] = &fakeCloudJob{job: cloudJob{ID: "repo.20261127-120000-0002", State: "running"}}
	srv.jobs["repo.20261127-120000-0003"] = &fakeCloudJob{job: cloudJob{ID: "repo.20261127-120000-0003", State: "running"}}
	srv.jobs["repo.20261127-120000-0004"] = &fakeCloudJob{job: cloudJob{ID: "repo.20261127-120000-0004", State: "running"}}
	srv.mu.Unlock()
	cand = ch.probe(context.Background())
	if cand.Unavailable != "cloud busy (4/4 running)" {
		t.Fatalf("busy Unavailable = %q", cand.Unavailable)
	}
	if pick := pickFleetHost([]fleetCandidate{cand}, pickRequest{}); pick.Action != pickRefuse || !strings.Contains(pick.Reason, "cloud busy (4/4 running)") {
		t.Fatalf("busy pick = %+v", pick)
	}

	// Missing budget (nil) and explicit capUSD <= 0 should not falsely claim cap reached.
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "missing budget", body: `{"protocol":1,"sizes":["medium"],"running":0,"maxConcurrent":2,"paused":false}`},
		{name: "zero capUSD", body: `{"protocol":1,"sizes":["medium"],"running":0,"maxConcurrent":2,"paused":false,"budget":{"month":"2026-11","spentUSD":0,"runningUSD":0,"capUSD":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noBudget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer noBudget.Close()
			c := &cloudHost{
				host:    fleetHost{Name: "cloud", Kind: "modal", Endpoint: noBudget.URL, TokenFile: tok, Priority: -1},
				do:      cloudHTTPClient.Do,
				headers: map[string]string{},
			}
			got := c.probe(context.Background())
			if !got.Reachable || got.Error != "" || got.Unavailable != "" {
				t.Fatalf("%s probe = %+v, want reachable and available", tc.name, got)
			}
		})
	}
}

func cloudDispatchTestApp(t *testing.T, ff *fakeFleet, hosts []fleetHost, httpDo func(*http.Request) (*http.Response, error)) (*App, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	path := hostsTestPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	rawHosts := make([]rawFleetHost, len(hosts))
	for i, h := range hosts {
		p := h.Priority
		rawHosts[i] = rawFleetHost{
			Name:         h.Name,
			Remote:       h.Remote,
			Project:      h.Project,
			DashboardURL: h.DashboardURL,
			Priority:     &p,
			Kind:         h.Kind,
			Endpoint:     h.Endpoint,
			TokenFile:    h.TokenFile,
			Mode:         h.Mode,
		}
	}
	raw, err := json.Marshal(map[string][]rawFleetHost{"hosts": rawHosts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "lantern")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "a.txt"), "hi\n")
	gitT(t, dir, "add", "a.txt")
	gitT(t, dir, "commit", "-q", "-m", "init")
	out := new(bytes.Buffer)
	errOut := new(bytes.Buffer)
	a := newApp()
	a.Remote, a.Project = "pc", mode.Project
	a.Out, a.Err = out, errOut
	a.StateDir = t.TempDir()
	a.Push, a.Post = nil, nil
	a.PausePoll = time.Hour
	a.WatchPoll = time.Millisecond
	a.R = ff
	a.HTTP = httpDo
	a.Now = func() time.Time { return time.Date(2026, 11, 27, 15, 4, 5, 0, time.UTC) }
	a.Rand = func() string { return "a1b2" }
	return a, out, errOut, dir
}

func TestCloudStartTurnAwayAndErrorSemantics(t *testing.T) {
	ctx := context.Background()

	t.Run("402, 409, and 423 turn away and fall back to next cloud host", func(t *testing.T) {
		for _, tc := range []struct {
			status int
			code   string
			msg    string
		}{
			{http.StatusPaymentRequired, "budget", "Monthly spend cap ($25.00) reached."},
			{http.StatusConflict, "busy", "Runner is at maxConcurrent (4/4)."},
			{http.StatusLocked, "paused", "Runner is paused; not accepting new jobs."},
		} {
			t.Run(tc.code, func(t *testing.T) {
				primary := newFakeRunnerV1(t)
				backup := newFakeRunnerV1(t)
				primary.startStatus, primary.startErrCode, primary.startErrMsg = tc.status, tc.code, tc.msg

				tmp := t.TempDir()
				hosts := []fleetHost{
					{Name: "primary-cloud", Kind: "modal", Endpoint: primary.endpoint(), TokenFile: primary.writeTokenFile(t, tmp, "p.json"), Mode: "function", Priority: 0},
					{Name: "backup-cloud", Kind: "modal", Endpoint: backup.endpoint(), TokenFile: backup.writeTokenFile(t, tmp, "b.json"), Mode: "function", Priority: -1},
				}
				ff := newFakeFleet(t)
				a, out, _, dir := cloudDispatchTestApp(t, ff, hosts, func(req *http.Request) (*http.Response, error) {
					if strings.HasPrefix(req.URL.String(), primary.endpoint()) {
						return primary.doHTTP(req)
					}
					return backup.doHTTP(req)
				})
				r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"go", "test", "./..."}, Reason: "Turn away fallback test."})
				if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "backup-cloud" || !strings.Contains(out.String(), "cloud-ok:") {
					t.Fatalf("Run = %+v, err=%v, out=%q", r, err, out.String())
				}
				if primary.startHits != 1 || backup.startHits != 1 {
					t.Fatalf("startHits = primary:%d backup:%d, want 1, 1", primary.startHits, backup.startHits)
				}
			})
		}
	})

	t.Run("400 returns Code 2 without turning away", func(t *testing.T) {
		primary := newFakeRunnerV1(t)
		backup := newFakeRunnerV1(t)
		primary.startStatus, primary.startErrCode, primary.startErrMsg = http.StatusBadRequest, "timeout_cap", "timeout exceeds cap"

		tmp := t.TempDir()
		hosts := []fleetHost{
			{Name: "primary-cloud", Kind: "modal", Endpoint: primary.endpoint(), TokenFile: primary.writeTokenFile(t, tmp, "p.json"), Mode: "function", Priority: 0},
			{Name: "backup-cloud", Kind: "modal", Endpoint: backup.endpoint(), TokenFile: backup.writeTokenFile(t, tmp, "b.json"), Mode: "function", Priority: -1},
		}
		a, _, _, dir := cloudDispatchTestApp(t, newFakeFleet(t), hosts, primary.doHTTP)
		r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"go", "test"}, Reason: "Bad request test."})
		if err == nil || r.Code != 2 || backup.probeHits != 0 || backup.startHits != 0 {
			t.Fatalf("Run = %+v, err=%v, backup probe=%d start=%d", r, err, backup.probeHits, backup.startHits)
		}
	})

	t.Run("500 and network error return unconfirmed error without retrying another host", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			netErr error
		}{
			{name: "500", status: http.StatusInternalServerError},
			{name: "network", netErr: io.ErrUnexpectedEOF},
		} {
			t.Run(tc.name, func(t *testing.T) {
				primary := newFakeRunnerV1(t)
				backup := newFakeRunnerV1(t)
				primary.startStatus = tc.status
				primary.startErrMsg = "internal failure"
				primary.startNetErr = tc.netErr

				tmp := t.TempDir()
				hosts := []fleetHost{
					{Name: "primary-cloud", Kind: "modal", Endpoint: primary.endpoint(), TokenFile: primary.writeTokenFile(t, tmp, "p.json"), Mode: "function", Priority: 0},
					{Name: "backup-cloud", Kind: "modal", Endpoint: backup.endpoint(), TokenFile: backup.writeTokenFile(t, tmp, "b.json"), Mode: "function", Priority: -1},
				}
				a, _, _, dir := cloudDispatchTestApp(t, newFakeFleet(t), hosts, func(req *http.Request) (*http.Response, error) {
					if strings.HasPrefix(req.URL.String(), primary.endpoint()) {
						return primary.doHTTP(req)
					}
					return backup.doHTTP(req)
				})
				r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"go", "test"}, Reason: "Unconfirmed submit test."})
				if err == nil || !strings.Contains(err.Error(), "submission could not be confirmed; check box jobs") || backup.startHits != 0 || backup.probeHits != 0 {
					t.Fatalf("Run = %+v, err=%v, backup probe=%d start=%d", r, err, backup.probeHits, backup.startHits)
				}
			})
		}
	})
}

func TestCloudInlineFollowAndLookupPaging(t *testing.T) {
	srv := newFakeRunnerV1(t)
	srv.jobDelay = 120 * time.Millisecond
	tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")
	hosts := []fleetHost{
		{Name: "cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1},
	}
	a, out, _, dir := cloudDispatchTestApp(t, newFakeFleet(t), hosts, srv.doHTTP)

	// Short inline wait detaches cleanly.
	r, err := a.Run(context.Background(), RunOpts{
		Dir:    dir,
		Cmd:    []string{"go", "test", "./..."},
		Wait:   30 * time.Millisecond,
		Reason: "Inline detach and follow test.",
	})
	if err != nil || r.State != stateDetached || r.Code != exitDetached || r.Host != "cloud" {
		t.Fatalf("detached Run = %+v, err=%v", r, err)
	}

	// Reattach via lookupJob (box job <qualified id>).
	lookup, err := a.lookupJob(r.Job)
	if err != nil {
		t.Fatal(err)
	}
	if _, queued, qerr := lookup.queued(context.Background()); queued || qerr != nil {
		t.Fatalf("cloud queued() = %v, %v, want false, nil", queued, qerr)
	}

	// Verify multipart spec sent on POST /v1/jobs matches runner-v1 schema.
	srv.mu.Lock()
	sentSpec := srv.jobs[lookup.id.Bare].spec
	sentBundle := srv.jobs[lookup.id.Bare].bundle
	srv.mu.Unlock()
	if sentSpec.ID != lookup.id.Bare || sentSpec.Repo != "lantern" || sentSpec.Ref == "" || len(sentSpec.Cmd) != 3 ||
		sentSpec.Size != "medium" || sentSpec.Reason == "" || sentSpec.Source == nil || !sentSpec.Source.Valid() || len(sentBundle) == 0 {
		t.Fatalf("sent spec = %+v, bundleLen=%d", sentSpec, len(sentBundle))
	}

	// Simulate streaming chunks with invalid UTF-8 split / raw bytes.
	srv.mu.Lock()
	fj := srv.jobs[lookup.id.Bare]
	fj.log = []byte("line-1\xffline-2\n")
	fj.job.LogBytes = int64(len(fj.log))
	srv.mu.Unlock()

	out.Reset()
	res, err := lookup.job(context.Background(), 0)
	if err != nil || res.State != stateDone || res.Code != 0 || res.Host != "cloud" || out.String() != "line-1\xffline-2\n" {
		t.Fatalf("lookup.job = %+v, err=%v, out=%q", res, err, out.String())
	}

	// Log paging via lookup.page.
	p1, end, err := lookup.page(context.Background(), 0, 6)
	if err != nil || end != nil || p1.Text != "line-1" || p1.NextOffset != 6 || !p1.More || p1.Host != "cloud" || p1.Job != r.Job {
		t.Fatalf("page 1 = %+v, end=%+v, err=%v", p1, end, err)
	}
	p2, end, err := lookup.page(context.Background(), p1.NextOffset, 64)
	if err != nil || end != nil || p2.Text != "\xffline-2\n" || p2.More || p2.LogBytes != int64(len("line-1\xffline-2\n")) {
		t.Fatalf("page 2 = %+v, end=%+v, err=%v", p2, end, err)
	}

	// Also verify remaining runner-v1 client methods directly against fake server.
	ch, err := a.newCloudHost(hosts[0])
	if err != nil {
		t.Fatal(err)
	}
	listed, err := ch.list(context.Background(), 10)
	if err != nil || len(listed) != 1 || listed[0].ID != lookup.id.Bare {
		t.Fatalf("list = %+v, err=%v", listed, err)
	}
	if _, err := ch.pull(context.Background(), lookup.id.Bare, "coverage"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("function-mode pull err = %v, want gone/unavailable", err)
	}
	paused, err := ch.pause(context.Background(), true)
	if err != nil || !paused {
		t.Fatalf("pause = %v, %v", paused, err)
	}
	stopped, err := ch.stop(context.Background())
	if err != nil || stopped != 0 {
		t.Fatalf("stop = %d, %v", stopped, err)
	}
}

func TestCloudRefusesExplicitBoxFlag(t *testing.T) {
	srv := newFakeRunnerV1(t)
	tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")
	hosts := []fleetHost{
		{Name: "pc", Remote: "pc", Project: mode.Project},
		{Name: "cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1},
	}
	a, _, _, dir := cloudDispatchTestApp(t, newFakeFleet(t), hosts, srv.doHTTP)
	r, err := a.Run(context.Background(), RunOpts{
		Dir:    dir,
		Host:   "cloud",
		Box:    "mybox",
		Cmd:    []string{"echo", "hi"},
		Reason: "Verify --box is refused on cloud host.",
	})
	if err == nil || r.Code != 2 || err.Error() != "--box needs an Incus host" {
		t.Fatalf("Run = %+v, err=%v, want Code 2 and --box refusal", r, err)
	}
	if srv.probeHits != 0 || srv.startHits != 0 {
		t.Fatalf("cloud server was contacted (%d probes, %d starts) despite --box refusal", srv.probeHits, srv.startHits)
	}
}

func TestMixedPCAndCloudFleetTieringAndSpillover(t *testing.T) {
	ctx := context.Background()

	t.Run("PC wins and cloud receives 0 probe requests", func(t *testing.T) {
		srv := newFakeRunnerV1(t)
		tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")
		pc := &fakeFleetHost{remote: "pc", project: mode.Project, mode: "full", queueCapable: true, execStdout: "pc-ok\n"}
		ff := newFakeFleet(t, pc)
		ff.dashHosts["https://pc.example.com"] = pc
		hosts := []fleetHost{
			{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"},
			{Name: "cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1},
		}
		a, out, _, dir := cloudDispatchTestApp(t, ff, hosts, func(req *http.Request) (*http.Response, error) {
			if strings.HasPrefix(req.URL.String(), srv.endpoint()) {
				return srv.doHTTP(req)
			}
			return ff.httpDo(req)
		})
		r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"go", "test"}, Reason: "PC preferred over cloud overflow.", Queue: true})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "pc" || !strings.Contains(out.String(), "pc-ok") {
			t.Fatalf("Run = %+v, err=%v, out=%q", r, err, out.String())
		}
		if srv.probeHits != 0 || srv.startHits != 0 {
			t.Fatalf("cloud received %d probes and %d starts; want 0 when PC can start", srv.probeHits, srv.startHits)
		}
	})

	t.Run("spills over to cloud when PC is unavailable or full", func(t *testing.T) {
		srv := newFakeRunnerV1(t)
		tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")
		pc := &fakeFleetHost{
			remote:       "pc",
			project:      mode.Project,
			mode:         "mypc",
			queueCapable: true,
		}
		ff := newFakeFleet(t, pc)
		ff.dashHosts["https://pc.example.com"] = pc
		hosts := []fleetHost{
			{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"},
			{Name: "cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1},
		}
		a, out, _, dir := cloudDispatchTestApp(t, ff, hosts, func(req *http.Request) (*http.Response, error) {
			if strings.HasPrefix(req.URL.String(), srv.endpoint()) {
				return srv.doHTTP(req)
			}
			return ff.httpDo(req)
		})
		r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"go", "test"}, Reason: "Spill over to cloud when PC is in My PC mode.", Queue: true})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "cloud" || !strings.Contains(out.String(), "cloud-ok:") {
			t.Fatalf("Run = %+v, err=%v, out=%q", r, err, out.String())
		}
		if srv.probeHits != 1 || srv.startHits != 1 {
			t.Fatalf("cloud hits = probe:%d start:%d, want 1, 1", srv.probeHits, srv.startHits)
		}
	})
}

func TestCloudRedirectRefusedWithoutForwardingHeaders(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/leak", http.StatusFound)
	}))
	defer redirector.Close()

	tokPath := filepath.Join(t.TempDir(), "modal.json")
	if err := os.WriteFile(tokPath, []byte(`{"headers":{"X-Runner-Secret":"top-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host := fleetHost{Name: "cloud", Kind: "modal", Endpoint: redirector.URL, TokenFile: tokPath, Mode: "sandbox", Priority: -1}

	a := newApp()
	a.Out, a.Err = io.Discard, io.Discard
	// a.HTTP is intentionally nil so newCloudHost uses cloudHTTPClient.
	ch, err := a.newCloudHost(host)
	if err != nil {
		t.Fatal(err)
	}

	if cand := ch.probe(context.Background()); cand.Reachable || !strings.Contains(cand.Error, "redirect") {
		t.Fatalf("probe on 302 = %+v, want unreachable redirect error", cand)
	}
	if _, err := ch.pull(context.Background(), "lantern.20261127-150405-a1b2", "out.txt"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("pull on 302 err = %v, want redirect error", err)
	}

	dir := filepath.Join(t.TempDir(), "lantern")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "a.txt"), "hi\n")
	gitT(t, dir, "add", "a.txt")
	gitT(t, dir, "commit", "-q", "-m", "init")
	snap, err := snapshot(context.Background(), execRunner{}, dir, "20261127-150405-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snap.bundle))

	prep := &fleetRun{repo: repo{Top: dir, Name: "lantern"}, ref: "20261127-150405-a1b2", snap: snap}
	if _, away, err := ch.start(context.Background(), RunOpts{Cmd: []string{"echo"}, Reason: "Redirect safety test."}, pickTestSize(t, "medium"), prep); err == nil || away != nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("start on 302 = away:%v err:%v, want redirect error", away, err)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target received %d requests; auth headers leaked across redirect!", targetHits)
	}
}

func TestCloudCancelAndPull(t *testing.T) {
	srv := newFakeRunnerV1(t)
	srv.jobDelay = 2 * time.Second
	srv.mode = "sandbox"
	tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")

	fnHost := fleetHost{Name: "fn-cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1}
	hosts := []fleetHost{fnHost}
	a, _, _, dir := cloudDispatchTestApp(t, newFakeFleet(t), hosts, srv.doHTTP)

	// Start a slow job and cancel it via CancelAny.
	r, err := a.Run(context.Background(), RunOpts{
		Dir:    dir,
		Cmd:    []string{"sleep", "10"},
		Wait:   20 * time.Millisecond,
		Reason: "Test CancelAny on cloud job ID.",
	})
	if err != nil || r.State != stateDetached {
		t.Fatalf("Run = %+v, err=%v", r, err)
	}
	removed, cancelled, code, err := a.CancelAny(context.Background(), r.Job)
	if err != nil || removed || !cancelled || code != 137 {
		t.Fatalf("CancelAny = removed:%v cancelled:%v code:%d err:%v", removed, cancelled, code, err)
	}

	// Function-mode pull refuses cleanly without hitting Incus or runner.
	if _, err := a.Pull(context.Background(), r.Job, "coverage", t.TempDir()); err == nil || !strings.Contains(err.Error(), "function-mode cloud jobs keep no files") {
		t.Fatalf("function-mode Pull err = %v", err)
	}

	// Switch hosts.json to sandbox mode and verify Pull extracts tar safely (dropping symlinks).
	sbHost := fleetHost{Name: "fn-cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "sandbox", Priority: -1}
	a, _, _, _ = cloudDispatchTestApp(t, newFakeFleet(t), []fleetHost{sbHost}, srv.doHTTP)
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("report-ok\n")
	if err := tw.WriteHeader(&tar.Header{Name: "coverage/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "coverage/out.txt", Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "coverage/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	lookup, err := a.lookupJob(r.Job)
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.jobs[lookup.id.Bare].fileTar = tarBuf.Bytes()
	srv.mu.Unlock()

	localDir := t.TempDir()
	dest, err := a.Pull(context.Background(), r.Job, "coverage", localDir)
	if err != nil {
		t.Fatalf("sandbox Pull err = %v", err)
	}
	gotBytes, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	if err != nil || string(gotBytes) != "report-ok\n" {
		t.Fatalf("pulled file = %q, err=%v", gotBytes, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink was not dropped: %v", err)
	}

	// 409 gone from runner surfaces runner's message.
	srv.mu.Lock()
	srv.jobs[lookup.id.Bare].fileGone = true
	srv.mu.Unlock()
	if _, err := a.Pull(context.Background(), r.Job, "coverage", t.TempDir()); err == nil || !strings.Contains(err.Error(), "Sandbox is no longer kept") {
		t.Fatalf("gone Pull err = %v", err)
	}
}

func TestCloudMCPBoxRunLongJobSetsPushFalse(t *testing.T) {
	srv := newFakeRunnerV1(t)
	srv.jobDelay = 150 * time.Millisecond
	tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")
	hosts := []fleetHost{
		{Name: "cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1},
	}
	a, _, _, dir := cloudDispatchTestApp(t, newFakeFleet(t), hosts, srv.doHTTP)
	a.Thread = "t3-thread-1"
	a.Push = &fakePusher{}
	var spawned int
	a.Spawn = func(string, string) error {
		spawned++
		return nil
	}
	a.InlineWait = 25 * time.Millisecond

	params, err := json.Marshal(map[string]any{
		"name": "box_run",
		"arguments": map[string]any{
			"dir":     dir,
			"command": "go test ./...",
			"reason":  "Verify long cloud MCP run reports push false without spawning watcher.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(context.Background(), a, params, nil)
	rawSC, err := json.Marshal(res["structuredContent"])
	if err != nil {
		t.Fatal(err)
	}
	var sc map[string]any
	if err := json.Unmarshal(rawSC, &sc); err != nil {
		t.Fatal(err)
	}
	if res["isError"] != false || sc["state"] != stateRunning || sc["push"] != false || sc["exitCode"] != nil || spawned != 0 {
		t.Fatalf("box_run >InlineWait = %+v, spawned=%d", res, spawned)
	}
	jobID, _ := sc["job"].(string)
	if jobID == "" {
		t.Fatalf("missing job id in %+v", res)
	}
	jobRes, jobData := lookupTool(t, a, jobID, false)
	if jobRes["isError"] != false || jobData["state"] != stateDone || jobData["exitCode"] != 0.0 {
		t.Fatalf("box_job follow-up = %+v", jobRes)
	}
}

func TestCloudStartContextCancelledMidUploadPreservesJobID(t *testing.T) {
	srv := newFakeRunnerV1(t)
	tok := srv.writeTokenFile(t, t.TempDir(), "modal.json")
	host := fleetHost{Name: "cloud", Kind: "modal", Endpoint: srv.endpoint(), TokenFile: tok, Mode: "function", Priority: -1}
	a, _, _, dir := cloudDispatchTestApp(t, newFakeFleet(t), []fleetHost{host}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	}
	ch, err := a.newCloudHost(host)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot(context.Background(), execRunner{}, dir, "20261127-150405-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snap.bundle))

	wantID := requireQualifiedJobID(t, host, "lantern.20261127-150405-a1b2").ID
	prep := &fleetRun{repo: repo{Top: dir, Name: "lantern"}, ref: "20261127-150405-a1b2", snap: snap}
	r, away, err := ch.start(ctx, RunOpts{Cmd: []string{"echo"}, Reason: "Mid-upload cancel preserves job ID."}, pickTestSize(t, "medium"), prep)
	if !errors.Is(err, context.Canceled) || away != nil || r.Job != wantID {
		t.Fatalf("start on ctx cancel = r:%+v away:%v err:%v (want Job=%s)", r, away, err, wantID)
	}
}
